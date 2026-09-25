package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stub stands in for our enrollment endpoint and records what reached it.
type stub struct {
	srv    *httptest.Server
	status int
	body   any

	calls       int
	path        string
	contentType string
	got         map[string]string
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{
		status: http.StatusOK,
		body:   Result{Email: "dev@example.com", DeviceID: "d-1", DeviceToken: "loops_v1_token"},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.path = r.URL.Path
		s.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&s.got)
		if r.URL.Path != completePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		if s.body != nil {
			_ = json.NewEncoder(w).Encode(s.body)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// The token a signed-in page would post back. Distinctive so a substring search
// cannot match it by accident.
const testIDToken = "IDTOKEN-9c41ab07-signed-by-firebase"

// newFlow builds a flow whose "browser" is the given function. Open is called
// after the listener is serving and before Run waits, so a test can drive the
// page's half synchronously rather than racing a goroutine against it.
func newFlow(t *testing.T, endpoint string, open func(string) error) *Flow {
	t.Helper()
	f, err := New(Options{Endpoint: endpoint, Open: open, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// post sends one callback the way the sign-in page's script does.
func post(t *testing.T, f *Flow, origin, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.CallbackURL(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting the callback: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func callbackBody(state, idToken string) string {
	b, _ := json.Marshal(map[string]string{"id_token": idToken, "state": state})
	return string(b)
}

// signIn returns an Open that posts a well-formed callback, as the page does.
func signIn(t *testing.T, f **Flow, origin string) func(string) error {
	return func(string) error {
		res := post(t, *f, origin, "application/json", callbackBody((*f).state, testIDToken))
		if res.StatusCode != http.StatusOK {
			t.Errorf("the page's callback was refused: %d", res.StatusCode)
		}
		return nil
	}
}

// thisMachine is the laptop every test in this file is run on. DeviceID is empty
// because a first install has none, which is the case the flow has to work in.
func thisMachine() Machine {
	return Machine{Hostname: "mymac", OS: "darwin", Arch: "arm64", AgentVersion: "1.2.3"}
}

func run(t *testing.T, f *Flow) (Result, error) {
	t.Helper()
	return f.Run(context.Background(), thisMachine())
}

// Property: the whole flow. A page that signs in and posts the ID token back
// yields the identity and the device credential our server minted for it.
func TestASignedInPageYieldsTheIdentityAndCredential(t *testing.T) {
	srv := newStub(t)
	var f *Flow
	f = newFlow(t, srv.srv.URL, signIn(t, &f, srv.srv.URL))

	got, err := run(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "dev@example.com" || got.DeviceToken != "loops_v1_token" || got.DeviceID != "d-1" {
		t.Fatalf("result = %+v", got)
	}
}

// Property: the browser is sent to our own sign-in page carrying the port this
// process is listening on and the state that binds the callback to this run.
// There is no OAuth client and no Google URL any more, so a redirect_uri, a
// client_id or a PKCE challenge appearing here would mean a flow nobody can
// complete.
func TestTheSignInURLNamesOurOwnPageWithThePortAndState(t *testing.T) {
	f := newFlow(t, "https://sessions.example.test/", func(string) error { return nil })

	u, err := url.Parse(f.SignInURL())
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "sessions.example.test" {
		t.Errorf("sign-in URL host = %s://%s, want our own endpoint", u.Scheme, u.Host)
	}
	if u.Path != signInPath {
		t.Errorf("sign-in path = %q, want %q", u.Path, signInPath)
	}
	q := u.Query()
	if q.Get("state") != f.state {
		t.Errorf("state = %q, want the flow's own %q", q.Get("state"), f.state)
	}
	// The port the page is told about has to be the one actually bound, or the
	// token is posted at whatever else is listening there.
	if want := "http://127.0.0.1:" + q.Get("port") + callbackPath; f.CallbackURL() != want {
		t.Errorf("port %q names %q, but the listener is at %q", q.Get("port"), want, f.CallbackURL())
	}
	for _, dead := range []string{"client_id", "redirect_uri", "code_challenge", "scope", "response_type"} {
		if q.Has(dead) {
			t.Errorf("the sign-in URL still carries the OAuth parameter %q", dead)
		}
	}
	if strings.Contains(f.SignInURL(), "accounts.google.com") {
		t.Error("the browser is still being sent to Google's OAuth endpoint")
	}
}

// Property: the listener is the 127.0.0.1 literal. RFC 8252 §8.3: "localhost"
// can resolve to an address off the loopback interface, which would put a live
// ID token on whatever network this laptop is attached to.
func TestTheListenerBindsTheLoopbackLiteral(t *testing.T) {
	f := newFlow(t, "https://x.test", func(string) error { return nil })
	if !strings.HasPrefix(f.CallbackURL(), "http://127.0.0.1:") {
		t.Fatalf("callback = %q, want the loopback IP literal", f.CallbackURL())
	}
	if strings.Contains(f.CallbackURL(), "localhost") {
		t.Fatal("localhost can resolve off-loopback; bind the literal")
	}
}

// Property: a callback whose state does not match is refused, and refusing it
// does not end the flow. Any process on this laptop can reach the port, so
// treating the first bad post as fatal would let one of them cancel somebody's
// enrollment; and the comparison is whole-value, so a state that is a prefix or
// an extension of the real one is refused like any other.
func TestACallbackWithTheWrongStateIsRefusedAndDoesNotEndTheFlow(t *testing.T) {
	srv := newStub(t)
	var f *Flow
	var refused []int
	f = newFlow(t, srv.srv.URL, func(string) error {
		wrong := []string{
			"",
			"attacker-chosen",
			f.state[:len(f.state)-1],
			f.state + "x",
			strings.ToUpper(f.state),
		}
		for _, s := range wrong {
			res := post(t, f, srv.srv.URL, "application/json", callbackBody(s, "IDTOKEN-attacker"))
			refused = append(refused, res.StatusCode)
		}
		// The real page's callback still completes afterwards.
		res := post(t, f, srv.srv.URL, "application/json", callbackBody(f.state, testIDToken))
		if res.StatusCode != http.StatusOK {
			t.Errorf("the real callback was refused after the bad ones: %d", res.StatusCode)
		}
		return nil
	})

	got, err := run(t, f)
	if err != nil {
		t.Fatalf("the flow did not survive the refused callbacks: %v", err)
	}
	for i, status := range refused {
		if status != http.StatusForbidden {
			t.Errorf("wrong state %d answered %d, want 403", i, status)
		}
	}
	if got.DeviceToken == "" {
		t.Fatal("no credential")
	}
	if srv.got["id_token"] != testIDToken {
		t.Errorf("the server was sent %q, want the token from the callback whose state matched",
			srv.got["id_token"])
	}
}

// Property: only our own sign-in page may post here. A browser sends Origin on
// every cross-origin request, so a page on somebody else's site that finds this
// port cannot deliver a token of its choosing.
func TestACallbackFromAnotherOriginIsRefused(t *testing.T) {
	srv := newStub(t)
	var f *Flow
	var res *http.Response
	f = newFlow(t, srv.srv.URL, func(string) error {
		res = post(t, f, "https://evil.example", "application/json", callbackBody(f.state, testIDToken))
		return nil
	})
	f.opts.Timeout = 200 * time.Millisecond

	if _, err := run(t, f); err == nil {
		t.Fatal("a callback from another origin enrolled a device")
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want nothing for a foreign origin", got)
	}
	if srv.calls != 0 {
		t.Error("a foreign origin's token was forwarded to the server")
	}
}

// Property: the preflight that makes a JSON post possible is answered for our
// own page and for nobody else. This is what the Content-Type rule below leans
// on: JSON is not a CORS-safelisted type, so the browser asks first.
func TestThePreflightIsAnsweredOnlyForOurOwnPage(t *testing.T) {
	srv := newStub(t)
	var f *Flow
	var ours, theirs *http.Response
	f = newFlow(t, srv.srv.URL, func(string) error {
		ours = preflight(t, f, srv.srv.URL)
		theirs = preflight(t, f, "https://evil.example")
		return nil
	})
	f.opts.Timeout = 200 * time.Millisecond

	if _, err := run(t, f); err == nil {
		t.Fatal("a preflight alone completed the flow")
	}
	if ours.StatusCode != http.StatusNoContent {
		t.Errorf("our own preflight = %d, want 204", ours.StatusCode)
	}
	if got := ours.Header.Get("Access-Control-Allow-Origin"); got != srv.srv.URL {
		t.Errorf("Access-Control-Allow-Origin = %q, want exactly %q and never a wildcard", got, srv.srv.URL)
	}
	if got := ours.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "content-type") {
		t.Errorf("Access-Control-Allow-Headers = %q, so the page cannot send JSON", got)
	}
	if got := ours.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Access-Control-Allow-Methods = %q, so the page cannot post", got)
	}
	// Chrome asks the local side to agree before a public page may reach a
	// loopback address. Without this the flow fails in one browser only, at the
	// last step, and looks like a hung installer.
	if got := ours.Header.Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Errorf("Access-Control-Allow-Private-Network = %q, want true", got)
	}
	if theirs.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign preflight = %d, want 403", theirs.StatusCode)
	}
	if got := theirs.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a foreign preflight was allowed: %q", got)
	}
}

func preflight(t *testing.T, f *Flow, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, f.CallbackURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// Property: the callback must be JSON. A form post and a text/plain post are
// the two shapes a browser will send cross-origin without asking permission
// first, so refusing them is what makes the preflight rule above load-bearing
// rather than decorative.
func TestACallbackThatIsNotJSONIsRefused(t *testing.T) {
	cases := map[string]string{
		"a form post":        "application/x-www-form-urlencoded",
		"plain text":         "text/plain;charset=UTF-8",
		"multipart":          "multipart/form-data; boundary=x",
		"nothing at all":     "",
		"json by name only":  "application/json-patch+json",
		"json in the params": "text/plain; type=application/json",
	}
	for name, contentType := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStub(t)
			var f *Flow
			var res *http.Response
			f = newFlow(t, srv.srv.URL, func(string) error {
				res = post(t, f, srv.srv.URL, contentType, callbackBody(f.state, testIDToken))
				return nil
			})
			f.opts.Timeout = 200 * time.Millisecond

			if _, err := run(t, f); err == nil {
				t.Fatal("a non-JSON callback enrolled a device")
			}
			if res.StatusCode != http.StatusUnsupportedMediaType {
				t.Errorf("status = %d, want 415", res.StatusCode)
			}
			if srv.calls != 0 {
				t.Error("the token was forwarded to the server")
			}
		})
	}
}

// Property: a callback carrying no token, or a body too large to be one, is
// refused rather than treated as a sign-in.
func TestACallbackWithoutAUsableTokenIsRefused(t *testing.T) {
	cases := map[string]struct {
		body   func(state string) string
		status int
	}{
		"no token at all": {
			body:   func(state string) string { return callbackBody(state, "") },
			status: http.StatusBadRequest,
		},
		"not JSON at all": {
			body:   func(string) string { return "{" },
			status: http.StatusBadRequest,
		},
		"a body far larger than a token": {
			// Truncated by the reader's cap mid-token, so it can only decode as
			// malformed: the point is that it is never buffered whole.
			body: func(state string) string {
				return callbackBody(state, strings.Repeat("A", maxCallbackBytes*2))
			},
			status: http.StatusBadRequest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStub(t)
			var f *Flow
			var res *http.Response
			f = newFlow(t, srv.srv.URL, func(string) error {
				res = post(t, f, srv.srv.URL, "application/json", tc.body(f.state))
				return nil
			})
			f.opts.Timeout = 200 * time.Millisecond

			if _, err := run(t, f); err == nil {
				t.Fatal("the flow completed on a callback carrying no usable token")
			}
			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if srv.calls != 0 {
				t.Error("the server was called anyway")
			}
		})
	}
}

// Property: this process serves exactly one path. Anything else on the port is
// a 404, so a curious scan finds no surface to work with.
func TestOnlyTheCallbackPathIsServed(t *testing.T) {
	srv := newStub(t)
	var f *Flow
	var codes []int
	f = newFlow(t, srv.srv.URL, func(string) error {
		base := strings.TrimSuffix(f.CallbackURL(), callbackPath)
		for _, path := range []string{"/", "/debug/pprof/", "/callback/../callback", "/Callback"} {
			res, err := http.Get(base + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = res.Body.Close()
			codes = append(codes, res.StatusCode)
		}
		// A GET on the right path is not a callback either.
		res, err := http.Get(f.CallbackURL())
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		codes = append(codes, res.StatusCode)
		return nil
	})
	f.opts.Timeout = 200 * time.Millisecond

	if _, err := run(t, f); err == nil {
		t.Fatal("a GET completed the flow")
	}
	for i, code := range codes {
		if code == http.StatusOK {
			t.Errorf("request %d answered 200", i)
		}
	}
}

// Property: what reaches our server is the ID token from the callback plus the
// facts that let an admin revoke this one machine. A device row with no
// hostname is a row nobody can act on.
func TestTheExchangeSendsTheIDTokenAndTheDeviceFacts(t *testing.T) {
	srv := newStub(t)
	var f *Flow
	f = newFlow(t, srv.srv.URL, signIn(t, &f, srv.srv.URL))

	if _, err := run(t, f); err != nil {
		t.Fatal(err)
	}
	if srv.path != completePath {
		t.Errorf("posted to %q, want %q", srv.path, completePath)
	}
	if srv.contentType != "application/json" {
		t.Errorf("Content-Type = %q", srv.contentType)
	}
	want := map[string]string{
		"id_token":      testIDToken,
		"hostname":      "mymac",
		"os":            "darwin",
		"arch":          "arm64",
		"agent_version": "1.2.3",
	}
	for k, v := range want {
		if srv.got[k] != v {
			t.Errorf("%s = %q, want %q", k, srv.got[k], v)
		}
	}
	// The fields of the OAuth exchange are gone. Sending them would mean the
	// client and the server disagree about which flow this is.
	for _, dead := range []string{"code", "code_verifier", "redirect_uri"} {
		if _, ok := srv.got[dead]; ok {
			t.Errorf("the request still carries %q", dead)
		}
	}
}

// Property: a laptop that already has an id says so, and one that does not says
// nothing at all. The id is how the server recognises a machine it has already
// enrolled instead of minting a second row for it; the absent case is a first
// install and a purged config, and it has to look to the server exactly like the
// request an older binary makes, which carries no such field.
func TestTheExchangeCarriesTheDeviceIDOnlyWhenTheLaptopHasOne(t *testing.T) {
	cases := map[string]struct {
		deviceID string
		want     string
		present  bool
	}{
		"a machine that enrolled before names its row": {
			deviceID: "11111111-2222-4333-8444-555555555555",
			want:     "11111111-2222-4333-8444-555555555555",
			present:  true,
		},
		"a first install claims nothing": {deviceID: "", present: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStub(t)
			var f *Flow
			f = newFlow(t, srv.srv.URL, signIn(t, &f, srv.srv.URL))

			m := thisMachine()
			m.DeviceID = tc.deviceID
			if _, err := f.Run(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			got, ok := srv.got["device_id"]
			if ok != tc.present {
				t.Fatalf("device_id present = %v, want %v (got %q)", ok, tc.present, got)
			}
			if got != tc.want {
				t.Errorf("device_id = %q, want %q", got, tc.want)
			}
		})
	}
}

// Property: the most likely real failure — signing in with a personal account —
// is reported as something the person can act on rather than as a status code.
func TestAnUnauthorisedAccountGetsAnActionableMessage(t *testing.T) {
	srv := newStub(t)
	srv.status, srv.body = http.StatusForbidden, map[string]string{"error": "not authorised"}
	var f *Flow
	f = newFlow(t, srv.srv.URL, signIn(t, &f, srv.srv.URL))

	_, err := run(t, f)
	if err == nil {
		t.Fatal("expected a rejection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "allowed domains") || !strings.Contains(msg, "admin") {
		t.Fatalf("message should tell the person what to do, got: %v", msg)
	}
	if strings.Contains(msg, "403") {
		t.Fatalf("message should not surface a raw status code: %v", msg)
	}
}

// Property: a server failure, and a response that parses but carries no
// credential, both fail loudly. The credential is returned exactly once and is
// stored only as a hash, so persisting an empty one would strand the machine
// with every later upload failing for no visible reason.
func TestAFailedExchangeIsNeverMistakenForSuccess(t *testing.T) {
	cases := map[string]struct {
		status int
		body   any
		says   string
	}{
		"the server is failing":      {http.StatusInternalServerError, nil, ""},
		"the server is unavailable":  {http.StatusServiceUnavailable, nil, ""},
		"no credential in the reply": {http.StatusOK, map[string]string{"email": "dev@example.com"}, "credential"},
		"no identity in the reply":   {http.StatusOK, map[string]string{"device_token": "t"}, "credential"},
		"a reply that is not JSON":   {http.StatusOK, "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStub(t)
			srv.status, srv.body = tc.status, tc.body
			var f *Flow
			f = newFlow(t, srv.srv.URL, signIn(t, &f, srv.srv.URL))

			got, err := run(t, f)
			if err == nil {
				t.Fatalf("a failure was reported as success: %+v", got)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error should say what was missing: %v", err)
			}
		})
	}
}

// Property: somebody who wanders off does not leave a listener bound, and
// nothing is sent to the server while nobody has signed in.
func TestATimeoutDoesNotHangForever(t *testing.T) {
	srv := newStub(t)
	f, err := New(Options{
		Endpoint: srv.srv.URL,
		Open:     func(string) error { return nil },
		Timeout:  120 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := run(t, f); err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %v; a person who wanders off must not leave a listener bound", elapsed)
	}
	if srv.calls != 0 {
		t.Error("something was sent to the server without a sign-in")
	}
	// The port is released, so re-running the installer does not accumulate
	// listeners on a laptop.
	if _, err := http.Get(f.CallbackURL()); err == nil {
		t.Error("the listener is still bound after the flow gave up")
	}
}

// Property: a machine with no browser, or a remote shell, is still usable —
// the URL to open is in the error rather than lost inside it.
func TestABrowserThatCannotBeOpenedPrintsTheURL(t *testing.T) {
	srv := newStub(t)
	f := newFlow(t, srv.srv.URL, func(string) error { return errors.New("no browser") })

	_, err := run(t, f)
	if err == nil {
		t.Fatal("expected an error when the browser cannot be opened")
	}
	if !strings.Contains(err.Error(), f.SignInURL()) {
		t.Fatalf("the fallback must include the URL to open manually: %v", err)
	}
}

// Property: two runs share nothing. A state reused between them would let a
// callback meant for one complete the other.
func TestEachFlowGetsAFreshStateAndPort(t *testing.T) {
	a := newFlow(t, "https://x.test", func(string) error { return nil })
	b := newFlow(t, "https://x.test", func(string) error { return nil })

	if a.state == b.state {
		t.Fatal("two flows shared a state; replay between them would be possible")
	}
	if a.CallbackURL() == b.CallbackURL() {
		t.Fatal("two concurrent flows bound the same port")
	}
	// 32 bytes of base64url. A short state is guessable by another process on
	// this machine, which is the whole thing it defends against.
	if len(a.state) < 40 {
		t.Errorf("state is %d characters, want the full 32 bytes", len(a.state))
	}
}

// Property: an endpoint this flow cannot post to is refused at construction,
// before a port is bound and before a browser opens. Everything the flow does
// derives from it — the page, the permitted origin, the exchange — so a bad one
// is not something to discover halfway through a sign-in.
func TestNewRefusesAnEndpointItCannotUse(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"blank":             "   ",
		"no scheme":         "sessions.example.test",
		"protocol relative": "//sessions.example.test",
		"not http":          "ftp://sessions.example.test",
		"a path only":       "/auth/cli",
		"scheme only":       "https://",
	}
	for name, endpoint := range cases {
		t.Run(name, func(t *testing.T) {
			if f, err := New(Options{Endpoint: endpoint}); err == nil {
				_ = f.Close()
				t.Fatalf("New accepted %q", endpoint)
			}
		})
	}
	f, err := New(Options{Endpoint: "https://sessions.example.test/"})
	if err != nil {
		t.Fatalf("a usable endpoint was refused: %v", err)
	}
	defer f.Close()
	// The trailing slash is absorbed rather than producing a double slash in
	// every URL this flow builds.
	if strings.Contains(f.SignInURL(), "//auth") {
		t.Errorf("sign-in URL = %q", f.SignInURL())
	}
}

// Property: the Content-Type check reads the media type and ignores parameters,
// because a browser's fetch() appends a charset of its own accord.
func TestIsJSON(t *testing.T) {
	cases := map[string]bool{
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"  Application/JSON  ;charset=utf8": true,
		"application/json-patch+json":       false,
		"text/plain":                        false,
		"text/plain; type=application/json": false,
		"":                                  false,
		"application/x-www-form-urlencoded": false,
	}
	for header, want := range cases {
		if got := isJSON(header); got != want {
			t.Errorf("isJSON(%q) = %v, want %v", header, got, want)
		}
	}
}
