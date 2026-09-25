package backfill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMetadataUsesFirstSessionRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := "{\"type\":\"event_msg\",\"payload\":{}}\n" +
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\"child\",\"source\":{\"subagent\":{\"thread_spawn\":{\"parent_thread_id\":\"root\"}}}}}\n" +
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\"root\",\"source\":\"cli\"}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	metadata, ok := readCodexMetadata(path)
	if !ok || metadata.session() != "child" || metadata.parent() != "root" {
		t.Fatalf("metadata=%+v ok=%v", metadata, ok)
	}
	if IsPrimaryCodex(path) {
		t.Fatal("subagent rollout counted as primary")
	}
}
