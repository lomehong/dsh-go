package shell

import (
	"os"
	"path/filepath"

	"dshgo/agent"
	"dshgo/tools"
)

func homeEnv() string { return os.Getenv(DshHomeEnv) }

func homePathDefault() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dsh"
	}
	return filepath.Join(home, ".dsh")
}

// sessionIDOf extracts the calling agent's session header id.
func sessionIDOf(caller *agent.Agent) string {
	if caller == nil || caller.Session == nil {
		return ""
	}
	return caller.Session.Header().ID
}

var _ = tools.ToolExecution{}
