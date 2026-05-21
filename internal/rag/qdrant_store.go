// qdrant_store.go — Qdrant 向量库的 VectorStore 实现 + 本地 BM25 稀疏索引。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【Qdrant 作为主向量库的理由】
//
//	· gRPC 协议，单次 Search 往返 2-5ms（Milvus 同等硬件 5-15ms）；
//	· Payload filter 能把 tenant_id / project / version 硬过滤，不用向量事
//	  先染色；
//	· 原生支持 scalar quantization，内存占用比 float32 小 4-8×；
//	· 单二进制部署，运维简单。
//
// 【Point ID 策略】
//
//	chunk 的天然 ID 是 `filepath:symbol:line`，但 Qdrant 强制 Point ID 必须
//	是 UUID 或 int64。deterministicUUID 把任意字符串 SHA-256 后格式化成
//	UUID v4。同一 chunk 被重新索引时 ID 稳定，upsert 语义 = 原地覆盖。
//
// 【双路召回的实现】
//
//	· 稠密（SearchDense）：走 Qdrant 原生 Query API，input 是 embedding 向量。
//	· 稀疏（SearchSparse）：走**本地** BM25 索引（见 bm25.go）。Qdrant 1.8+
//	  支持 sparse vector，但 go-client 的版本支持尚不稳定，折中用内存索引。
//	   ↑ 这是 P0 #18 的修复——之前 SearchSparse 只做 symbol_name 的子串
//	  匹配，硬编码 score=0.5，基本没用。
//
// 【稀疏索引懒构建 + TTL 刷新】
//
//	第一次调 SearchSparse 时触发 scrollAllChunks + BM25.Build；之后在 5min
//	TTL 内复用。调用方 upsert 新 chunk 后应 InvalidateSparseIndex，否则要等
//	TTL 到期才能搜到。sparseMu 保证同一时刻只有一次重建。
//
// 【payload <-> CodeChunk 转换】
//
//	Qdrant 的 payload 是 map[string]Value（protobuf Struct 风格），我们用
//	codeChunkToPayload / payloadToCodeChunk 双向转换。Metadata 字段是
//	map[string]string，额外存到 payload 里，SearchSparse 在内存侧过滤用。
//
// ============================================================================
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	pb "github.com/qdrant/go-client/qdrant"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// deterministicUUID generates a valid UUID v4-format string from an arbitrary
// string key using SHA-256 hashing. This is necessary because Qdrant requires
// valid UUID format for point IDs, but our chunk IDs are file paths like
// "docs/arch.md:Architecture:5".
func deterministicUUID(key string) string {
	h := sha256.Sum256([]byte(key))
	// Format as UUID v4: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
	// Set version (4) and variant (8/9/a/b) bits
	h[6] = (h[6] & 0x0f) | 0x40 // version 4
	h[8] = (h[8] & 0x3f) | 0x80 // variant 10
	dst := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		dst[0:8], dst[8:12], dst[12:16], dst[16:20], dst[20:32])
}

// QdrantStore implements the VectorStore interface using Qdrant vector database.
type QdrantStore struct {
	conn       *grpc.ClientConn
	points     pb.PointsClient
	collection pb.CollectionsClient
	cfg        *config.QdrantConfig
	logger     *zap.Logger

	// BM25 sparse index, rebuilt lazily and refreshed on a TTL. The prior
	// SearchSparse implementation did a substring match on symbol_name with
	// a hardcoded score; this replaces it with a real in-process BM25 index
	// over all indexed chunks.
	sparseMu      sync.Mutex
	sparseIndex   *BM25Index
	sparseBuiltAt time.Time
	sparseTTL     time.Duration // how stale the index can get before rebuild
}

// NewQdrantStore creates a new Qdrant-backed vector store.
func NewQdrantStore(cfg *config.QdrantConfig, logger *zap.Logger) (*QdrantStore, error) {
	conn, err := grpc.NewClient(
		cfg.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Qdrant: %w", err)
	}

	store := &QdrantStore{
		conn:        conn,
		points:      pb.NewPointsClient(conn),
		collection:  pb.NewCollectionsClient(conn),
		cfg:         cfg,
		logger:      logger.With(zap.String("component", "qdrant")),
		sparseIndex: NewBM25Index(),
		sparseTTL:   5 * time.Minute, // rebuild sparse index every 5min on demand
	}

	// Ensure collection exists
	if err := store.ensureCollection(context.Background()); err != nil {
		conn.Close()
		return nil, err
	}

	return store, nil
}

// ensureCollection creates the vector collection if it doesn't exist.
func (s *QdrantStore) ensureCollection(ctx context.Context) error {
	// Check if collection exists
	_, err := s.collection.Get(ctx, &pb.GetCollectionInfoRequest{
		CollectionName: s.cfg.Collection,
	})
	if err == nil {
		return nil // Collection already exists
	}

	// Create collection with the configured vector size
	vectorSize := uint64(s.cfg.VectorSize)
	_, err = s.collection.Create(ctx, &pb.CreateCollection{
		CollectionName: s.cfg.Collection,
		VectorsConfig: &pb.VectorsConfig{
			Config: &pb.VectorsConfig_Params{
				Params: &pb.VectorParams{
					Size:     vectorSize,
					Distance: pb.Distance_Cosine,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create collection: %w", err)
	}

	s.logger.Info("qdrant collection created",
		zap.String("collection", s.cfg.Collection),
		zap.Uint64("vector_size", vectorSize),
	)
	return nil
}

// Upsert stores code chunks with their embeddings in Qdrant.
func (s *QdrantStore) Upsert(ctx context.Context, chunks []models.CodeChunk) error {
	points := make([]*pb.PointStruct, 0, len(chunks))

	for _, chunk := range chunks {
		if len(chunk.Embedding) == 0 {
			continue
		}

		payload := map[string]*pb.Value{
			"file_path":   {Kind: &pb.Value_StringValue{StringValue: chunk.FilePath}},
			"language":    {Kind: &pb.Value_StringValue{StringValue: chunk.Language}},
			"symbol_name": {Kind: &pb.Value_StringValue{StringValue: chunk.SymbolName}},
			"symbol_type": {Kind: &pb.Value_StringValue{StringValue: chunk.SymbolType}},
			"content":     {Kind: &pb.Value_StringValue{StringValue: chunk.Content}},
			"start_line":  {Kind: &pb.Value_IntegerValue{IntegerValue: int64(chunk.StartLine)}},
			"end_line":    {Kind: &pb.Value_IntegerValue{IntegerValue: int64(chunk.EndLine)}},
		}

		// Add metadata
		for k, v := range chunk.Metadata {
			payload[k] = &pb.Value{Kind: &pb.Value_StringValue{StringValue: v}}
		}

		// Generate a deterministic UUID from the chunk ID (which is a file path string).
		// Qdrant requires valid UUID format for PointId_Uuid.
		pointUUID := deterministicUUID(chunk.ID)

		point := &pb.PointStruct{
			Id: &pb.PointId{
				PointIdOptions: &pb.PointId_Uuid{Uuid: pointUUID},
			},
			Vectors: &pb.Vectors{
				VectorsOptions: &pb.Vectors_Vector{
					Vector: &pb.Vector{Data: chunk.Embedding},
				},
			},
			Payload: payload,
		}
		points = append(points, point)
	}

	if len(points) == 0 {
		return nil
	}

	waitUpsert := true
	_, err := s.points.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: s.cfg.Collection,
		Wait:           &waitUpsert,
		Points:         points,
	})
	if err != nil {
		return fmt.Errorf("failed to upsert points: %w", err)
	}

	return nil
}

// SearchDense performs a dense vector similarity search.
func (s *QdrantStore) SearchDense(ctx context.Context, vector []float32, topK int, filters map[string]string) ([]models.RetrievalResult, error) {
	searchReq := &pb.SearchPoints{
		CollectionName: s.cfg.Collection,
		Vector:         vector,
		Limit:          uint64(topK),
		WithPayload:    &pb.WithPayloadSelector{SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true}},
	}

	// Apply filters
	if len(filters) > 0 {
		searchReq.Filter = buildQdrantFilter(filters)
	}

	resp, err := s.points.Search(ctx, searchReq)
	if err != nil {
		return nil, fmt.Errorf("dense search failed: %w", err)
	}

	return s.convertSearchResults(resp.Result), nil
}

// SearchSparse performs BM25 keyword retrieval over all indexed chunks.
// The index is built lazily on the first call and refreshed on a TTL; callers
// pay the rebuild cost only when the cached index is stale. Post-ranking we
// apply the caller's filters (tenant_id, project, etc.) — BM25 itself is
// filter-agnostic so we over-fetch and filter in-memory.
//
// Scale: see BM25Index — suitable for ≤100k chunks. Beyond that, migrate
// to Qdrant sparse vectors or a dedicated keyword engine.
func (s *QdrantStore) SearchSparse(ctx context.Context, query string, topK int, filters map[string]string) ([]models.RetrievalResult, error) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return nil, nil
	}

	if err := s.ensureSparseIndex(ctx); err != nil {
		return nil, fmt.Errorf("sparse index build failed: %w", err)
	}

	// Over-fetch by 3x when filters are present so filtering in-memory still
	// leaves a useful top-K. When there are no filters, fetch exactly topK.
	fetch := topK
	if len(filters) > 0 {
		fetch = topK * 3
	}
	raw := s.sparseIndex.Search(query, fetch)

	if len(filters) == 0 {
		if len(raw) > topK {
			raw = raw[:topK]
		}
		return raw, nil
	}

	// Apply filters post-hoc. The chunk payload carries tenant/project/etc.
	// as metadata; we match each key exactly.
	filtered := make([]models.RetrievalResult, 0, topK)
	for _, r := range raw {
		if !chunkMatchesFilters(r.Chunk, filters) {
			continue
		}
		filtered = append(filtered, r)
		if len(filtered) >= topK {
			break
		}
	}
	return filtered, nil
}

// ensureSparseIndex rebuilds the BM25 index if it's stale. Uses a per-store
// mutex so concurrent queries serialise on rebuild rather than each doing
// their own scroll.
func (s *QdrantStore) ensureSparseIndex(ctx context.Context) error {
	s.sparseMu.Lock()
	defer s.sparseMu.Unlock()

	if !s.sparseBuiltAt.IsZero() && time.Since(s.sparseBuiltAt) < s.sparseTTL {
		return nil
	}

	chunks, err := s.scrollAllChunks(ctx)
	if err != nil {
		return err
	}
	s.sparseIndex.Build(chunks)
	s.sparseBuiltAt = time.Now()
	s.logger.Info("bm25 index rebuilt",
		zap.Int("chunks", len(chunks)),
		zap.Duration("ttl", s.sparseTTL))
	return nil
}

// InvalidateSparseIndex forces the next SearchSparse call to rebuild.
// Callers that upsert chunks and want fresh results immediately (tests,
// manual reindex paths) should call this after Upsert.
func (s *QdrantStore) InvalidateSparseIndex() {
	s.sparseMu.Lock()
	s.sparseBuiltAt = time.Time{}
	s.sparseMu.Unlock()
}

// scrollAllChunks pages through every point in the collection and returns
// the decoded CodeChunks. Called from ensureSparseIndex under lock.
func (s *QdrantStore) scrollAllChunks(ctx context.Context) ([]models.CodeChunk, error) {
	const pageSize = uint32(512)
	var all []models.CodeChunk
	var offset *pb.PointId

	for {
		req := &pb.ScrollPoints{
			CollectionName: s.cfg.Collection,
			Limit:          pb.PtrOf(pageSize),
			WithPayload:    &pb.WithPayloadSelector{SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true}},
			Offset:         offset,
		}
		resp, err := s.points.Scroll(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, p := range resp.Result {
			chunk := payloadToCodeChunk(p.Payload)
			if p.Id != nil {
				if u := p.Id.GetUuid(); u != "" {
					chunk.ID = u
				}
			}
			all = append(all, chunk)
		}
		if resp.NextPageOffset == nil {
			break
		}
		offset = resp.NextPageOffset
	}
	return all, nil
}

// chunkMatchesFilters reports whether a chunk's metadata satisfies all
// key=value constraints. Missing keys or mismatches reject the chunk.
// Used only by SearchSparse — dense retrieval pushes filters into Qdrant.
func chunkMatchesFilters(chunk models.CodeChunk, filters map[string]string) bool {
	for k, want := range filters {
		got, ok := chunk.Metadata[k]
		if !ok || got != want {
			return false
		}
	}
	return true
}

// Delete removes chunks by IDs from the vector store.
func (s *QdrantStore) Delete(ctx context.Context, ids []string) error {
	pointIDs := make([]*pb.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = &pb.PointId{
			PointIdOptions: &pb.PointId_Uuid{Uuid: id},
		}
	}

	waitDelete := true
	_, err := s.points.Delete(ctx, &pb.DeletePoints{
		CollectionName: s.cfg.Collection,
		Wait:           &waitDelete,
		Points: &pb.PointsSelector{
			PointsSelectorOneOf: &pb.PointsSelector_Points{
				Points: &pb.PointsIdsList{Ids: pointIDs},
			},
		},
	})
	return err
}

// Close releases Qdrant connection resources.
func (s *QdrantStore) Close() error {
	return s.conn.Close()
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func (s *QdrantStore) convertSearchResults(results []*pb.ScoredPoint) []models.RetrievalResult {
	var out []models.RetrievalResult
	for _, r := range results {
		chunk := payloadToCodeChunk(r.Payload)
		if r.Id != nil {
			if uuid := r.Id.GetUuid(); uuid != "" {
				chunk.ID = uuid
			}
		}
		out = append(out, models.RetrievalResult{
			Chunk:  chunk,
			Score:  float64(r.Score),
			Source: "dense",
		})
	}
	return out
}

func payloadToCodeChunk(payload map[string]*pb.Value) models.CodeChunk {
	chunk := models.CodeChunk{
		Metadata: make(map[string]string),
	}

	if v, ok := payload["file_path"]; ok {
		chunk.FilePath = v.GetStringValue()
	}
	if v, ok := payload["language"]; ok {
		chunk.Language = v.GetStringValue()
	}
	if v, ok := payload["symbol_name"]; ok {
		chunk.SymbolName = v.GetStringValue()
	}
	if v, ok := payload["symbol_type"]; ok {
		chunk.SymbolType = v.GetStringValue()
	}
	if v, ok := payload["content"]; ok {
		chunk.Content = v.GetStringValue()
	}
	if v, ok := payload["start_line"]; ok {
		chunk.StartLine = int(v.GetIntegerValue())
	}
	if v, ok := payload["end_line"]; ok {
		chunk.EndLine = int(v.GetIntegerValue())
	}

	return chunk
}

func buildQdrantFilter(filters map[string]string) *pb.Filter {
	if len(filters) == 0 {
		return nil
	}

	var conditions []*pb.Condition
	for k, v := range filters {
		conditions = append(conditions, &pb.Condition{
			ConditionOneOf: &pb.Condition_Field{
				Field: &pb.FieldCondition{
					Key: k,
					Match: &pb.Match{
						MatchValue: &pb.Match_Keyword{Keyword: v},
					},
				},
			},
		})
	}

	return &pb.Filter{Must: conditions}
}
