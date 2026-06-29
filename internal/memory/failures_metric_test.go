package memory

import (
	"testing"

	"github.com/agent/code_agent/internal/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	dto "github.com/prometheus/client_model/go"
)

// counterValue extracts the current value of a labeled prometheus
// counter without depending on the optional testutil subpackage.
func counterValue(t *testing.T, tier, op, severity string) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, metrics.MemoryFailuresTotal.WithLabelValues(tier, op, severity).Write(&m))
	require.NotNil(t, m.Counter)
	return m.Counter.GetValue()
}

// TestMemoryFailuresTotal_LabelMatrix is the AUDIT-P2-4 contract test:
// the failures_total counter accepts every documented {tier, op,
// severity} tuple and increments independently. Catches typos (e.g.
// severity "critical" vs "fatal") at build time rather than at first
// incident.
func TestMemoryFailuresTotal_LabelMatrix(t *testing.T) {
	cases := []struct{ tier, op, severity string }{
		{"cold", "store", "critical"},
		{"hot", "store", "warn"},
		{"cold", "retrieve", "error"},
		{"hot", "retrieve", "warn"},
		{"cold", "list", "error"},
		{"hot", "list", "warn"},
		{"blackboard", "publish", "warn"},
		{"embedder", "embed", "warn"},
		{"embedder", "embed", "error"},
	}

	baseline := map[string]float64{}
	for _, c := range cases {
		key := c.tier + "/" + c.op + "/" + c.severity
		baseline[key] = counterValue(t, c.tier, c.op, c.severity)
	}

	for _, c := range cases {
		metrics.MemoryFailuresTotal.WithLabelValues(c.tier, c.op, c.severity).Inc()
	}

	for _, c := range cases {
		key := c.tier + "/" + c.op + "/" + c.severity
		got := counterValue(t, c.tier, c.op, c.severity)
		assert.Equal(t, baseline[key]+1, got, "label tuple %s should have incremented exactly once", key)
	}
}
