package app

import "strings"

// testDomainsOf is the allowlist the test fixtures are built with, derived
// from the fixture addresses themselves so the list and the addresses cannot
// drift apart: a verifier or a sign-in built over it accepts exactly the
// domains the fixtures use.
func testDomainsOf(emails ...string) []string {
	out := make([]string, 0, len(emails))
	for _, e := range emails {
		out = append(out, e[strings.LastIndex(e, "@")+1:])
	}
	return out
}
