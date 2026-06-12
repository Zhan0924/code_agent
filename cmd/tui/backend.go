package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/tui"
)

// RemoteBackend connects to a running code-agent HTTP server via SSE.
type RemoteBackend struct {
	baseURL    string
	httpClient *http.Client
}

func NewRemoteBackend(addr string) *RemoteBackend {
	return &RemoteBackend{
		baseURL:    fmt.Sprintf("http://%s", addr),
		httpClient: &http.Client{},
	}
}

func (r *RemoteBackend) SendMessage(ctx context.Context, sessionID, message string) (<-chan models.ReactStreamEvent, error) {
	eventCh := make(chan models.ReactStreamEvent, 64)

	payload := map[string]string{"session_id": sessionID, "message": message}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/api/v1/chat/react-stream", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to agent: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	go r.readSSE(ctx, resp.Body, eventCh)
	return eventCh, nil
}

func (r *RemoteBackend) readSSE(_ context.Context, body io.ReadCloser, ch chan<- models.ReactStreamEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip empty lines and event lines
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		
		// Parse the outer wrapper
		var wrapper struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
			continue
		}
		
		// Re-marshal the data field to parse as ReactStreamEvent
		dataBytes, err := json.Marshal(wrapper.Data)
		if err != nil {
			continue
		}
		
		var ev models.ReactStreamEvent
		if err := json.Unmarshal(dataBytes, &ev); err != nil {
			continue
		}
		ch <- ev
	}
}

func (r *RemoteBackend) CreateSession(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/api/v1/sessions", strings.NewReader(`{}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.SessionID, nil
}

func (r *RemoteBackend) ListSessions(ctx context.Context) ([]tui.SessionSummary, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", r.baseURL+"/api/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Sessions []struct {
			ID           string `json:"id"`
			MessageCount int    `json:"message_count"`
			LastPreview  string `json:"last_message_preview"`
			UpdatedAt    string `json:"updated_at"`
		} `json:"sessions"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	summaries := make([]tui.SessionSummary, len(result.Sessions))
	for i, s := range result.Sessions {
		summaries[i] = tui.SessionSummary{
			ID:             s.ID,
			MessageCount:   s.MessageCount,
			LastMessage:    s.LastPreview,
			LastUpdateTime: s.UpdatedAt,
		}
	}
	return summaries, nil
}

func (r *RemoteBackend) Close() error { return nil }