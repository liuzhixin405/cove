package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type sseTransport struct {
	baseURL   string
	client    *http.Client
	sessionID string
	msgChan   chan json.RawMessage
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewSSETransport(baseURL string) (*sseTransport, error) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &sseTransport{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
		msgChan: make(chan json.RawMessage, 64),
		ctx:     ctx,
		cancel:  cancel,
	}

	req, _ := http.NewRequest("POST", t.baseURL+"/sse", nil)
	resp, err := t.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sse connect: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.SessionID == "" {
		cancel()
		return nil, fmt.Errorf("sse: no session ID")
	}
	t.sessionID = result.SessionID

	go t.listenSSE()
	return t, nil
}

func (t *sseTransport) listenSSE() {
	defer t.cancel()
	req, _ := http.NewRequestWithContext(t.ctx, "GET", fmt.Sprintf("%s/message?sessionId=%s", t.baseURL, url.QueryEscape(t.sessionID)), nil)
	req.Header.Set("Accept", "text/event-stream")
	// Use a client with no timeout for long-lived SSE stream
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 256)
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}
		n, err := resp.Body.Read(chunk)
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
				default:
				}
			}
		}
	}
}

func (t *sseTransport) Send(ctx context.Context, msg any) error {
	data, _ := json.Marshal(msg)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/message?sessionId=%s", t.baseURL, url.QueryEscape(t.sessionID)),
		bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (t *sseTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg := <-t.msgChan:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, io.EOF
	}
}

func (t *sseTransport) Close() error {
	t.cancel()
	return nil
}
