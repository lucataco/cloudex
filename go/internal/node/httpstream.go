package node

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Hop-by-hop headers, which a proxy doesn't pass on.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

// Headers the WebSocket client sets itself.
var wsOwn = map[string]bool{
	"sec-websocket-key": true, "sec-websocket-version": true, "sec-websocket-extensions": true,
	"sec-websocket-protocol": true, "sec-websocket-accept": true,
}

// PreviewMessageMax is the largest WebSocket message a preview may send.
const PreviewMessageMax = 1 << 20

var previewTransport = &http.Transport{
	DialContext:        (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	DisableCompression: true,
	MaxIdleConns:       64,
	IdleConnTimeout:    30 * time.Second,
}

// http sends one HTTP request to a port of the machine (a preview) and
// streams the answer back: {"status"}, then the body on channel 1. A
// WebSocket upgrade is answered 101 and its messages are relayed.
func (h *Handler) http(s *Stream, m Target, r StreamHTTP) {
	upgrade := false
	for _, kv := range r.Headers {
		if strings.EqualFold(kv[0], "upgrade") && strings.EqualFold(strings.TrimSpace(kv[1]), "websocket") {
			upgrade = true
		}
	}
	if upgrade {
		h.websocket(s, m, r)
		return
	}
	pr, pw := io.Pipe()
	var body io.Reader = http.NoBody
	if r.Body {
		body = pr
	}
	req, err := http.NewRequestWithContext(s.ctx, r.Method, "http://"+net.JoinHostPort(m.Address, strconv.Itoa(r.Port))+r.Path, body)
	if err != nil {
		h.fail(s, err.Error())
		return
	}
	for _, kv := range r.Headers {
		k := strings.ToLower(kv[0])
		switch {
		case k == "host":
			req.Host = kv[1]
		case k == "content-length":
			req.ContentLength, _ = strconv.ParseInt(kv[1], 10, 64)
		case hopByHop[k]:
		default:
			req.Header.Add(kv[0], kv[1])
		}
	}
	if !r.Body {
		req.ContentLength = 0
	}
	pump := newStdinPump(s, pw, func() error { return pw.Close() })
	defer pump.stop()
	go func() {
		for {
			in, err := s.Read()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			switch {
			case in.Ctl == nil && in.Ch == ChStdin:
				pump.push(in.Data)
			case in.Ctl != nil && in.Ctl.EOF != nil:
				pump.eof()
			}
		}
	}()
	res, err := previewTransport.RoundTrip(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			s.Send(StreamMsg{Refused: strp(m.Address)})
			s.Close(1000, "refused")
			return
		}
		h.fail(s, err.Error())
		return
	}
	defer res.Body.Close()
	hs := [][2]string{}
	for k, vs := range res.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			hs = append(hs, [2]string{k, v})
		}
	}
	if res.ContentLength >= 0 && res.Header.Get("Content-Length") == "" && res.StatusCode != 204 && res.StatusCode != 304 {
		hs = append(hs, [2]string{"Content-Length", strconv.FormatInt(res.ContentLength, 10)})
	}
	if err := s.Send(StreamMsg{Status: &HTTPStatus{Code: res.StatusCode, Headers: hs}}); err != nil {
		return
	}
	buf := make([]byte, frameMax)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 && s.Data(ChStdout, buf[:n]) != nil {
			return
		}
		if err != nil {
			break
		}
	}
	s.Send(StreamMsg{EOF: intp(ChStdout)})
	s.Close(1000, "done")
}

// websocket relays a preview's WebSocket: messages, not bytes, so that the
// object can hand them to the visitor's socket as they are (Vite's hot reload).
func (h *Handler) websocket(s *Stream, m Target, r StreamHTTP) {
	hdr := http.Header{}
	var protocols []string
	host := ""
	for _, kv := range r.Headers {
		k := strings.ToLower(kv[0])
		switch {
		case k == "host":
			host = kv[1]
		case k == "sec-websocket-protocol":
			for _, p := range strings.Split(kv[1], ",") {
				if p = strings.TrimSpace(p); p != "" {
					protocols = append(protocols, p)
				}
			}
		case hopByHop[k] || wsOwn[k] || k == "content-length":
		default:
			hdr.Add(kv[0], kv[1])
		}
	}
	if host != "" {
		hdr.Set("Host", host)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	c, res, err := websocket.Dial(ctx, "ws://"+net.JoinHostPort(m.Address, strconv.Itoa(r.Port))+r.Path, &websocket.DialOptions{
		HTTPHeader: hdr, Subprotocols: protocols, HTTPClient: &http.Client{Transport: previewTransport},
	})
	cancel()
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			s.Send(StreamMsg{Refused: strp(m.Address)})
			s.Close(1000, "refused")
			return
		}
		// The app answered without upgrading: pass its status on.
		if res != nil {
			code := res.StatusCode
			if code < 100 || code > 599 {
				code = 502
			}
			s.Send(StreamMsg{Status: &HTTPStatus{Code: code, Headers: [][2]string{}}})
			if res.Body != nil {
				buf, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
				s.Data(ChStdout, buf)
			}
			s.Send(StreamMsg{EOF: intp(ChStdout)})
			s.Close(1000, "done")
			return
		}
		h.fail(s, err.Error())
		return
	}
	defer c.CloseNow()
	// A preview's message is read whole before it's passed on: a machine
	// could make each one 64 MiB. The Worker takes at most 1 MiB (review H4).
	c.SetReadLimit(PreviewMessageMax)
	hs := [][2]string{}
	if p := c.Subprotocol(); p != "" {
		hs = append(hs, [2]string{"sec-websocket-protocol", p})
	}
	if err := s.Send(StreamMsg{Status: &HTTPStatus{Code: 101, Headers: hs}}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			typ, b, err := c.Read(s.ctx)
			if err != nil {
				code := int(websocket.CloseStatus(err))
				if code <= 0 {
					code = 1000
				}
				s.Send(StreamMsg{Close: &WSClose{Code: code, Reason: ""}})
				return
			}
			ch := ChStdout
			if typ == websocket.MessageText {
				ch = ChTextOut
			}
			if s.Data(ch, b) != nil {
				return
			}
		}
	}()
	go func() {
		for {
			in, err := s.Read()
			if err != nil {
				c.Close(websocket.StatusGoingAway, "")
				return
			}
			switch {
			case in.Ctl == nil && (in.Ch == ChStdin || in.Ch == ChTextIn):
				typ := websocket.MessageBinary
				if in.Ch == ChTextIn {
					typ = websocket.MessageText
				}
				c.Write(s.ctx, typ, in.Data)
				s.Consumed(in.Ch, len(in.Data))
			case in.Ctl != nil && in.Ctl.Close != nil:
				code := in.Ctl.Close.Code
				if code < 1000 || code == 1005 || code == 1006 || code == 1015 {
					code = 1000
				}
				c.Close(websocket.StatusCode(code), in.Ctl.Close.Reason)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-s.Done():
	}
	s.Close(1000, "closed")
}
