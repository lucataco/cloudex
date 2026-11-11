package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ProbeStreams is for the release checks (cloudex-node probe stream): it
// connects a control socket as the node, asks for one stream ticket for a
// sandbox, and opens a stream with it four ways: as issued (101), the same
// again (a replay), with the ticket altered, and to another stream id. It
// returns the four HTTP statuses. Run it while the agent is stopped: its
// connection replaces the agent's.
func ProbeStreams(ctx context.Context, api, nodeID string, key ed25519.PrivateKey, version, sandbox string) ([]int, error) {
	full, signed, err := Endpoint(api, "/v1/node/connect")
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(HeaderProtocol, strconv.Itoa(Protocol))
	Signer{Key: key, Node: nodeID}.Sign(h, http.MethodGet, signed, "")
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, full, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "probe")
	write := func(v any) error {
		b, _ := json.Marshal(v)
		return c.Write(dctx, websocket.MessageText, b)
	}
	if err := write(NewHello(version, HostFacts(false), nil)); err != nil {
		return nil, err
	}
	if err := write(TicketRequest{Type: "tickets", ID: "tk-probe", Sandbox: sandbox, Count: 1}); err != nil {
		return nil, err
	}
	var t Tickets
	for t.ID != "tk-probe" {
		_, b, err := c.Read(dctx)
		if err != nil {
			return nil, fmt.Errorf("waiting for the ticket: %w", err)
		}
		json.Unmarshal(b, &t)
	}
	if len(t.Tickets) != 1 {
		return nil, fmt.Errorf("no ticket: %s", t.Error)
	}
	tk, err := readTicket(t.Tickets[0])
	if err != nil {
		return nil, err
	}
	open := func(stream, ticket string) int {
		u, _, _ := Endpoint(api, "/v1/node/streams/"+sandbox+"/"+stream)
		h := http.Header{}
		h.Set(HeaderTicket, ticket)
		octx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		sc, res, err := websocket.Dial(octx, u, &websocket.DialOptions{HTTPHeader: h})
		if sc != nil {
			sc.Close(websocket.StatusNormalClosure, "probe")
			return http.StatusSwitchingProtocols
		}
		if res != nil {
			return res.StatusCode
		}
		_ = err
		return 0
	}
	altered := tk.raw
	if i := strings.LastIndexByte(altered, '.'); i > 0 && i+1 < len(altered) {
		b := []byte(altered)
		if b[i+1] == 'A' {
			b[i+1] = 'B'
		} else {
			b[i+1] = 'A'
		}
		altered = string(b)
	}
	other := "0000000000000000"
	if tk.stream == other {
		other = "1111111111111111"
	}
	return []int{open(tk.stream, tk.raw), open(tk.stream, tk.raw), open(tk.stream, altered), open(other, tk.raw)}, nil
}
