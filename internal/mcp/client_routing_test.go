package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

// mockTransport delivers queued server→client messages and, for a "test" call,
// auto-replies with a matching-id response (so the response is enqueued only
// after Call has registered its pending entry — avoiding a race).
type mockTransport struct {
	sends  chan json.RawMessage
	closed chan struct{}
}

func newMockTransport() *mockTransport {
	return &mockTransport{sends: make(chan json.RawMessage, 8), closed: make(chan struct{})}
}

func (m *mockTransport) Send(_ context.Context, msg any) error {
	data, _ := json.Marshal(msg)
	var req struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	_ = json.Unmarshal(data, &req)
	if req.Method == "test" {
		m.sends <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"ok"}`, req.ID))
	}
	return nil
}

func (m *mockTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg := <-m.sends:
		return msg, nil
	case <-m.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *mockTransport) Close() error {
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}

func TestClient_RoutesResponseAndNotification(t *testing.T) {
	mock := newMockTransport()
	c := NewClient(mock)
	go c.receiveLoop()
	defer c.Close()

	// Notification (method, no id) must land on Notifications(), not be treated
	// as a response.
	mock.sends <- json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/progress"}`)
	select {
	case n := <-c.Notifications():
		if n.Method != "notifications/progress" {
			t.Fatalf("notification method = %q", n.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not delivered")
	}

	// Response (id, no method) must resolve the pending Call.
	var out string
	if err := c.Call(context.Background(), "test", nil, &out); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "ok" {
		t.Fatalf("result = %q, want ok", out)
	}
}

// Closing the client while a Call is blocked must return an error, not panic on
// a nil *Response from the closed channel.
func TestClient_CloseUnblocksCallWithoutPanic(t *testing.T) {
	mock := newMockTransport()
	c := NewClient(mock)
	go c.receiveLoop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Call(context.Background(), "never-answered", nil, nil)
	}()
	time.Sleep(50 * time.Millisecond) // let Call register its pending entry
	c.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after Close")
	}
}
