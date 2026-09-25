package backfill

import (
	"bufio"
	"encoding/json"
	"os"
)

type codexMetadata struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	ParentThreadID string `json:"parent_thread_id"`
	Timestamp      string `json:"timestamp"`
	Cwd            string `json:"cwd"`
	CLIVersion     string `json:"cli_version"`
	// Originator names the launcher; codex exec runs identify themselves here.
	Originator string `json:"originator"`
	Git        *struct {
		Branch string `json:"branch"`
	} `json:"git"`
	Source json.RawMessage `json:"source"`
}

func (m codexMetadata) session() string {
	if m.ID != "" {
		return m.ID
	}
	return m.SessionID
}

func (m codexMetadata) parent() string {
	if m.ParentThreadID != "" {
		return m.ParentThreadID
	}
	var source struct {
		Subagent *struct {
			ThreadSpawn *struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(m.Source, &source) != nil || source.Subagent == nil || source.Subagent.ThreadSpawn == nil {
		return ""
	}
	return source.Subagent.ThreadSpawn.ParentThreadID
}

func readCodexMetadata(path string) (codexMetadata, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexMetadata{}, false
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), maxLineBytes)
	for s.Scan() {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(s.Bytes(), &record) != nil || record.Type != "session_meta" {
			continue
		}
		var metadata codexMetadata
		if json.Unmarshal(record.Payload, &metadata) == nil && metadata.session() != "" {
			return metadata, true
		}
	}
	return codexMetadata{}, false
}

// IsPrimaryCodex reports whether path identifies a top-level Codex rollout.
func IsPrimaryCodex(path string) bool {
	metadata, ok := readCodexMetadata(path)
	return ok && metadata.parent() == ""
}
