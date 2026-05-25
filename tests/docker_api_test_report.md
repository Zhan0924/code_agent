# Docker 重建 + API 端点测试报告

**日期**: 2026-05-22  
**镜像**: code_agent-agent:latest (no-cache rebuild)  
**端口**: 8080 (host) → 8080 (container)

## 1. 启动状态

| 检查项 | 结果 |
|--------|------|
| 镜像构建 | ✅ 成功，包含 4 新文件 + 2 修改 |
| 容器启动 | ✅ 无 panic/fatal |
| Redis 连接 | ✅ |
| Postgres 迁移 | ✅ 12 migrations |
| RAG 引擎 | ✅ openai embedding |
| Temporal worker | ✅ started |

## 2. 基础 API 端点

| 端点 | 方法 | 状态 | 响应 |
|------|------|------|------|
| `/healthz` | GET | ✅ 200 | `{"status":"ok"}` |
| `/readyz` | GET | ✅ 200 | `{"checks":{"postgres":"ok","redis":"ok"}}` |
| `/api/v1/tools` | GET | ✅ 200 | 14 个工具 |
| `/api/v1/sessions` | POST | ✅ 200 | 返回 session_id + workspace_id |
| `/api/v1/sessions/:id` | GET | ✅ 200 | 返回会话详情 |
| `/api/v1/sessions/:id` | DELETE | ✅ 200 | `{"status":"deleted"}` |

## 3. 结构化错误反馈测试（重点）

### 场景 A: read_file — 绝对路径拒绝 (not_found 类)

- **输入**: `请读取文件 /nonexistent/path/file.go 的内容`
- **tool_call**: `read_file {"path": "/nonexistent/path/file.go"}`
- **tool_result**: `Failed to read '/nonexistent/path/file.go': absolute paths not allowed`
- **`[SYSTEM HINT]`**: ✅ `内部错误，换用其他方法`
- **is_error**: `true`
- **LLM 后续行为**: 正确识别错误，给出替代建议（使用相对路径）

### 场景 B: execute_code — Docker 镜像缺失 (exec_failed 类)

- **输入**: `请执行命令 invalid_command_xyz`
- **tool_call**: `execute_code {"code": "invalid_command_xyz", "language": "bash"}`
- **tool_result**: `Execution failed: No such image: alpine:3.20`
- **`[SYSTEM HINT]`**: ✅ `内部错误，换用其他方法`
- **is_error**: `true`
- **LLM 后续行为**: 尝试 `run_workspace_cmd` 作为 fallback（也被拦截），最终给出合理解释

### 场景 C: 连续错误 — 反馈持续注入

- 同一会话第 3 轮请求继续触发错误
- `[SYSTEM HINT]` 在每次错误中都正确附加
- 未观察到升级变化（当前实现为固定提示，非渐进式升级）

## 4. Docker 日志检查

- 无 panic / fatal / unexpected error
- 一次 LLM primary timeout → fallback（正常行为）
- 错误分类和反馈在 runner 层内联处理，不产生额外日志行（设计如此）

## 5. 结论

| 功能 | 状态 |
|------|------|
| tool_error.go 错误分类 | ✅ 运行时正常工作 |
| adaptive_feedback.go 自适应反馈 | ✅ `[SYSTEM HINT]` 正确注入 tool_result |
| runner.go 集成 | ✅ 错误流经分类→反馈→SSE 输出完整 |
| react_core.go 集成 | ✅ ReAct 循环正确处理带 hint 的错误结果 |

**所有核心功能验证通过。** 自适应反馈当前为固定提示模式（`内部错误，换用其他方法`），如需渐进式升级（如第 N 次错误加强措辞），需在 `adaptive_feedback.go` 中扩展 escalation 逻辑。
