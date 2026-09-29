package redisstreams

import "strings"

// matches reports whether a subject satisfies a subscription's pattern, with NATS' rules so that the
// two bus adapters accept the same patterns: tokens are separated by '.', `*` stands for exactly one
// token, and `>` — only as the last token — for one or more.
func matches(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	p, s := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, tok := range p {
		switch {
		case tok == ">" && i == len(p)-1:
			return len(s) > i
		case i >= len(s):
			return false
		case tok == "*":
		case tok != s[i]:
			return false
		}
	}
	return len(p) == len(s)
}
