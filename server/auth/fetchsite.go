package auth

import "net/http"

// SameOriginPOST reports whether a request is a POST the browser itself
// marks as coming from this origin: the Sec-Fetch-Site header is set by the
// browser, never by a page's script, so a cross-site form or fetch carrying
// the dashboard cookie arrives as "cross-site" (or without the header from
// a client that is not a browser) and a mutation gated on this is not
// forgeable from another site. Every cookie-authenticated POST that
// changes state (the derive rerun, the token routes) answers 403 when it
// is false. Only the exact same-origin value passes: "same-site" would
// admit a sibling subdomain, and "none" is a bookmark or a typed URL,
// neither of which submits a form.
func SameOriginPOST(r *http.Request) bool {
	return r.Method == http.MethodPost && r.Header.Get("Sec-Fetch-Site") == "same-origin"
}
