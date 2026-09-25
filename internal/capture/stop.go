package capture

import (
	"encoding/json"
	"strings"
)

// stopResponse uses the Stop payload, never the transcript, for visible text.
// Claude documents that the final message may not have reached the transcript
// when Stop fires: https://code.claude.com/docs/en/hooks#stop-input.
// An absent, empty, or unsupported answer must not revive a previous turn.
// Older message payloads contribute visible text only from assistant content;
// their scrubbed original payload remains archived separately in Raw.
func stopResponse(h HookEvent) string {
	var text string
	if len(h.LastAssistantMessage) != 0 {
		_ = json.Unmarshal(h.LastAssistantMessage, &text)
		return text
	}
	if json.Unmarshal(h.Message, &text) == nil {
		return text
	}
	var message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(h.Message, &message) != nil || message.Role != "assistant" {
		return ""
	}
	if json.Unmarshal(message.Content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(message.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "tool_use" || block.Type == "tool_result" {
			return ""
		}
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
