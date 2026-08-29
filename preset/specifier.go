// How one composition row's `name` reaches a module.
//
// A preset composition is read by the composition loader relative to the
// preset's own directory. That is right for a row naming a file the preset
// ships and wrong for a row naming a package: a locally authored preset
// lives under the user's home, where an upward `node_modules` walk never
// reaches the harness's own dependencies. Both the mount's import override
// and discovery's health check therefore have to classify a row's name
// before they can act on it, and they must classify it the same way — a row
// discovery resolves from one base and the mount imports from another would
// be reported healthy and then fail to load.
package preset

import "path/filepath"

// Row specifier kinds, mirroring the official RowSpecifier union.
const (
	// SpecifierBuiltin: a `cordis:` builtin the Loader supplies; nothing is
	// resolved.
	SpecifierBuiltin = "builtin"
	// SpecifierPreset: a path relative to the preset's own directory; the
	// preset ships the file.
	SpecifierPreset = "preset"
	// SpecifierFile: an absolute path or `file:` URL; it names one file and
	// no base.
	SpecifierFile = "file"
	// SpecifierPackage: a package name resolved from the installed harness.
	SpecifierPackage = "package"
)

// RowSpecifier is one composition row's module specifier, classified by
// where it resolves.
type RowSpecifier struct {
	// Kind decides which base the specifier resolves against.
	Kind string
	// Specifier is the string to hand a resolver.
	Specifier string
}

// ClassifyRowSpecifier classifies one row's `name`, exactly as the row
// wrote it.
//
// Go deviation from the official TypeScript: an absolute filesystem path
// stays a path instead of becoming a `file:` URL — Node's ESM resolver
// needs the URL spelling for Windows drive-letter paths, Go's os/stat does
// not — so Kind is the only routing fact either consumer may rely on, and
// Specifier is whatever the platform resolver takes.
func ClassifyRowSpecifier(name string) RowSpecifier {
	switch {
	case len(name) >= len("cordis:") && name[:len("cordis:")] == "cordis:":
		return RowSpecifier{Kind: SpecifierBuiltin, Specifier: name}
	case len(name) > 0 && name[0] == '.':
		return RowSpecifier{Kind: SpecifierPreset, Specifier: name}
	case hasFileURLPrefix(name):
		return RowSpecifier{Kind: SpecifierFile, Specifier: name}
	case filepath.IsAbs(name):
		return RowSpecifier{Kind: SpecifierFile, Specifier: name}
	default:
		return RowSpecifier{Kind: SpecifierPackage, Specifier: name}
	}
}

func hasFileURLPrefix(name string) bool {
	prefix := "file:"
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}
