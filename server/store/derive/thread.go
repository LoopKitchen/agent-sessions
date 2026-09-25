package derive

import (
	"regexp"
	"strings"
)

// agentCoreRE extracts the sixteen-hex identity both capture paths embed:
// hook ids are a<16hex>, transcript ids end in the same sixteen hex after a
// filename slug. It is the rule server/web/transcript.go applies when it
// partitions a page into threads, spelled the same so the two cannot
// disagree about which rows are one subagent.
var agentCoreRE = regexp.MustCompile(`^a([0-9a-f]{16})$|([0-9a-f]{16})$`)

// CanonicalThread names the thread an event belongs to: "" for the main
// conversation, otherwise the shared core of the agent id.
//
// Live hook capture names a subagent a<16hex> while the transcript walker
// derives agent-<slug>-<16hex> from its file name, so a session captured
// both ways stores one subagent under two spellings. The sixteen hex digits
// are the same on both paths and are the join; an agent id with no such core
// is used as it is, because a wrong join is worse than none.
func CanonicalThread(agentID string) string {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return ""
	}
	if m := agentCoreRE.FindStringSubmatch(agentID); m != nil {
		if m[1] != "" {
			return m[1]
		}
		if m[2] != "" {
			return m[2]
		}
	}
	return agentID
}
