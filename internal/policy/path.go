// Package policy contains shared operator authorization rules.
package policy

import (
	"path"
	"strings"
)

// Matches treats a literal directory as a subtree and ** as zero or more
// complete path segments. Both admission and delivery use this exact rule.
func Matches(pattern, file string) bool {
	if pattern == "" || file == "" {
		return false
	}
	if pattern == file || strings.HasPrefix(file, strings.TrimSuffix(pattern, "/")+"/") {
		return true
	}
	var match func([]string, []string) bool
	match = func(p, f []string) bool {
		if len(p) == 0 {
			return len(f) == 0
		}
		if p[0] == "**" {
			return match(p[1:], f) || (len(f) > 0 && match(p, f[1:]))
		}
		if len(f) == 0 {
			return false
		}
		ok, _ := path.Match(p[0], f[0])
		return ok && match(p[1:], f[1:])
	}
	return match(strings.Split(pattern, "/"), strings.Split(file, "/"))
}
