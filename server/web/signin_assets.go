package web

import _ "embed"

// The sign-in page's markup and script live beside the dashboard's other assets
// because Go's embed directive cannot reach outside its own package directory:
// keeping them next to the handler that serves them, in package app, would mean
// a second static-asset layout in a second place.
//
// Nothing in this package serves them, and that is deliberate. The sign-in page
// is the one response in this service that may run script, so it needs a
// Content-Security-Policy that is the opposite of Secure's — and a policy is
// only worth anything if one handler owns the whole response it applies to.
// Exposing the bytes rather than a handler keeps the relaxed policy in
// server/app/signin.go, scoped to that route, where a reviewer looking for it
// will find it beside the thing it protects.

//go:embed templates/signin.html
var signInPage string

//go:embed static/signin.js
var signInScript string

//go:embed templates/cli.html
var cliPage string

//go:embed static/cli.js
var cliScript string

// SignInPageTemplate is the html/template source for the sign-in page.
//
// It is not in the renderer's page set and extends no base template, because a
// signed-out browser must not be handed the dashboard's chrome: base.html reads
// a Viewer's name, admin flag and current search, none of which exist yet at the
// moment somebody is being asked to sign in.
func SignInPageTemplate() string { return signInPage }

// SignInScript is the module the sign-in page loads.
//
// It is returned as a string rather than as a []byte so that a caller cannot
// edit the embedded copy every later request would serve.
func SignInScript() string { return signInScript }

// CLIPageTemplate is the html/template source for the page the laptop agent
// opens in a browser.
//
// It is a separate document rather than the sign-in page under a flag because
// the two differ in the one thing worth being able to read off the page: where
// the ID token goes. The sign-in page carries a form that posts to this origin;
// this one carries no form at all and hands the token to a loopback listener.
// A single template branching on which it is would put both destinations in
// every rendering of both pages.
func CLIPageTemplate() string { return cliPage }

// CLIScript is the module that page loads.
func CLIScript() string { return cliScript }
