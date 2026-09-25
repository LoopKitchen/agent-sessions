package store

import "strings"

// Deployment is the part of the server's configuration the store needs to
// know: which email domains the deployment allows, and which pairs of those
// domains one Workspace serves as aliases of each other. Both come from the
// environment (ALLOWED_DOMAINS and DOMAIN_ALIASES, server/app/config.go) and
// are handed over once at boot through SetDeployment, so nothing in this
// package carries a default that names any particular organisation.
type Deployment struct {
	// AllowedDomains is the ALLOWED_DOMAINS list, lower-cased. A bound actor
	// on a laptop source token must sit in one of them (source_tokens.go).
	// Empty means no address qualifies, which is the safe reading of "not
	// configured".
	AllowedDomains []string
	// DomainAliases is the DOMAIN_ALIASES list: pairs of domains on which the
	// same local part is the same person. The export's viewer_emails reads it
	// (export_reads.go). Empty means an address is its own only viewer.
	DomainAliases []DomainAlias
}

// DomainAlias says that one Workspace serves both domains, so a person's
// sessions recorded under local@A are theirs to read as local@B and the other
// way round. It is a statement about accounts, not about spelling: two
// domains may be aliased only when the same local part cannot name two
// different people across them.
type DomainAlias struct {
	A, B string
}

// SetDeployment records the deployment's domains. Called once at boot, before
// the server serves, the way SetLogger is; it is not safe to race with reads.
func (s *Store) SetDeployment(d Deployment) {
	s.deployment = Deployment{
		AllowedDomains: cleanDomainList(d.AllowedDomains),
		DomainAliases:  cleanAliases(d.DomainAliases),
	}
}

func cleanDomainList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func cleanAliases(in []DomainAlias) []DomainAlias {
	out := make([]DomainAlias, 0, len(in))
	for _, a := range in {
		a.A = strings.ToLower(strings.TrimSpace(a.A))
		a.B = strings.ToLower(strings.TrimSpace(a.B))
		if a.A == "" || a.B == "" || a.A == a.B {
			continue
		}
		out = append(out, a)
	}
	return out
}
