package node

import "github.com/lucataco/cloudex/go/internal/direct"

// The direct path (v0.2.3; packages/schema/src/node.ts, "the direct path").
// M1 has the messages only: the listener, the relay and the reports come in
// M2 (research/v0.2.3-plan.md). Until then the agent reports no direct fact,
// so the Worker sends clients no candidates, and directEnd finds nothing.

// directEndMax bounds a directEnd's list: the most sessions a Sandbox object
// keeps (direct.ts SESSIONS_KEPT).
const directEndMax = 4 * direct.SessionsMax

// endDirect closes the named direct sessions of a sandbox and refuses their
// tickets from now on; it answers how many it closed.
func (a *Agent) endDirect(d DirectEnd) int {
	return 0
}
