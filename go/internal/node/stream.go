package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Heartbeats on stream sockets: the agent sends HB every HBEvery, the
// sandbox's object answers HBOK by auto-response without waking, and a
// stream with no answer for HBDead is dead (the spikes' half-open sockets).
const (
	HB      = "cx-hb"
	HBOK    = "cx-hb-ok"
	HBEvery = 15 * time.Second
	HBDead  = 45 * time.Second
)

// frameMax is the most bytes of data one binary frame carries.
const frameMax = 256 << 10

var errStreamClosed = errors.New("the stream closed")

// window counts what a sender may still send on one channel.
type window struct {
	mu    sync.Mutex
	cond  *sync.Cond
	avail int
	dead  bool
}

func newWindow(n int) *window {
	w := &window{avail: n}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// take waits for at least one byte of credit and takes up to want.
func (w *window) take(want int) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.avail <= 0 && !w.dead {
		w.cond.Wait()
	}
	if w.dead {
		return 0, errStreamClosed
	}
	n := min(want, w.avail)
	w.avail -= n
	return n, nil
}

func (w *window) grant(n int) {
	w.mu.Lock()
	w.avail += n
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *window) kill() {
	w.mu.Lock()
	w.dead = true
	w.mu.Unlock()
	w.cond.Broadcast()
}

// Stream is one stream socket after its open: framed data with credits, the
// control messages, and the heartbeat.
type Stream struct {
	ID   string
	c    *websocket.Conn
	ctx  context.Context
	stop context.CancelFunc

	out    [5]*window // what we may send, per channel
	in     [5]atomic.Int64
	lastOK atomic.Int64 // ms: the object's last heartbeat answer
	raw    bool         // a splice: bytes with no channel byte and no credit
}

func newStream(ctx context.Context, id string, c *websocket.Conn) *Stream {
	sctx, stop := context.WithCancel(ctx)
	s := &Stream{ID: id, c: c, ctx: sctx, stop: stop}
	for i := range s.out {
		s.out[i] = newWindow(StreamWindow)
	}
	s.lastOK.Store(time.Now().UnixMilli())
	return s
}

// Done is closed when the stream is.
func (s *Stream) Done() <-chan struct{} { return s.ctx.Done() }

// Close ends the stream with a code, after what was sent has arrived: the
// closing handshake goes first, because cancelling the context of a pending
// Read tears the connection down at once, and frames still on their way are
// lost (the object sees 1006).
func (s *Stream) Close(code int, reason string) {
	if len(reason) > 120 {
		reason = reason[:120]
	}
	s.c.Close(websocket.StatusCode(code), reason)
	s.stop()
	for _, w := range s.out {
		w.kill()
	}
}

// Abort ends it without the closing handshake.
func (s *Stream) Abort() {
	s.stop()
	for _, w := range s.out {
		w.kill()
	}
	s.c.CloseNow()
}

// Send writes a control message.
func (s *Stream) Send(m StreamMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.c.Write(s.ctx, websocket.MessageText, b)
}

// Data sends p on a channel, within the channel's credit: it blocks until the
// object has granted room, so a slow reader slows the source down. In a
// splice it sends the bytes as they are.
func (s *Stream) Data(ch int, p []byte) error {
	if s.raw {
		for len(p) > 0 {
			n := min(len(p), frameMax)
			if err := s.c.Write(s.ctx, websocket.MessageBinary, p[:n]); err != nil {
				return err
			}
			p = p[n:]
		}
		return nil
	}
	for len(p) > 0 {
		n, err := s.out[ch].take(min(len(p), frameMax))
		if err != nil {
			return err
		}
		frame := make([]byte, n+1)
		frame[0] = byte(ch)
		copy(frame[1:], p[:n])
		if err := s.c.Write(s.ctx, websocket.MessageBinary, frame); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Consumed grants the object credit for n bytes of a channel that were used
// up (written to the machine), in steps of a quarter window.
func (s *Stream) Consumed(ch, n int) {
	if s.raw {
		return
	}
	if v := s.in[ch].Add(int64(n)); v >= StreamWindow/4 {
		s.in[ch].Add(-v)
		s.Send(StreamMsg{Credit: &Credit{Ch: ch, N: int(v)}})
	}
}

// heartbeat sends HB every HBEvery and ends the stream when the object has
// stopped answering.
func (s *Stream) heartbeat() {
	t := time.NewTicker(HBEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if time.Since(time.UnixMilli(s.lastOK.Load())) > HBDead {
				s.Close(StreamHeartbeat, "no heartbeat answer for 45 s")
				return
			}
			wctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			s.c.Write(wctx, websocket.MessageText, []byte(HB))
			cancel()
		}
	}
}

// Inbound is what a stream's reader hands its handler.
type Inbound struct {
	Ctl  *StreamMsg // a control message, or
	Ch   int        // data on a channel (Ch < 0 in a splice)
	Data []byte
}

// Read returns the next control message or data, skipping heartbeats.
func (s *Stream) Read() (Inbound, error) {
	for {
		typ, b, err := s.c.Read(s.ctx)
		if err != nil {
			return Inbound{}, err
		}
		if typ == websocket.MessageBinary {
			if s.raw {
				return Inbound{Ch: -1, Data: b}, nil
			}
			if len(b) < 1 || b[0] > ChTextOut {
				return Inbound{}, fmt.Errorf("a data frame on no channel")
			}
			return Inbound{Ch: int(b[0]), Data: b[1:]}, nil
		}
		if string(b) == HBOK {
			s.lastOK.Store(time.Now().UnixMilli())
			continue
		}
		var m StreamMsg
		if err := json.Unmarshal(b, &m); err != nil {
			return Inbound{}, fmt.Errorf("a control message that isn't JSON: %.100s", b)
		}
		if m.Credit != nil {
			if m.Credit.Ch >= 0 && m.Credit.Ch < len(s.out) && m.Credit.N > 0 {
				s.out[m.Credit.Ch].grant(m.Credit.N)
			}
			continue
		}
		return Inbound{Ctl: &m}, nil
	}
}
