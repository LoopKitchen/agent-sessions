package derive

import "testing"

func TestCanonicalThreadJoinsBothSpellingsOfAnAgent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"a642e1e7c059e7d99", "642e1e7c059e7d99"},
		{"agent-general-purpose-642e1e7c059e7d99", "642e1e7c059e7d99"},
		{"agent-a642e1e7c059e7d99", "642e1e7c059e7d99"},
		{"agent-deadbeef-cafe-642e1e7c059e7d99", "642e1e7c059e7d99"},
		{"a0b0c46", "a0b0c46"},
		{"agent-afub-adversarial", "agent-afub-adversarial"},
		{"  agent-x  ", "agent-x"},
	} {
		if got := CanonicalThread(tc.in); got != tc.want {
			t.Errorf("CanonicalThread(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if CanonicalThread("a642e1e7c059e7d99") != CanonicalThread("agent-general-a642e1e7c059e7d99") {
		t.Error("the hook and walker spellings of one agent do not join")
	}
}
