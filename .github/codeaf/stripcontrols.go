package main

import (
	"strings"
	"unicode"
)

// stripControls removes every control character from s except newline and
// tab. Untrusted text (a refused PR's diff, an error line) must be safe to
// paste verbatim inside a comment fence; this is the neutralisation
// (refusedcomment.go). It moved here from the deleted search.go (issue #2),
// which used it for the same reason on printed search results.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
