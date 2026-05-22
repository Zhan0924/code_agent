package orchestrator

import (
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/models"
)

const (
	compactionInterval     = 5
	compactionKeepRecent   = 3
	compactionMaxChars     = 2000
	compactionHeadChars    = 400
	compactionTailChars    = 400
)

// compactEarlyMessages compresses old tool_result messages in-place to free context budget.
// It keeps the most recent keepRecent tool results intact and truncates older ones.
func compactEarlyMessages(messages []models.Message, currentStep int) {
	if currentStep < compactionInterval {
		return
	}
	if currentStep%compactionInterval != 0 {
		return
	}

	toolResultCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == models.RoleTool {
			toolResultCount++
		}
	}

	if toolResultCount <= compactionKeepRecent {
		return
	}

	// Forward pass: compact old tool results
	compactUpTo := toolResultCount - compactionKeepRecent
	toolIdx := 0

	for i := range messages {
		if messages[i].Role != models.RoleTool {
			continue
		}
		toolIdx++
		if toolIdx > compactUpTo {
			break
		}

		content := messages[i].Content
		runes := []rune(content)
		if len(runes) <= compactionMaxChars {
			continue
		}

		// Already compacted
		if strings.Contains(content, "[compacted:") {
			continue
		}

		head := string(runes[:compactionHeadChars])
		tail := string(runes[len(runes)-compactionTailChars:])
		omitted := len(runes) - compactionHeadChars - compactionTailChars
		messages[i].Content = fmt.Sprintf("%s\n\n[compacted: %d chars omitted]\n\n%s", head, omitted, tail)
	}

}
