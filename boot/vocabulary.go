// Vocabulary assembly: the session log's known-event guard set is extended
// here, explicitly, for the whole static build. The Go port has no dynamic
// plugin loading — the assembly IS the load moment — so vocabulary
// membership is decided by this one function instead of being scattered
// across package init side effects. Every domain package exposes an
// idempotent RegisterEvents; re-assembly is safe.
package boot

import (
	"dshgo/agent"
	"dshgo/commands"
	"dshgo/compaction"
	"dshgo/hookprotocol"
	"dshgo/interaction/permissionpresets"
	"dshgo/interaction/userapproval"
	"dshgo/planmode"
	"dshgo/preset"
	"dshgo/schedule"
	"dshgo/sessionquery"
	"dshgo/subagent"
	"dshgo/todo"
)

// RegisterVocabulary extends the session vocabulary with every statically
// linked domain package's event types. Assemble calls it before any mount;
// a log carrying a type outside this set still fails closed.
func RegisterVocabulary() {
	agent.RegisterEvents()
	commands.RegisterEvents()
	compaction.RegisterEvents()
	hookprotocol.RegisterEvents()
	sessionquery.RegisterEvents()
	todo.RegisterEvents()
	userapproval.RegisterEvents()
	schedule.RegisterEvents()
	subagent.RegisterEvents()
	preset.RegisterEvents()
	permissionpresets.RegisterEvents()
	planmode.RegisterEvents()
}
