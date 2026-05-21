// Package config 提供 Code Agent 的配置加载、校验与类型安全访问。
//
// # 配置分层（优先级从低到高）
//
//  1. 代码内 Default  —— DefaultConfig() 返回生产安全的默认值
//  2. configs/config.yaml  —— 主配置（Viper 读 YAML/JSON/TOML 皆可）
//  3. 环境变量 CODE_AGENT_*  —— 形如 CODE_AGENT_LLM_PRIMARY_API_KEY，下划线分隔嵌套键
//  4. 命令行 --flag  —— 暂限少数关键 flag（--config / --port）
//
// Viper 的 AutomaticEnv() 把点号 `.` 映射为 `_`，使得 docker-compose/K8s 可纯靠
// env 覆盖而不需要改 yaml。
//
// # 配置结构
//
// Config 根结构聚合 14 个子配置：
//
//	Server    —— HTTP/WS 地址与超时
//	LLM       —— primary/secondary provider + 熔断器
//	Redis     —— 单机或 Cluster 地址、连接池
//	Postgres  —— DSN、连接池、迁移开关
//	Qdrant    —— gRPC 地址、TLS、collection 名
//	Temporal  —— 主机、namespace、task queue
//	Sandbox   —— Docker endpoint、白名单镜像、资源配额
//	MCP       —— 已配置的 MCP server 列表（stdio / HTTP）
//	RAG       —— embedding 模型、chunk 大小、rerank 开关
//	Session   —— 滑动窗口阈值、归档 TTL
//	Security  —— 敏感规则库、HMAC secret、egress policy
//	Logging   —— level、format（json/console）、输出位置
//	Auth      —— JWT secret、APIKey、rate limit 配置
//	Tracing   —— OTLP endpoint、sample rate
//
// # 生产安全默认值
//
// DefaultConfig 的每一项都按"最严"方向取值：
//
//	Sandbox.NetworkMode     = "none"
//	Sandbox.ReadonlyRootfs  = true
//	Security.EgressPolicy   = "deny"
//	Auth.JWTExpireDuration  = 1h (短过期强制 refresh)
//	RateLimit.RPS           = 10 (per IP)
//	LLM.Timeout             = 60s
//
// # 校验（validate.go）
//
// 加载后调 Validate()：
//   - 必填字段非空（secret、addr 等）
//   - 枚举合法（log level in [debug,info,warn,error]）
//   - 数值范围（timeout > 0, pool_size ∈ [1,1000]）
//   - 文件/目录存在（tls cert paths）
//   - 逻辑一致（若 auth.method=jwt 则 jwt_secret 非空）
//
// 失败直接 os.Exit(1) 并打印错误；运行时再读到错配置代价更大。
//
// # 典型用法
//
//	cfg, err := config.Load("configs/config.yaml")
//	if err != nil { log.Fatal(err) }
//	if err := cfg.Validate(); err != nil { log.Fatal(err) }
//
//	srv := api.NewServer(cfg)
//
// 详见 docs/architecture/01_config.md。
package config
