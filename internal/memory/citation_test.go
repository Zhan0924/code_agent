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

func TestResolveCitedMemoryIDs_StructuredPreferred(t *testing.T) {
	ids, source := ResolveCitedMemoryIDs([]string{"mem-structured"}, "plain answer")
	assert.Equal(t, []string{"mem-structured"}, ids)
	assert.Equal(t, "structured", source)
}

func TestResolveCitedMemoryIDs_RegexFallback(t *testing.T) {
	ids, source := ResolveCitedMemoryIDs(nil, "see [mem:mem-regex]")
	assert.Equal(t, []string{"mem-regex"}, ids)
	assert.Equal(t, "regex_fallback", source)
}
