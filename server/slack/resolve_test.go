package slack

import (
	"context"
	"errors"
	"testing"
	"time"
)

// resolveFake feeds the directory caches without a network. The Poster
// methods are stubs that must never fire: Channels/home/ResolveDestination
// only type-assert the two list methods.
type resolveFake struct{ Poster }

func (f *resolveFake) ListChannels(context.Context) ([]Channel, error) {
	return []Channel{{ID: "C1ENG12345", Name: "eng-pod"}, {ID: "C1CI999999", Name: "circleci"}}, nil
}
func (f *resolveFake) ListUsers(context.Context) ([]User, error) {
	return []User{{ID: "U1MORGAN23", Handle: "morgan", RealName: "Morgan Kim"},
		{ID: "U1URGENT99", Handle: "urgent", RealName: "On Call"}}, nil
}

func TestResolveDestination(t *testing.T) {
	m := &Mirror{slack: &resolveFake{}, now: func() time.Time { return time.Unix(1754900000, 0) }}
	ctx := context.Background()
	for _, tc := range []struct{ kind, raw, want string }{
		{"dm", "", "dm"},
		{"channel", "#eng-pod", "C1ENG12345"},
		{"channel", "ENG-POD", "C1ENG12345"},    // case-insensitive name match
		{"channel", "C1PRIV1234", "C1PRIV1234"}, // raw id: the private-channel door
		// A lowercase name whose uppercase form is id-shaped is still a NAME.
		{"channel", "circleci", "C1CI999999"},
		{"person", "@morgan", "U1MORGAN23"},
		{"person", "morgan", "U1MORGAN23"},
		{"person", "U1OTHER123", "U1OTHER123"},
		// A handle that starts with "u" must not read as an id.
		{"person", "urgent", "U1URGENT99"},
	} {
		got, err := m.ResolveDestination(ctx, tc.kind, tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%s, %q) = %q, %v; want %q", tc.kind, tc.raw, got, err, tc.want)
		}
	}
	if _, err := m.ResolveDestination(ctx, "channel", "#nope"); !errors.Is(err, ErrNoSuchDestination) {
		t.Errorf("an unknown channel resolved: %v", err)
	}
	if _, err := m.ResolveDestination(ctx, "person", "@nobody"); !errors.Is(err, ErrNoSuchDestination) {
		t.Errorf("an unknown person resolved: %v", err)
	}
	if !ValidDestination("U1MORGAN23") || !ValidDestination("dm") || ValidDestination("bogus") {
		t.Error("ValidDestination does not accept the widened id set")
	}
}
