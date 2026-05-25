# Plan: Docker 重建 + API 端口详细测试

## Context

用户要求基于最新代码重建 Docker 服务，然后对 API 端口进行详细测试，结合 API 返回和 docker logs 综合分析，并将结果写入测试文件。

## 步骤

### 1. 确认代码为最新

- `git status` 确认无未暂存的关键改动影响构建（当前有新增 tool_error 相关文件，需要确保都被包含）
- 确认 git 工作目录干净或所有改动都已在本地（Docker build 用 `COPY .`，会包含未提交文件）

### 2. 重建 Docker 服务

```bash
docker compose down -v   # 清理旧容器和 volume
docker compose build --no-cache agent   # 用最新代码重建
docker compose up -d     # 启动全部服务
```

等待健康检查通过后开始测试。

### 3. API 端口测试

对 localhost:8080 进行分组测试：

| 分组 | 端点 |
|------|------|
| 健康检查 | GET /healthz, GET /readyz |
| Session 管理 | POST /api/v1/sessions, GET /api/v1/sessions/:id, DELETE /api/v1/sessions/:id |
| Chat (核心) | POST /api/v1/chat, POST /api/v1/chat/stream, POST /api/v1/chat/react-stream |
| 工具/MCP/Skills | GET /api/v1/tools, GET /api/v1/mcp/servers, GET /api/v1/skills |
| Workspace | GET /api/v1/workspaces |
| Auth | POST /api/v1/auth/token |

每个测试记录：
- curl 命令
- HTTP status code + response body
- 对应时间点的 `docker logs code-agent` 输出

### 4. 综合分析并写入测试结果文件

将所有测试结果（API 响应 + Docker 日志摘要 + 分析结论）写入 `test_results.log`（覆盖旧内容）。

## 验证

- 所有健康检查端点返回 200
- Session 创建/查询/删除正常工作
- Chat 接口能发起对话并返回流式响应
- 失败的接口有明确原因分析（结合 docker logs）
