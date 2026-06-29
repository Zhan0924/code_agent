package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestHybridStore_DedupOversampleDefault(t *testing.T) {
	h := NewHybridStore(nil, nil, zap.NewNop())
	assert.Equal(t, defaultDedupOversample, h.DedupOversample())
}

func TestHybridStore_SetDedupOversample(t *testing.T) {
	h := NewHybridStore(nil, nil, zap.NewNop())
	h.SetDedupOversample(15)
	assert.Equal(t, 15, h.DedupOversample())
}
