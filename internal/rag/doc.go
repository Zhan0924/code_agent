// Package rag 实现代码与技术文档的深度语义检索引擎（Retrieval-Augmented Generation）。
//
// # 与普通 RAG 的区别
//
// 本包并非简单的"文本切分+向量搜索"，而是针对代码场景做了 4 项优化：
//
//  1. AST 切分：使用 tree-sitter 按函数/类/结构体粒度切块，保留类型签名、注释、依赖关系
//  2. 双路召回：Dense（语义向量）+ Sparse（BM25 精确关键词）并行检索后做 RRF 融合
//  3. 交叉编码重排：本地 CrossEncoder（bge-reranker）把 Top 40 粗排精排到 Top 5
//  4. 租户隔离：Qdrant Payload 过滤支持 project/version/branch 三级硬过滤
//
// # 检索流水线
//
//	Query → QueryNormalizer → ┬→ Embedder → QdrantVectorSearch (k=20) ─┐
//	                          └→ BM25 Scorer → QdrantTextSearch (k=20) ─┤→ RRF Fusion (k=40) → Reranker → Top 5
//	                                                                    │
//	(索引流水线，异步)
//	Files → AST Parser → Semantic Chunks → Enricher → Embedder → Qdrant.Upsert
//
// # 关键类型
//
//	Engine            —— 对外统一入口，Retrieve / Index / Delete
//	ASTParser         —— tree-sitter 抽象语法树解析器（ast_parser.go / ast_native.go）
//	MarkdownParser    —— 文档类文件的 heading-aware 切分
//	QdrantStore       —— Qdrant gRPC 客户端封装（upsert / search / filter）
//	Embedder          —— 向量生成接口；可选 OpenAI 或本地 TEI (local_embedder.go)
//	Reranker          —— CrossEncoder 本地重排（CPU 可跑）
//	ReconnectWrapper  —— Qdrant 断线自愈（指数退避）
//
// # 性能与容量
//
//   - 单次检索（Top 5）在 100k chunk 上 < 50ms (p95)
//   - 索引吞吐 500 chunks/s（ada-002 并行 batch）
//   - Qdrant 水平扩展：3 node StatefulSet 可承载 10M+ 向量
//
// # 软降级
//
// 当 Qdrant/Embedder 不可用时，Retrieve 返回空切片而非 error，使得 Orchestrator
// 依然能走 Tool-Call 路线完成问答（只是缺失代码上下文）。指标 rag_degraded_total
// 上报，便于告警。
//
// 详见 docs/architecture/04_rag.md。
package rag
