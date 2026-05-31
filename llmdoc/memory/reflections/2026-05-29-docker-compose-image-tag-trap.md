# 反思：Docker Compose 镜像标签陷阱 + P1 功能验证

日期：2026-05-29

## 触发事件

为验证 P1 功能（Tree-sitter / LSP / PTY）在 Docker 中生效，按计划：
1. 更新 `configs/config.yaml` 启用 `tree_sitter.enabled = true` 和 `pty.enabled = true`
2. 重新构建镜像并启动服务
3. 通过 API 返回 + Docker 日志双重验证

实际过程踩了两个坑，调试耗时约 30 分钟，远超预期。

## 做得好的地方

- 验证策略合理：API 返回 + 日志双重验证，能区分"工具注册了但未运行"与"完全未注册"
- 加调试日志（`logger.Info("P1 config debug", ...)`）后立即定位了真正问题——日志里的 caller 行号仍是旧版本（main.go:577 → main.go:654 直接跳过），证明运行的是旧 binary
- 用 volume mount (`./configs/config.yaml:/etc/code-agent/configs/config.yaml:ro`) 解决了配置同步问题（镜像内 layer cache 的旧 config 被运行时 mask 掉）。注意：**未** 避免密钥入镜像——Dockerfile 仍 `COPY configs/` 整个目录，`.dockerignore` 也未排除它；volume 只是运行时遮盖，镜像层里仍含本地 config.yaml。要真正避免，需改 `.dockerignore` 或 Dockerfile。

## 做错的地方 / 返工

1. **不必要的 docker build --no-cache 重建**：第一次容器内配置缺少 `tree_sitter`/`pty` 段时，怀疑是 Docker layer 缓存问题，跑了 `docker build --no-cache` 重建（耗时约 2 分钟）。实际上 `docker build -t code-agent:latest .` 构建的镜像 **不被 `docker compose up -d` 使用**（compose 有自己的标签命名 `code_agent-agent:latest`）。多次重建的镜像被 compose 完全忽略。

2. **怀疑方向错误**：先怀疑是 Viper SetEnvKeyReplacer 把 `tree_sitter` 误解析为 `tree.sitter`，再怀疑 .dockerignore 排除了 config.yaml，再怀疑 binary 优化删除了字符串。真正原因是 compose 用错误镜像，验证步骤应更早：`docker compose images agent` 看实际使用的镜像 ID 和构建时间。

3. **配置文件 .gitignore 的副作用未预见**：`configs/config.yaml` 在 .gitignore 中（含密钥）。Dockerfile `COPY .` 会复制工作区文件（不受 .gitignore 影响），所以"未提交"本身不是问题。但 `docker build` vs `docker compose build` 用不同镜像才是真正陷阱。

## 根因分析

- **docker build vs docker compose build 标签不一致**：
  - `docker build -t code-agent:latest .` → 镜像名 `code-agent:latest`
  - `docker compose build agent` → 镜像名 `code_agent-agent:latest`（项目名 + 服务名）
  - compose 的 `build: .` 字段不会复用 `code-agent:latest`，除非显式声明 `image: code-agent:latest`
  - Makefile 的 `docker-build` target 用了第一种，但实际部署用 compose，这俩没对齐

- **二进制行号是验证镜像新鲜度的金标准**：日志里的 `caller: "agent/main.go:577"` 是编译时确定的源码行号。在源码新加几行后，编译产物的行号必然变化。这是最直接的"镜像是不是新的"验证。

## 缺失的文档或信号

1. **Makefile 的 `docker-build` 和 `docker-up` 的关系未记录**：使用者会自然地以为 `make docker-build && make docker-up` 是"构建并启动"，但实际 `docker-up` 用的是 compose 的镜像，与 `docker-build` 产物无关。
2. **config.yaml 的部署方式未记录**：之前 Dockerfile `COPY configs` 把配置烤入镜像，但 config.yaml 在 .gitignore 中。docker-compose 部署时应该用 volume mount，但这个最佳实践没记在任何文档里。
3. **Docker 镜像新鲜度验证模式未记录**：日志 caller 行号 / `strings <binary> | rg <known_string>` / `docker compose images` 三种验证手段在排查中很有用，应作为部署指南的一部分。

## 晋升候选（从 memory → 稳定文档）

| 内容 | 目标位置 | 理由 |
|------|----------|------|
| docker build vs docker compose build 镜像标签差异 | `docs/architecture/20_deploy.md`（已存在）+ `must/working-agreement.md` 新增"Docker 部署陷阱"节 | 高频踩坑点，影响所有 Docker 验证任务 |
| config.yaml volume mount 模式 | `docs/architecture/20_deploy.md` "关键细节"节（已补充） | 部署模式变更，必须记录 |
| Docker 镜像新鲜度验证三招 | `guides/docker-debugging.md`（新建）或 `docs/architecture/20_deploy.md` 补节 | 排障流程，未来会复用 |

## 后续行动

1. **修复 Makefile**：`docker-build` 应改为 `docker compose build`，或者在 docker-compose.yml 中显式 `image: code-agent:${VERSION}` 让两者对齐
2. **更新 docs/architecture/20_deploy.md**：补充镜像标签陷阱 + 镜像新鲜度验证三招（caller 行号、strings 检查、`docker compose images`）
3. **.dockerignore 显式排除 configs/config.yaml**（**未做**）：当前 Dockerfile 仍 `COPY configs/`，volume mount 只是运行时遮盖、不是真正的密钥隔离。要让镜像可发布，必须在 `.dockerignore` 排除 `configs/config.yaml`（或 Dockerfile 改为只 `COPY config.example.yaml`）
4. **更新 must/working-agreement.md**：新增"Docker 部署陷阱"节，简要列出本次踩坑要点

## 本次任务总结

- 提交：1 commit (`a8ac81e` feat: 通过 volume 挂载配置文件)
- 验证：5 步骤全部通过（健康检查、工具注册、tree-sitter 索引、PTY shell_exec）
- 已知限制：tree-sitter 在 Docker 内使用 regex fallback（Dockerfile `CGO_ENABLED=0`），LSP 暂未启用
