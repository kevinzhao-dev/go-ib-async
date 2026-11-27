package mcpserver

import (
	"fmt"
	"strings"
	"testing"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
)

// refusedBroker is a broker whose connection attempts TWS refuses with
// error 326 because another API client holds the client id.
type refusedBroker struct{ *ibgo.IB }

func (refusedBroker) IsConnected() bool { return false }
func (refusedBroker) LastConnectError() error {
	return fmt.Errorf("connection closed during handshake: %w", &ibgo.HandshakeError{
		ClientID: 17, Code: 326, Message: "Unable to connect as the client id is already in use. Retry with a unique client id.",
	})
}

// connectingBroker has not finished its first connection attempt.
type connectingBroker struct{ *ibgo.IB }

func (connectingBroker) IsConnected() bool       { return false }
func (connectingBroker) LastConnectError() error { return nil }

func TestNotConnectedReportsWhy(t *testing.T) {
	h := newHarness(t, harnessOpts{offline: true, broker: func(ib *ibgo.IB) Broker { return refusedBroker{ib} }})
	st := h.ok("get_status", map[string]any{})
	why := fmt.Sprint(st["connection_error"])
	if st["connected"] != false || !strings.Contains(why, "error 326 for client id 17") || !strings.Contains(why, "-client-id (env IB_CLIENT_ID)") {
		t.Fatalf("status = %v", st)
	}
	if _, ok := st["client_id"]; ok {
		t.Fatalf("status reports a client id while not connected: %v", st)
	}
	msg := h.fail("get_account_summary", map[string]any{}, "not connected to TWS/IB Gateway: ")
	if !strings.Contains(msg, "error 326") || !strings.Contains(msg, "another agent session") {
		t.Fatalf("tool error = %q", msg)
	}

	// No failed attempt yet: no reason to report.
	c := newHarness(t, harnessOpts{offline: true, broker: func(ib *ibgo.IB) Broker { return connectingBroker{ib} }})
	if st := c.ok("get_status", map[string]any{}); st["connection_error"] != nil {
		t.Fatalf("status = %v", st)
	}
	c.fail("get_positions", map[string]any{}, "not connected to TWS/IB Gateway (the server keeps retrying")

	// Connected: no connection error either.
	if st := newHarness(t, harnessOpts{}).ok("get_status", map[string]any{}); st["connected"] != true || st["connection_error"] != nil {
		t.Fatalf("status = %v", st)
	}
}
