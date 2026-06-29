package memory

import (
	"sort"
)

func fuseRRF(hot, cold []Memory, limit int) []Memory {
	type entry struct {
		mem   Memory
		score float64
	}
	merged := make(map[string]*entry, len(hot)+len(cold))

	for i, m := range hot {
		e, ok := merged[m.ID]
		if !ok {
			e = &entry{mem: m}
			merged[m.ID] = e
		}
		e.score += 1.0/(rrfK+float64(i+1)) + hotBonus
	}
	for i, m := range cold {
		e, ok := merged[m.ID]
		if !ok {
			e = &entry{mem: m}
			merged[m.ID] = e
		}
		e.score += 1.0 / (rrfK + float64(i+1))
	}

	out := make([]entry, 0, len(merged))
	for _, e := range merged {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })

	if len(out) > limit {
		out = out[:limit]
	}
	result := make([]Memory, len(out))
	for i, e := range out {
		result[i] = e.mem
	}
	return result
}
