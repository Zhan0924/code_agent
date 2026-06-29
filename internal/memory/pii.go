package memory

import (
	"os"
	"regexp"
	"strings"
)

// PIIMasker scrubs common secret formats before content reaches the LLM
// (defence-in-depth) and before being persisted as a long-term memory
// (compliance — once stored, this content may be re-injected into prompts
// of future sessions, possibly across organisational boundaries).
//
// Conservative on purpose: we'd rather over-mask (and lose a bit of
// extraction quality) than persist a token. False positives are visible
// (the user sees [REDACTED:*]); silent leaks are not.
type PIIMasker struct {
	patterns []*regexp.Regexp
	labels   []string
}

// NewPIIMasker creates a new PIIMasker with built-in rules and optional
// custom rules defined in the AGENT_CUSTOM_PII_REGEX environment variable
// (format: LABEL1=regex1,LABEL2=regex2).
func NewPIIMasker() *PIIMasker {
	type entry struct {
		label string
		re    string
	}
	specs := []entry{
		// AWS access key + secret
		{"AWS_KEY", `\bAKIA[0-9A-Z]{16}\b`},
		// Generic high-entropy long token (hex/base64) — 32+ chars
		{"TOKEN", `\b[A-Fa-f0-9]{32,}\b`},
		{"TOKEN", `\b[A-Za-z0-9+/]{40,}={0,2}\b`},
		// JWT (3 dot-separated base64 segments)
		{"JWT", `\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`},
		// OpenAI-style sk- key (also catches sk-ant- etc.)
		{"API_KEY", `\bsk-[A-Za-z0-9_\-]{20,}\b`},
		// "<key_name>=<value>" assignments commonly leaking secrets
		{"SECRET", `(?i)\b(api[_-]?key|secret|password|passwd|token|access[_-]?key)\s*[:=]\s*['"]?[A-Za-z0-9_\-\.+/=]{8,}['"]?`},
		// Bearer tokens
		{"BEARER", `(?i)bearer\s+[A-Za-z0-9_\-\.+/=]{8,}`},
		// Private key blocks
		{"PRIVATE_KEY", `-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+PRIVATE KEY-----`},
		// Email addresses (mask but keep domain hint)
		{"EMAIL", `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`},
		// IPv4 addresses
		{"IPV4", `\b(?:\d{1,3}\.){3}\d{1,3}\b`},
	}

	// Support custom business secrets / tokens injected via env var
	customEnv := os.Getenv("AGENT_CUSTOM_PII_REGEX")
	if customEnv != "" {
		parts := strings.Split(customEnv, ",")
		for _, part := range parts {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				specs = append(specs, entry{label: strings.TrimSpace(kv[0]), re: strings.TrimSpace(kv[1])})
			}
		}
	}

	m := &PIIMasker{}
	for _, s := range specs {
		re, err := regexp.Compile(s.re)
		if err == nil {
			m.patterns = append(m.patterns, re)
			m.labels = append(m.labels, s.label)
		}
	}
	return m
}

func (m *PIIMasker) Mask(s string) string {
	if s == "" || m == nil {
		return s
	}
	out := s
	for i, re := range m.patterns {
		label := m.labels[i]
		out = re.ReplaceAllString(out, "[REDACTED:"+label+"]")
	}
	return out
}
