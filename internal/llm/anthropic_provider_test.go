package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestAnthropicProvider_ConvertMessages(t *testing.T) {
	cfg := &config.LLMProviderConfig{
		Provider:            "anthropic",
		Model:               "claude-opus-4-20250514",
		APIKey:              "test-key",
		MaxTokens:           4096,
		Temperature:         0.7,
		Timeout:             30 * time.Second,
		EnablePromptCaching: false,
	}
	logger := zap.NewNop()
	p, err := newAnthropicProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	tests := []struct {
		name           string
		input          []models.Message
		wantMsgCount   int
		wantSystemText string
		wantFirstRole  string
	}{
		{
			name: "system_message_extracted",
			input: []models.Message{
				{Role: models.RoleSystem, Content: "You are a helpful assistant."},
				{Role: models.RoleUser, Content: "Hello"},
			},
			wantMsgCount:   1,
			wantSystemText: "You are a helpful assistant.",
			wantFirstRole:  "user",
		},
		{
			name: "multiple_system_messages_merged",
			input: []models.Message{
				{Role: models.RoleSystem, Content: "System prompt 1."},
				{Role: models.RoleSystem, Content: "System prompt 2."},
				{Role: models.RoleUser, Content: "Hello"},
			},
			wantMsgCount:   1,
			wantSystemText: "System prompt 1.\n\nSystem prompt 2.",
			wantFirstRole:  "user",
		},
		{
			name: "assistant_first_inserts_user",
			input: []models.Message{
				{Role: models.RoleAssistant, Content: "I'm ready."},
			},
			wantMsgCount:   2,
			wantSystemText: "",
			wantFirstRole:  "user",
		},
		{
			name: "tool_result_as_user_role",
			input: []models.Message{
				{Role: models.RoleUser, Content: "Run test"},
				{Role: models.RoleAssistant, Content: "", ToolCalls: []models.ToolCall{
					{ID: "call_1", Name: "run_test", Args: json.RawMessage(`{"file":"test.py"}`)},
				}},
				{Role: models.RoleTool, Content: "Test passed", ToolCallID: "call_1"},
			},
			wantMsgCount:   3,
			wantSystemText: "",
			wantFirstRole:  "user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages, systemText := p.convertMessages(tt.input)
			if len(messages) != tt.wantMsgCount {
				t.Errorf("got %d messages, want %d", len(messages), tt.wantMsgCount)
			}
			if systemText != tt.wantSystemText {
				t.Errorf("got system text %q, want %q", systemText, tt.wantSystemText)
			}
			if len(messages) > 0 && string(messages[0].Role) != tt.wantFirstRole {
				t.Errorf("first message role = %s, want %s", messages[0].Role, tt.wantFirstRole)
			}
		})
	}
}

func TestAnthropicProvider_ConvertMessages_CacheControl(t *testing.T) {
	cfg := &config.LLMProviderConfig{
		Provider:            "anthropic",
		Model:               "claude-opus-4-20250514",
		APIKey:              "test-key",
		MaxTokens:           4096,
		Temperature:         0.7,
		Timeout:             30 * time.Second,
		EnablePromptCaching: true,
	}
	logger := zap.NewNop()
	p, err := newAnthropicProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	input := []models.Message{
		{
			Role:         models.RoleUser,
			Content:      "Cached message",
			CacheControl: &models.CacheControl{Type: "ephemeral"},
		},
		{Role: models.RoleUser, Content: "Regular message"},
	}

	messages, _ := p.convertMessages(input)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// First message should have cache_control
	firstContent := messages[0].Content
	if len(firstContent) == 0 {
		t.Fatal("first message has no content blocks")
	}
	if firstContent[0].OfText == nil {
		t.Fatal("first content block is not text")
	}
	if firstContent[0].OfText.CacheControl.Type != "ephemeral" {
		t.Errorf("first message cache_control.type = %s, want ephemeral", firstContent[0].OfText.CacheControl.Type)
	}

	// Second message should not have cache_control
	secondContent := messages[1].Content
	if len(secondContent) == 0 {
		t.Fatal("second message has no content blocks")
	}
	if secondContent[0].OfText == nil {
		t.Fatal("second content block is not text")
	}
	if secondContent[0].OfText.CacheControl.Type != "" {
		t.Errorf("second message should not have cache_control, got type=%s", secondContent[0].OfText.CacheControl.Type)
	}
}

func TestAnthropicProvider_ConvertTools(t *testing.T) {
	cfg := &config.LLMProviderConfig{
		Provider:    "anthropic",
		Model:       "claude-opus-4-20250514",
		APIKey:      "test-key",
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newAnthropicProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	input := []models.ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a file from disk",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path"}
				},
				"required": ["path"]
			}`),
		},
	}

	tools := p.convertTools(input)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0].OfTool
	if tool == nil {
		t.Fatal("tool is nil")
	}
	if tool.Name != "read_file" {
		t.Errorf("tool name = %s, want read_file", tool.Name)
	}
	if tool.Description.Value != "Read a file from disk" {
		t.Errorf("tool description = %s, want 'Read a file from disk'", tool.Description.Value)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "path" {
		t.Errorf("tool required fields = %v, want [path]", tool.InputSchema.Required)
	}
}

func TestAnthropicProvider_ChatCompletion(t *testing.T) {
	// Mock Anthropic API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/messages") {
			t.Errorf("expected /messages path, got %s", r.URL.Path)
		}

		// Verify Authorization header
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("expected Bearer token, got %s", auth)
		}

		// Read request body
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("failed to parse request: %v", err)
		}

		// Verify model field
		if req["model"] != "claude-opus-4-20250514" {
			t.Errorf("model = %v, want claude-opus-4-20250514", req["model"])
		}

		// Send mock response
		resp := map[string]interface{}{
			"id":    "msg_123",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-opus-4-20250514",
			"content": []map[string]interface{}{
				{"type": "text", "text": "Hello! How can I help you?"},
			},
			"usage": map[string]interface{}{
				"input_tokens":  10,
				"output_tokens": 20,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "anthropic",
		Model:       "claude-opus-4-20250514",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newAnthropicProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	req := &ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "Hello"},
		},
		MaxTokens:   100,
		Temperature: 0.7,
	}

	ctx := context.Background()
	resp, err := p.ChatCompletion(ctx, req)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if resp.Content != "Hello! How can I help you?" {
		t.Errorf("content = %q, want 'Hello! How can I help you?'", resp.Content)
	}
	if resp.Usage.PromptTokens != 10 {
		t.Errorf("prompt tokens = %d, want 10", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 20 {
		t.Errorf("completion tokens = %d, want 20", resp.Usage.CompletionTokens)
	}
}

func TestAnthropicProvider_ChatCompletion_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"id":    "msg_456",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-opus-4-20250514",
			"content": []map[string]interface{}{
				{"type": "text", "text": "Let me read that file."},
				{
					"type":  "tool_use",
					"id":    "call_abc",
					"name":  "read_file",
					"input": map[string]interface{}{"path": "/tmp/test.txt"},
				},
			},
			"usage": map[string]interface{}{
				"input_tokens":  15,
				"output_tokens": 25,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "anthropic",
		Model:       "claude-opus-4-20250514",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newAnthropicProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	req := &ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "Read /tmp/test.txt"},
		},
		Tools: []models.ToolDefinition{
			{
				Name:        "read_file",
				Description: "Read a file",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
	}

	ctx := context.Background()
	resp, err := p.ChatCompletion(ctx, req)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if resp.Content != "Let me read that file." {
		t.Errorf("content = %q, want 'Let me read that file.'", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_abc" {
		t.Errorf("tool call ID = %s, want call_abc", resp.ToolCalls[0].ID)
	}
	if resp.ToolCalls[0].Name != "read_file" {
		t.Errorf("tool call name = %s, want read_file", resp.ToolCalls[0].Name)
	}
}

func TestAnthropicProvider_ChatCompletionStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify stream parameter in request
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)
		if req["stream"] != true {
			t.Errorf("stream = %v, want true", req["stream"])
		}

		// Send SSE-formatted streaming response
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		// message_start event
		w.Write([]byte("event: message_start\n"))
		w.Write([]byte(`data: {"type":"message_start","message":{"id":"msg_789","type":"message","role":"assistant","model":"claude-opus-4-20250514"}}`))
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// content_block_start (text)
		w.Write([]byte("event: content_block_start\n"))
		w.Write([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// content_block_delta (text)
		w.Write([]byte("event: content_block_delta\n"))
		w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`))
		w.Write([]byte("\n\n"))
		flusher.Flush()

		w.Write([]byte("event: content_block_delta\n"))
		w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`))
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// content_block_stop
		w.Write([]byte("event: content_block_stop\n"))
		w.Write([]byte(`data: {"type":"content_block_stop","index":0}`))
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// message_stop
		w.Write([]byte("event: message_stop\n"))
		w.Write([]byte(`data: {"type":"message_stop"}`))
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "anthropic",
		Model:       "claude-opus-4-20250514",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newAnthropicProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	req := &ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "Say hello"},
		},
	}

	ctx := context.Background()
	ch, err := p.ChatCompletionStream(ctx, req)
	if err != nil {
		t.Fatalf("ChatCompletionStream failed: %v", err)
	}

	var chunks []StreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
	}

	// Verify we got content chunks
	var content strings.Builder
	for _, chunk := range chunks {
		content.WriteString(chunk.Content)
	}
	if content.String() != "Hello world" {
		t.Errorf("streamed content = %q, want 'Hello world'", content.String())
	}

	// Verify last chunk is done
	if len(chunks) == 0 || !chunks[len(chunks)-1].Done {
		t.Error("expected last chunk to have Done=true")
	}
}
