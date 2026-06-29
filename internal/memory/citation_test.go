package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCitationIDs_DedupAndExtract(t *testing.T) {
	text := "Used [mem:abc-123] and again [mem:abc-123] plus [mem:xyz]"
	assert.Equal(t, []string{"abc-123", "xyz"}, ParseCitationIDs(text))
}

func TestParseCitationIDs_Empty(t *testing.T) {
	assert.Nil(t, ParseCitationIDs("no citations here"))
}
