package event

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The anchor fields are the wire contract between the laptop agent and the
// server's derive layer: a hook copy and a transcript copy of one turn are
// recognised as the same thing only if both sides spell these keys the same
// way. Pinning the JSON tags here means a rename fails a test instead of
// silently splitting every turn into two rows again.
func TestAnchorFieldsRoundTripWithPinnedTags(t *testing.T) {
	exists := true
	in := Event{
		ID:               "abc",
		Source:           SourceClaudeCode,
		Origin:           OriginHook,
		Type:             AssistantTurn,
		SessionID:        "s1",
		Seq:              3,
		OccurredAt:       time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		PromptID:         "bb03a102-4ccd-40e9-b146-8a67e8db1ec1",
		ToolUseID:        "toolu_012HH5HfZ73a2TttRtX5xLQU",
		RecordUUID:       "6d6374f7-1111-2222-3333-444444444444",
		ParentRecordUUID: "9f90859f-aaaa-bbbb-cccc-dddddddddddd",
		LineageSource:    "fork_uuid",
		ForkPrefixSeq:    41,
		Launcher: &Launcher{
			Entrypoint: "cli",
			BundleID:   "dev.warp.Warp-Stable",
			Term:       "WarpTerminal",
			ParentComm: "zsh",
		},
		TranscriptExists: &exists,
		HarnessTitle:     "Create gantry fixture",
		Tool: &Tool{
			Name: "Edit",
			Diff: &Diff{Path: "/tmp/x", Before: "a", After: "b", Truncated: true},
		},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"prompt_id"`, `"tool_use_id"`, `"record_uuid"`, `"parent_record_uuid"`,
		`"lineage_source"`, `"fork_prefix_seq"`, `"launcher"`, `"transcript_exists"`,
		`"harness_title"`, `"entrypoint"`, `"bundle_id"`, `"term"`, `"parent_comm"`,
		`"diff":{"path":"/tmp/x","before":"a","after":"b","truncated":true}`,
	} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("encoded event lacks %s; the server keys on this exact tag:\n%s", key, b)
		}
	}

	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the event:\n in=%+v\nout=%+v", in, out)
	}
}

// Zero-valued anchors must vanish from the wire: every old event and every
// event from a harness that never carried these ids is encoded exactly as
// before, so the deterministic ids and the stored bodies of history are
// untouched by this change.
func TestAnchorFieldsAreOmittedWhenUnset(t *testing.T) {
	b, err := json.Marshal(Event{ID: "x", SessionID: "s", Type: UserPrompt, Source: SourceClaudeCode, Origin: OriginHook, OccurredAt: time.Unix(0, 0).UTC(),
		Launcher: &Launcher{}, Tool: &Tool{Name: "Edit", Diff: &Diff{Path: "/tmp/x"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"prompt_id", "tool_use_id", "record_uuid", "parent_record_uuid", "lineage_source", "fork_prefix_seq", "transcript_exists", "harness_title",
		"entrypoint", "bundle_id", "term", "parent_comm", "truncated"} {
		if strings.Contains(string(b), key) {
			t.Fatalf("unset anchor %q leaked onto the wire: %s", key, b)
		}
	}
}
