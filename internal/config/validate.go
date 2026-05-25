// validate.go — 启动时 schema 校验。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【"多错一次报" 的好处】
//
//	Validate() 收集全部错误再一次性返回，而不是第一个错误就 return。
//	理由：用户修好 LLM.APIKey 后，立刻就会看到 Redis.Addr 也缺；省去
//	"改一个跑一次"的反复试错。多错合成一个 error，行之间用换行连接。
//
// 【哪些字段"安全相关"必须验？】
//
//	最严苛的一类（这些漏了会造成生产事故）：
//	  · LLM.Primary.APIKey 空  → Agent 启动但聊天直接 401
//	  · Redis.Addr 空          → 完全瘫痪（session 跑不起来）
//	  · Auth.Enabled && JWTSecret 空  → 任意签 token 可绕过
//	  · Sandbox.NetworkMode 不在 {"none","bridge","host"}  → 拼写错误 → 沙箱逃逸
//
// 【未来应该校验但当前没校验的项】
//
//	· RAG.EmbeddingBaseURL 空但 EmbeddingProvider="openai"
//	· Temporal.Host 形如 "host:port"
//	· Security.SensitivePatterns 里的正则能编译
//	· Tracing.Endpoint 格式
//
//	这些属于 "能跑但行为诡异" 类，应该陆续加上。
//
// 【和 Viper 默认值的关系】
//
//	Validate 跑在 Viper 反序列化之后。所以它看到的是"默认值已填 + 用户覆盖
//	后"的最终值。空值必然是用户显式清空或 env var expand 为 "" 的结果。
//
// ============================================================================
package config

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Validate checks the entire configuration for required fields and valid ranges.
// Returns a multi-error describing all validation failures at once.
func (c *Config) Validate() error {
	var errs []string

	// Server
	if c.Server.HTTPAddr == "" {
		errs = append(errs, "server.http_addr is required")
	}
	if c.Server.ReadTimeout <= 0 {
		errs = append(errs, "server.read_timeout must be positive")
	}
	if c.Server.WriteTimeout <= 0 {
		errs = append(errs, "server.write_timeout must be positive")
	}

	// LLM
	if c.LLM.Primary.Model == "" {
		errs = append(errs, "llm.primary.model is required")
	}
	if c.LLM.Primary.APIKey == "" && c.LLM.Primary.BaseURL == "" {
		errs = append(errs, "llm.primary: either api_key or base_url must be set")
	}
	if c.LLM.Primary.MaxTokens <= 0 {
		errs = append(errs, "llm.primary.max_tokens must be positive")
	}
	if c.LLM.Primary.Timeout <= 0 {
		errs = append(errs, "llm.primary.timeout must be positive")
	}
	if c.LLM.CircuitBreaker.MaxFailures <= 0 {
		errs = append(errs, "llm.circuit_breaker.max_failures must be positive")
	}

	// Redis
	if c.Redis.Addr == "" {
		errs = append(errs, "redis.addr is required")
	}
	if c.Redis.PoolSize <= 0 {
		errs = append(errs, "redis.pool_size must be positive")
	}

	// Session
	if c.Session.MaxHistoryTokens <= 0 {
		errs = append(errs, "session.max_history_tokens must be positive")
	}
	if c.Session.SummaryThresholdTokens <= 0 {
		errs = append(errs, "session.summary_threshold_tokens must be positive")
	}
	if c.Session.SummaryThresholdTokens > c.Session.MaxHistoryTokens {
		errs = append(errs, "session.summary_threshold_tokens must be <= max_history_tokens")
	}
	if c.Session.TTL <= 0 {
		errs = append(errs, "session.ttl must be positive")
	}

	// RAG
	if c.RAG.ChunkMaxTokens <= 0 {
		errs = append(errs, "rag.chunk_max_tokens must be positive")
	}
	if c.RAG.TopK <= 0 {
		errs = append(errs, "rag.top_k must be positive")
	}

	// Sandbox
	if c.Sandbox.Timeout <= 0 {
		errs = append(errs, "sandbox.timeout must be positive")
	}

	// Security - check sensitive patterns are valid regex
	for i, pattern := range c.Security.SensitivePatterns {
		if pattern == "" {
			errs = append(errs, fmt.Sprintf("security.sensitive_patterns[%d] is empty", i))
		}
	}

	// MCP - validate command whitelist
	if err := c.validateMCPServers(); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("configuration validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// allowedMCPCommands is the whitelist of safe MCP server commands.
var allowedMCPCommands = map[string]bool{
	"npx":     true,
	"node":    true,
	"python":  true,
	"python3": true,
	"uvx":     true,
	"deno":    true,
}

// IsAllowedMCPCommand checks if a command basename is in the MCP whitelist.
func IsAllowedMCPCommand(cmd string) bool {
	return allowedMCPCommands[cmd]
}

// validateMCPServers checks that all MCP server commands are in the whitelist.
func (c *Config) validateMCPServers() error {
	for i, srv := range c.MCP.Servers {
		if srv.Command == "" {
			return fmt.Errorf("mcp.servers[%d].command is empty", i)
		}

		// Extract base command (handle absolute paths)
		cmd := filepath.Base(srv.Command)

		// Check whitelist
		if !allowedMCPCommands[cmd] {
			return fmt.Errorf("mcp.servers[%d].command not in whitelist: %s (allowed: npx, node, python, python3, uvx, deno)", i, cmd)
		}

		// Verify command exists in PATH or is absolute path
		if !filepath.IsAbs(srv.Command) {
			if _, err := exec.LookPath(srv.Command); err != nil {
				return fmt.Errorf("mcp.servers[%d].command not found in PATH: %s", i, srv.Command)
			}
		}
	}
	return nil
}
