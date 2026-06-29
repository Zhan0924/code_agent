package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/agent/code_agent/internal/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

type failEmbedder struct{}

func (failEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("injected embedder failure")
}

func degradedCounter(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, metrics.MemoryRetrieveDegradedTotal.WithLabelValues(reason).Write(&m))
	require.NotNil(t, m.Counter)
	return m.Counter.GetValue()
}

func TestEmbedText_RecordsEmbedderFailure(t *testing.T) {
	before := counterValue(t, "embedder", "embed", "error")
	h := &HybridStore{
		embedder: failEmbedder{},
		logger:   zap.NewNop(),
	}
	vec := h.embedText(context.Background(), "我喜欢用 tabs")
	assert.Nil(t, vec)
	after := counterValue(t, "embedder", "embed", "error")
	assert.Equal(t, before+1, after)
}

func TestRetrieve_DegradedWhenEmbedderFails(t *testing.T) {
	before := degradedCounter(t, "embedder_failed")
	h := &HybridStore{
		embedder: failEmbedder{},
		logger:   zap.NewNop(),
	}
	out, err := h.Retrieve(context.Background(), "alice", "p1", "我喜欢用 tabs", 5)
	require.NoError(t, err)
	assert.Nil(t, out)
	after := degradedCounter(t, "embedder_failed")
	assert.Equal(t, before+1, after)
}

func TestRetrieve_DegradedWhenEmbedderNil(t *testing.T) {
	before := degradedCounter(t, "embedder_nil")
	h := &HybridStore{
		logger: zap.NewNop(),
	}
	_, err := h.Retrieve(context.Background(), "alice", "p1", "query", 5)
	require.NoError(t, err)
	after := degradedCounter(t, "embedder_nil")
	assert.Equal(t, before+1, after)
}
