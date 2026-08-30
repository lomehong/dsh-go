package subprocess

import (
	"os"
	"regexp"
	"runtime"
	"strings"
)

// sensitiveEnvPattern matches credential-shaped environment names: they are
// NOT forwarded to children (the harness's own secrets must not leak into a
// spawned process implicitly). A deliberately supplied entry survives
// because explicit env layers merge after the scrub.
var sensitiveEnvPattern = regexp.MustCompile(`KEY|PASSWORD|SECRET|TOKEN`)

// ScrubbedParentEnv returns the ambient parent environment minus
// credential-shaped names and minus all DSH_* names (both case-insensitive:
// Windows environment names are case-insensitive, so a parent `dsh_*` entry
// would otherwise survive and read back as `$env:DSH_*` in the child) — the
// canonical base every harness child starts from. PATH, HOME, locale, and
// proxy variables survive, so child CLIs run normally; harness identity
// never leaks implicitly.
func ScrubbedParentEnv() map[string]string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, DshEnvPrefix) || sensitiveEnvPattern.MatchString(upper) {
			continue
		}
		env[key] = value
	}
	return env
}

// ChildEnv merges the explicit caller entries onto the scrubbed parent base
// using the target platform's environment-key semantics. A non-nil value
// deliberately restores or overrides an entry; a nil tombstone removes an
// ordinary ambient entry.
func ChildEnv(extra map[string]*string) map[string]string {
	env := ScrubbedParentEnv()
	if !isWindows() {
		for key, value := range extra {
			if value == nil {
				delete(env, key)
				continue
			}
			env[key] = *value
		}
		return env
	}
	// Windows keys are case-insensitive: an explicit entry replaces every
	// inherited spelling of the same key.
	for key, value := range extra {
		normalized := strings.ToUpper(key)
		for inherited := range env {
			if strings.ToUpper(inherited) == normalized {
				delete(env, inherited)
			}
		}
		if value != nil {
			env[key] = *value
		}
	}
	return env
}

func isWindows() bool { return runtime.GOOS == "windows" }
