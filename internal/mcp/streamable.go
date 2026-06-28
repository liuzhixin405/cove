package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

// streamableHTTPTransport implements the MCP Streamable HTTP transport
// (2025 spec). Unlike SSE which uses separate endpoints for session
// creation and message exchange, StreamableHTTP uses a single endpoint
// and relies on the HTTP response to stream server→client messages.
type streamableHTTPTransport struct {
	baseURL string
	client  *http.Client
	msgChan chan json.RawMessage
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	stream  io.ReadCloser // active streaming response body
}

// NewStreamableHTTPTransport connects to an MCP server using the
// Streamable HTTP transport. The endpoint should support both POST
// (for sending) and GET (for receiving via streaming).
func NewStreamableHTTPTransport(endpoint string) (*streamableHTTPTransport, error) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &streamableHTTPTransport{
		baseURL: strings.TrimRight(endpoint, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
		msgChan: make(chan json.RawMessage, 64),
		ctx:     ctx,
		cancel:  cancel,
	}

	// Open the streaming connection
	if err := t.openStream(); err != nil {
		cancel()
		return nil, fmt.Errorf("streamablehttp connect: %w", err)
	}

	return t, nil
}

// openStream establishes a long-lived GET connection for server→client messages.
func (t *streamableHTTPTransport) openStream() error {
	req, err := http.NewRequestWithContext(t.ctx, "GET", t.baseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// Use a client with no timeout for the streaming connection
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return fmt.Errorf("streamablehttp: server returned %d", resp.StatusCode)
	}

	t.mu.Lock()
	if t.stream != nil {
		t.stream.Close()
	}
	t.stream = resp.Body
	t.mu.Unlock()

	go t.readStream(resp.Body)
	return nil
}

// readStream continuously reads SSE events from the stream.
func (t *streamableHTTPTransport) readStream(body io.ReadCloser) {
	defer func() {
		t.mu.Lock()
		if t.stream == body {
			t.stream = nil
		}
		t.mu.Unlock()
		body.Close()
	}()

	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 256)
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}
		n, err := body.Read(chunk)
		if err != nil {
			if err != io.EOF {
				log.Warnf("streamablehttp: read error: %v", err)
			}
			return
		}
		buf = append(buf, chunk[:n]...)

		// Process complete SSE events (delimited by \n\n)
		for {
			idx := bytes.Index(buf, []byte("\n\n"))
			if idx == -1 {
				break
			}
			event := strings.TrimSpace(string(buf[:idx]))
			buf = buf[idx+2:]

			if strings.HasPrefix(event, "data: ") {
				payload := strings.TrimPrefix(event, "data: ")
				// Block until the message is consumed (backpressure) rather than
				// dropping it; dropping a JSON-RPC response would hang the caller.
				// Unblock on transport shutdown.
				select {
				case t.msgChan <- json.RawMessage(payload):
				case <-t.ctx.Done():
					return
				}
			}
		}
	}
}

// Send posts a JSON-RPC message to the server.
func (t *streamableHTTPTransport) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", t.baseURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return fmt.Errorf("streamablehttp: POST %d: %s", resp.StatusCode, string(body))
	}

	// If the response is itself a stream, hand the body to a reader goroutine
	// which owns and closes it. We must NOT close it here (a deferred close
	// would race the goroutine's reads and truncate the streamed response).
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		go t.readStreamFromResponse(resp.Body)
		return nil
	}

	resp.Body.Close()
	return nil
}

// readStreamFromResponse reads SSE events from an HTTP response body.
func (t *streamableHTTPTransport) readStreamFromResponse(body io.ReadCloser) {
	defer body.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 256)
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}
		n, err := body.Read(chunk)
		if err != nil {
			return
		}
		buf = append(buf, chunk[:n]...)
		for {
			idx := bytes.Index(buf, []byte("\n\n"))
			if idx == -1 {
				break
			}
			event := strings.TrimSpace(string(buf[:idx]))
			buf = buf[idx+2:]
			if strings.HasPrefix(event, "data: ") {
				payload := strings.TrimPrefix(event, "data: ")
				select {
				case t.msgChan <- json.RawMessage(payload):
				case <-t.ctx.Done():
					return
				}
			}
		}
	}
}

// Receive returns the next server→client message.
func (t *streamableHTTPTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg := <-t.msgChan:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, io.EOF
	}
}

// Close shuts down the transport.
func (t *streamableHTTPTransport) Close() error {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stream != nil {
		t.stream.Close()
		t.stream = nil
	}
	return nil
}
