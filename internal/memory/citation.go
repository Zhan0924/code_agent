package memory

import "regexp"

// MinMemoryScore is the decay floor; BoostScoreBatch must not push below it.
const MinMemoryScore = 0.01

// MaxMemoryScore caps reinforcement from citations and user feedback.
const MaxMemoryScore = 1.0

var memCitationRegex = regexp.MustCompile(`\[mem:([^\]]+)\]`)

// ParseCitationIDs extracts unique memory IDs cited as [mem:<id>] in text.
func ParseCitationIDs(text string) []string {
	matches := memCitationRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		id := m[1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// TouchRefsFromCitationIDs builds TouchRef slice for the given cited IDs.
func TouchRefsFromCitationIDs(userID, projectID string, ids []string) []TouchRef {
	refs := make([]TouchRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, TouchRef{UserID: userID, ProjectID: projectID, ID: id})
	}
	return refs
}

// ResolveCitedMemoryIDs prefers persisted cited_memory_ids on the message,
// falling back to regex parsing of content (REAUDIT-P1-4).
func ResolveCitedMemoryIDs(stored []string, content string) (ids []string, source string) {
	if len(stored) > 0 {
		return stored, "structured"
	}
	if ids = ParseCitationIDs(content); len(ids) > 0 {
		return ids, "regex_fallback"
	}
	return nil, "none"
}
