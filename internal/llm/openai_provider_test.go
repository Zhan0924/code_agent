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

func TestOpenAIProvider_ConvertMessages(t *testing.T) {
	cfg := &config.LLMProviderConfig{
		Provider:    "openai",
		Model:       "gpt-4",
		APIKey:      "test-key",
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	tests := []struct {
		name         string
		input        []models.Message
		wantMsgCount int
		wantRoles    []string
	}{
		{
			name: "basic_user_assistant",
			input: []models.Message{
				{Role: models.RoleUser, Content: "Hello"},
				{Role: models.RoleAssistant, Content: "Hi there!"},
			},
			wantMsgCount: 2,
			wantRoles:    []string{"user", "assistant"},
		},
		{
			name: "system_message_preserved",
			input: []models.Message{
				{Role: models.RoleSystem, Content: "You are helpful."},
				{Role: models.RoleUser, Content: "Hello"},
			},
			wantMsgCount: 2,
			wantRoles:    []string{"system", "user"},
		},
		{
			name: "tool_result_with_tool_call_id",
			input: []models.Message{
				{Role: models.RoleUser, Content: "Run test"},
				{Role: models.RoleAssistant, Content: "", ToolCalls: []models.ToolCall{
					{ID: "call_1", Name: "run_test", Args: json.RawMessage(`{"file":"test.py"}`)},
				}},
				{Role: models.RoleTool, Content: "Test passed", ToolCallID: "call_1"},
			},
			wantMsgCount: 3,
			wantRoles:    []string{"user", "assistant", "tool"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := p.convertMessages(tt.input)
			if len(messages) != tt.wantMsgCount {
				t.Errorf("got %d messages, want %d", len(messages), tt.wantMsgCount)
			}
			for i, wantRole := range tt.wantRoles {
				if i >= len(messages) {
					break
				}
				if messages[i].Role != wantRole {
					t.Errorf("message[%d].role = %s, want %s", i, messages[i].Role, wantRole)
				}
			}

			// Verify tool call ID is set for tool messages
			for i, msg := range tt.input {
				if msg.Role == models.RoleTool && msg.ToolCallID != "" {
					if i >= len(messages) {
						continue
					}
					if messages[i].ToolCallID != msg.ToolCallID {
						t.Errorf("message[%d].tool_call_id = %s, want %s", i, messages[i].ToolCallID, msg.ToolCallID)
					}
				}
			}
		})
	}
}

func TestOpenAIProvider_ConvertMessagesWithCache(t *testing.T) {
	cfg := &config.LLMProviderConfig{
		Provider:            "openai",
		Model:               "gpt-4",
		APIKey:              "test-key",
		MaxTokens:           4096,
		Temperature:         0.7,
		Timeout:             30 * time.Second,
		EnablePromptCaching: true,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
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

	messages := p.convertMessagesWithCache(input)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// First message should have cache_control
	if messages[0].CacheControl == nil {
		t.Error("first message should have cache_control")
	} else if messages[0].CacheControl.Type != "ephemeral" {
		t.Errorf("first message cache_control.type = %s, want ephemeral", messages[0].CacheControl.Type)
	}

	// Second message should not have cache_control
	if messages[1].CacheControl != nil {
		t.Errorf("second message should not have cache_control, got %+v", messages[1].CacheControl)
	}
}

func TestOpenAIProvider_ConvertTools(t *testing.T) {
	cfg := &config.LLMProviderConfig{
		Provider:    "openai",
		Model:       "gpt-4",
		APIKey:      "test-key",
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
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
		{
			Name:        "write_file",
			Description: "Write content to a file",
			Parameters:  json.RawMessage(`{}`),
		},
	}

	tools := p.convertTools(input)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// Check first tool
	if tools[0].Function.Name != "read_file" {
		t.Errorf("tool[0].name = %s, want read_file", tools[0].Function.Name)
	}
	if tools[0].Function.Description != "Read a file from disk" {
		t.Errorf("tool[0].description = %s, want 'Read a file from disk'", tools[0].Function.Description)
	}

	// Check second tool (empty parameters should be handled)
	if tools[1].Function.Name != "write_file" {
		t.Errorf("tool[1].name = %s, want write_file", tools[1].Function.Name)
	}
}

func TestOpenAIProvider_ChatCompletion(t *testing.T) {
	// Mock OpenAI API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			t.Errorf("expected /chat/completions path, got %s", r.URL.Path)
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
		if req["model"] != "gpt-4" {
			t.Errorf("model = %v, want gpt-4", req["model"])
		}

		// Send mock response
		resp := map[string]interface{}{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   "gpt-4",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Hello! How can I help you today?",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     12,
				"completion_tokens": 18,
				"total_tokens":      30,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "openai",
		Model:       "gpt-4",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
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

	if resp.Content != "Hello! How can I help you today?" {
		t.Errorf("content = %q, want 'Hello! How can I help you today?'", resp.Content)
	}
	if resp.Usage.PromptTokens != 12 {
		t.Errorf("prompt tokens = %d, want 12", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 18 {
		t.Errorf("completion tokens = %d, want 18", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("total tokens = %d, want 30", resp.Usage.TotalTokens)
	}
}

func TestOpenAIProvider_ChatCompletion_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"id":      "chatcmpl-456",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   "gpt-4",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Let me read that file for you.",
						"tool_calls": []map[string]interface{}{
							{
								"id":   "call_xyz",
								"type": "function",
								"function": map[string]interface{}{
									"name":      "read_file",
									"arguments": `{"path":"/tmp/test.txt"}`,
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     20,
				"completion_tokens": 30,
				"total_tokens":      50,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "openai",
		Model:       "gpt-4",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
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

	if resp.Content != "Let me read that file for you." {
		t.Errorf("content = %q, want 'Let me read that file for you.'", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_xyz" {
		t.Errorf("tool call ID = %s, want call_xyz", resp.ToolCalls[0].ID)
	}
	if resp.ToolCalls[0].Name != "read_file" {
		t.Errorf("tool call name = %s, want read_file", resp.ToolCalls[0].Name)
	}

	// Verify tool call arguments are valid JSON
	var args map[string]interface{}
	if err := json.Unmarshal(resp.ToolCalls[0].Args, &args); err != nil {
		t.Errorf("tool call args are not valid JSON: %v", err)
	}
	if args["path"] != "/tmp/test.txt" {
		t.Errorf("tool call args.path = %v, want /tmp/test.txt", args["path"])
	}
}

func TestOpenAIProvider_ChatCompletionStream(t *testing.T) {
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

		// First chunk
		chunk1 := map[string]interface{}{
			"id":      "chatcmpl-789",
			"object":  "chat.completion.chunk",
			"created": 1234567890,
			"model":   "gpt-4",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"role":    "assistant",
						"content": "Hello",
					},
				},
			},
		}
		data, _ := json.Marshal(chunk1)
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// Second chunk
		chunk2 := map[string]interface{}{
			"id":      "chatcmpl-789",
			"object":  "chat.completion.chunk",
			"created": 1234567890,
			"model":   "gpt-4",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"content": " world",
					},
				},
			},
		}
		data, _ = json.Marshal(chunk2)
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// Done marker
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "openai",
		Model:       "gpt-4",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
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

func TestOpenAIProvider_ChatCompletionStream_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		// Tool call chunk 1: ID and name
		chunk1 := map[string]interface{}{
			"id":      "chatcmpl-tool",
			"object":  "chat.completion.chunk",
			"created": 1234567890,
			"model":   "gpt-4",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []map[string]interface{}{
							{
								"index": 0,
								"id":    "call_123",
								"type":  "function",
								"function": map[string]interface{}{
									"name":      "read_file",
									"arguments": "",
								},
							},
						},
					},
				},
			},
		}
		data, _ := json.Marshal(chunk1)
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// Tool call chunk 2: arguments
		chunk2 := map[string]interface{}{
			"id":      "chatcmpl-tool",
			"object":  "chat.completion.chunk",
			"created": 1234567890,
			"model":   "gpt-4",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []map[string]interface{}{
							{
								"index": 0,
								"function": map[string]interface{}{
									"arguments": `{"path":"/tmp/file.txt"}`,
								},
							},
						},
					},
				},
			},
		}
		data, _ = json.Marshal(chunk2)
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		flusher.Flush()

		// Done
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	cfg := &config.LLMProviderConfig{
		Provider:    "openai",
		Model:       "gpt-4",
		APIKey:      "test-key",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
		Timeout:     30 * time.Second,
	}
	logger := zap.NewNop()
	p, err := newOpenAIProvider(cfg, nil, logger)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	req := &ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "Read file"},
		},
		Tools: []models.ToolDefinition{
			{Name: "read_file", Description: "Read a file", Parameters: json.RawMessage(`{}`)},
		},
	}

	ctx := context.Background()
	ch, err := p.ChatCompletionStream(ctx, req)
	if err != nil {
		t.Fatalf("ChatCompletionStream failed: %v", err)
	}

	var toolCalls []models.ToolCall
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		toolCalls = append(toolCalls, chunk.ToolCalls...)
	}

	// Verify we got tool calls
	if len(toolCalls) == 0 {
		t.Fatal("expected tool calls, got none")
	}

	// Check first tool call
	if toolCalls[0].ID != "call_123" {
		t.Errorf("tool call ID = %s, want call_123", toolCalls[0].ID)
	}
	if toolCalls[0].Name != "read_file" {
		t.Errorf("tool call name = %s, want read_file", toolCalls[0].Name)
	}
}
