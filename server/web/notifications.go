package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Slack notification settings: the page where groups are made and the levers
// where they are pulled. The feature arrives through its own small port so a
// deployment without a Slack mirror simply has no page — the nav link, the
// routes and the session panel all key off the port being present.

// SlackGroup is one named destination as the pages show it.
type SlackGroup struct {
	ID          int64
	Name        string
	OwnerEmail  string
	Visibility  string
	Destination string
	Disabled    bool
}

// SlackSettingsView is the settings page's whole content.
type SlackSettingsView struct {
	Groups        []SlackGroup
	DefaultGroup  int64
	LiveDisabled  bool
	DigestMode    string
	DigestChannel string
	// DestLabels decorates destination ids with human spellings, best-effort.
	DestLabels map[string]string
}

// SlackSessionMirror is one active attachment on the session page.
type SlackSessionMirror struct {
	GroupID   int64
	GroupName string
	Dest      string
}

// ErrSlackConflict is a name collision on create; the page says so plainly.
var ErrSlackConflict = errors.New("web: a group with that name exists")

// SlackChannel is one workspace channel the destination picker offers.
type SlackChannel struct {
	ID   string
	Name string
}

// SlackUser is one workspace member the person picker offers.
type SlackUser struct {
	ID       string
	Handle   string
	RealName string
}

// SlackSettings is what the pages need from the mirror. Nil means the
// deployment has no mirror and the whole surface is absent.
type SlackSettings interface {
	Settings(ctx context.Context, email string) (SlackSettingsView, error)
	// Channels and Users feed the picker datalists; limit <= 0 means all,
	// because the browser prunes as the person types, not the server.
	Channels(ctx context.Context, prefix string, limit int) ([]SlackChannel, error)
	Users(ctx context.Context) ([]SlackUser, error)
	// ResolveDestination turns a picker submission (mode + what was typed or
	// chosen) into the stored id. Unknown names come back ErrNoSuchDestination.
	ResolveDestination(ctx context.Context, kind, raw string) (string, error)
	CreateGroup(ctx context.Context, email, name, visibility, destination string) error
	UpdateGroup(ctx context.Context, email string, g SlackGroup) error
	DeleteGroup(ctx context.Context, email string, id int64) error
	SetLiveKill(ctx context.Context, email string, disabled bool) error
	SessionMirrors(ctx context.Context, sessionID string) ([]SlackSessionMirror, error)
	Attach(ctx context.Context, email, sessionID, groupRef string) error
	Detach(ctx context.Context, email, sessionID, groupRef string) error
}

// ErrNoSuchDestination mirrors the slack package's sentinel through the port.
var ErrNoSuchDestination = errors.New("web: no such destination")

// notificationsView renders settings.html.
type notificationsView struct {
	Page     Page
	Settings SlackSettingsView
	// The whole directory, for the picker panels: combo.js prunes these as the
	// person types, which is what makes the picker feel like a picker.
	Channels []SlackChannel
	People   []SlackUser
	// What the create-group form should come back showing. The panel options
	// are links to this page carrying their own value, so that a browser with
	// the script blocked still picks by clicking: the click round-trips here
	// and the field arrives filled in. With the script running the click is
	// intercepted and none of these are ever set.
	DestMode    string
	ChannelPick string
	PersonPick  string
	// The rest of the form, carried across that round trip. Without them the
	// no-JS pick answered by clearing the card it was meant to fill in: the
	// name went blank and visibility fell back to private, which is a quiet
	// downgrade of what somebody had already chosen.
	Name       string
	Visibility string
}

// destPickURL addresses this page with one destination pre-chosen, carrying
// the rest of the create form with it. It is the href on every option in the
// two picker panels, and the reason those options are real links rather than
// script hooks.
//
// What it can carry is what the server put in it, which is the state of the
// last server response and nothing since. An anchor is fixed at render time,
// so text typed into the name box after the page loaded is not in the href and
// cannot be, on this pick or any later one: retyping the name and picking
// again loses it exactly the same way. What survives is state the server sent,
// which today means a create that failed to resolve its destination and came
// back with the card it was given. handleNotifGroupCreate is the only producer
// of these parameters for that reason.
//
// Closing the rest of the gap needs the pick to be a form submission rather
// than a link, which is a bigger change than the fallback warrants while the
// scripted path never round-trips at all.
func (v notificationsView) destPickURL(mode, field, value string) string {
	q := url.Values{}
	q.Set("dest_mode", mode)
	q.Set(field, value)
	if v.Name != "" {
		q.Set("name", v.Name)
	}
	if v.Visibility != "" {
		q.Set("visibility", v.Visibility)
	}
	return "/settings/notifications?" + q.Encode() + "#destination"
}

// ChannelPickURL and PersonPickURL spell the two modes the picker offers. The
// values carry their sigil because that is what the person sees in the box and
// what ResolveDestination parses back.
func (v notificationsView) ChannelPickURL(name string) string {
	return v.destPickURL("channel", "channel_pick", "#"+name)
}

func (v notificationsView) PersonPickURL(handle string) string {
	return v.destPickURL("person", "person_pick", "@"+handle)
}

// ModeIs reports which destination radio starts checked. An unrecognised or
// absent mode falls back to the DM, which is the one destination that needs no
// directory lookup and therefore cannot be wrong.
func (v notificationsView) ModeIs(mode string) bool {
	switch v.DestMode {
	case "channel", "person":
		return v.DestMode == mode
	}
	return mode == "dm"
}

// VisibilityIs reports which visibility pill starts checked. Anything the form
// does not offer falls back to private, so a mangled round trip narrows access
// rather than widening it.
func (v notificationsView) VisibilityIs(vis string) bool {
	if v.Visibility == "org" {
		return vis == "org"
	}
	return vis == "private"
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	sv, err := s.slack.Settings(r.Context(), v.Email)
	if err != nil {
		s.readError(w, r, v, err)
		return
	}
	view := notificationsView{Settings: sv}
	// Both directories load best-effort: a Slack hiccup leaves the raw-id
	// entry working, never a broken page.
	if chs, err := s.slack.Channels(r.Context(), "", 0); err != nil {
		s.log.Error("channel directory", "err", err)
	} else {
		view.Channels = chs
	}
	if us, err := s.slack.Users(r.Context()); err != nil {
		s.log.Error("people directory", "err", err)
	} else {
		view.People = us
	}
	// A pick that arrived by link rather than by script, plus the rest of the
	// card it was made on. Nothing here is trusted beyond being echoed into a
	// form field: the destination is resolved against the directory on POST,
	// exactly as a typed one is, and the name and visibility are validated by
	// the same create path that handles a form nobody round-tripped.
	q := r.URL.Query()
	view.DestMode = strings.TrimSpace(q.Get("dest_mode"))
	view.ChannelPick = strings.TrimSpace(q.Get("channel_pick"))
	view.PersonPick = strings.TrimSpace(q.Get("person_pick"))
	view.Name = strings.TrimSpace(q.Get("name"))
	view.Visibility = strings.TrimSpace(q.Get("visibility"))
	view.Page = s.page(v, "Notifications", "settings")
	view.Page.Notice, view.Page.Error = flashFrom(r)
	s.rnd.render(w, http.StatusOK, "settings.html", view)
}

// flashFrom carries one action's outcome across the redirect, in the URL
// rather than a cookie: the message is not secret and a cookie would need a
// second write path.
func flashFrom(r *http.Request) (notice, errMsg string) {
	q := r.URL.Query()
	return strings.TrimSpace(q.Get("ok")), strings.TrimSpace(q.Get("err"))
}

func (s *Server) handleNotifGroupCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(v, r.PostFormValue("csrf")) {
		http.Error(w, "stale form; reload and retry", http.StatusForbidden)
		return
	}
	kind := strings.TrimSpace(r.PostFormValue("dest_mode"))
	field, raw := "", ""
	switch kind {
	case "channel":
		field, raw = "channel_pick", r.PostFormValue("channel_pick")
	case "person":
		field, raw = "person_pick", r.PostFormValue("person_pick")
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	visibility := strings.TrimSpace(r.PostFormValue("visibility"))
	dest, err := s.slack.ResolveDestination(r.Context(), kind, raw)
	if err != nil {
		// Mistyping a destination is the one outcome that sends somebody back
		// to a card they have to fill in again, so it sends the card back with
		// them. Without this the page answered a misspelt channel by also
		// clearing the name they typed and dropping org visibility to private,
		// which is a second, quieter thing going wrong.
		//
		// This is also the only producer of the query state that
		// destPickURL and handleNotifications read. Nothing else needs to be:
		// a create that works clears the form on purpose.
		q := url.Values{}
		if kind != "" {
			q.Set("dest_mode", kind)
		}
		if field != "" && raw != "" {
			q.Set(field, raw)
		}
		if name != "" {
			q.Set("name", name)
		}
		if visibility != "" {
			q.Set("visibility", visibility)
		}
		back := "/settings/notifications"
		if len(q) > 0 {
			back += "?" + q.Encode()
		}
		redirectOutcome(w, r, back, "", err)
		return
	}
	err = s.slack.CreateGroup(r.Context(), v.Email, name, visibility, dest)
	redirectOutcome(w, r, "/settings/notifications", "group created", err)
}

func (s *Server) handleNotifGroupDelete(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(v, r.PostFormValue("csrf")) {
		http.Error(w, "stale form; reload and retry", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad group id", http.StatusBadRequest)
		return
	}
	err = s.slack.DeleteGroup(r.Context(), v.Email, id)
	redirectOutcome(w, r, "/settings/notifications", "group deleted", err)
}

func (s *Server) handleNotifGroupUpdate(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(v, r.PostFormValue("csrf")) {
		http.Error(w, "stale form; reload and retry", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad group id", http.StatusBadRequest)
		return
	}
	g := SlackGroup{
		ID:          id,
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Visibility:  strings.TrimSpace(r.PostFormValue("visibility")),
		Destination: strings.TrimSpace(r.PostFormValue("destination")),
		Disabled:    r.PostFormValue("disabled") == "on",
	}
	err = s.slack.UpdateGroup(r.Context(), v.Email, g)
	redirectOutcome(w, r, "/settings/notifications", "group saved", err)
}

func (s *Server) handleNotifControls(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(v, r.PostFormValue("csrf")) {
		http.Error(w, "stale form; reload and retry", http.StatusForbidden)
		return
	}
	// The toggle button IS the save: its value says which way to flip, and
	// clicking it submits. No checkbox-then-Save two-step.
	disabled := r.PostFormValue("live") == "pause"
	err := s.slack.SetLiveKill(r.Context(), v.Email, disabled)
	done := "live mirroring resumed"
	if disabled {
		done = "live mirroring paused"
	}
	redirectOutcome(w, r, "/settings/notifications", done, err)
}

func (s *Server) handleSessionMirrorForm(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.csrfValid(v, r.PostFormValue("csrf")) {
		http.Error(w, "stale form; reload and retry", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	group := strings.TrimSpace(r.PostFormValue("group"))
	var err error
	var done string
	if r.PostFormValue("action") == "detach" {
		err = s.slack.Detach(r.Context(), v.Email, id, group)
		done = "mirroring stopped"
	} else {
		err = s.slack.Attach(r.Context(), v.Email, id, group)
		done = "mirroring to " + group
	}
	redirectOutcome(w, r, "/sessions/"+id, done, err)
}

// redirectOutcome sends the browser back with one line about what happened.
//
// back may already carry a query, because the create path sends the submitted
// card along with its own failure, so the flash is appended rather than
// assumed to be the first parameter.
func redirectOutcome(w http.ResponseWriter, r *http.Request, back, ok string, err error) {
	sep := "?"
	if strings.Contains(back, "?") {
		sep = "&"
	}
	if err != nil {
		msg := "that did not work"
		if errors.Is(err, ErrSlackConflict) {
			msg = "a group with that name already exists"
		}
		if errors.Is(err, ErrNoSuchDestination) {
			// The resolver's message names the spelling that failed, which is
			// the one detail the person needs — without the sentinel prefix.
			msg = strings.TrimPrefix(err.Error(), ErrNoSuchDestination.Error()+": ")
		}
		http.Redirect(w, r, back+sep+"err="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+sep+"ok="+url.QueryEscape(ok), http.StatusSeeOther)
}
