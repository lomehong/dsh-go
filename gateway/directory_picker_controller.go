package gateway

import (
	"context"

	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"dshgo/typert"
)

// directoryPickerUnavailable is the diagnostic answered while no
// directory-picking backend is composed in the Go web profile (official
// dsh-host-directory-picker rows: T3-planned, see DECISIONS).
const directoryPickerUnavailable = "directoryPicker: no directory-picking backend is composed in this deployment"

// maxPickerEntries bounds one listing level (official browse default 1000).
const maxPickerEntries = 1000

// DirectoryPickerController hosts the directoryPicker Remote namespace
// (official DirectoryPickerController): the in-app browser listing and
// child-directory creation over the host filesystem. The OS chooser (pick)
// needs the native capability, which the Go web profile does not compose, so
// it answers the unavailable diagnostic.
type DirectoryPickerController struct{}

// NewDirectoryPickerController builds the namespace host.
func NewDirectoryPickerController() *DirectoryPickerController {
	return &DirectoryPickerController{}
}

// pickerEntry is one listing/crumb row: name, absolute path, hidden flag.
type pickerEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Hidden bool   `json:"hidden"`
}

// Pick opens the host's OS chooser; unavailable without the native backend.
func (c *DirectoryPickerController) Pick(ctx context.Context) (any, error) {
	return nil, wrapGatewayError("directory-picker/unavailable", "directoryPicker/pick", "", nil, "%s", directoryPickerUnavailable)
}

// List answers one directory level with its ancestry for the in-app browser.
// An absent path (empty wire string) lists the home directory. Only
// enterable directories contend for rows, name-sorted, bounded at
// maxPickerEntries with the truncated flag marking a cut level.
func (c *DirectoryPickerController) List(ctx context.Context, path string) (any, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	target := home
	if path != "" {
		if !fullyQualifiedPath(path) {
			return nil, wrapGatewayError("directory-picker/unreadable", "directoryPicker/list", "", nil,
				"cannot list %q: not a fully qualified path", path)
		}
		target = filepath.Clean(path)
	}
	entries, truncated, err := c.listLevel(target)
	if err != nil {
		return nil, wrapGatewayError("directory-picker/unreadable", "directoryPicker/list", "", err,
			"cannot list %s", target)
	}
	return map[string]any{
		"path":      target,
		"home":      home,
		"crumbs":    ancestryCrumbs(target),
		"entries":   entries,
		"truncated": truncated,
	}, nil
}

// listLevel reads one directory's enterable child directories, name-sorted.
func (c *DirectoryPickerController) listLevel(target string) ([]any, bool, error) {
	file, err := os.Open(target)
	if err != nil {
		return nil, false, err
	}
	names, err := file.Readdirnames(-1)
	closeErr := file.Close()
	if err != nil && err != os.ErrClosed {
		return nil, false, err
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	sort.Strings(names)
	entries := make([]any, 0, len(names))
	truncated := false
	for _, name := range names {
		if len(entries) >= maxPickerEntries {
			truncated = true
			break
		}
		full := filepath.Join(target, name)
		info, statErr := os.Stat(full)
		if statErr != nil || !info.IsDir() {
			continue
		}
		entries = append(entries, pickerEntry{Name: name, Path: full, Hidden: strings.HasPrefix(name, ".")})
	}
	return entries, truncated, nil
}

// CreateDirectory creates one child directory under an absolute parent and
// answers the created directory's absolute path.
func (c *DirectoryPickerController) CreateDirectory(ctx context.Context, path string, name string) (any, error) {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return nil, wrapGatewayError("gateway/bad-request", "directoryPicker/createDirectory", "", nil,
			"invalid payload for host.createDirectory: name must be a single non-blank path segment")
	}
	if !fullyQualifiedPath(path) {
		return nil, wrapGatewayError("directory-picker/unreadable", "directoryPicker/createDirectory", "", nil,
			"cannot create under %q: not a fully qualified path", path)
	}
	created := filepath.Join(filepath.Clean(path), name)
	if _, err := os.Stat(created); err == nil {
		return nil, wrapGatewayError("directory-picker/exists", "directoryPicker/createDirectory", "", nil,
			"directory already exists: %s", created)
	}
	if err := os.Mkdir(created, 0o755); err != nil {
		return nil, wrapGatewayError("directory-picker/create-failed", "directoryPicker/createDirectory", "", err,
			"cannot create directory %s", created)
	}
	return created, nil
}

// fullyQualifiedPath reports whether the path names one fixed filesystem
// location regardless of process state (official fullyQualified): on Windows
// only drive-qualified (C:\…) or complete UNC forms; elsewhere any absolute
// path.
func fullyQualifiedPath(path string) bool {
	if runtime.GOOS == "windows" {
		if len(path) < 3 || path[1] != ':' {
			return strings.HasPrefix(path, `\\`) && len(path) > 3
		}
		return (path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')
	}
	return filepath.IsAbs(path)
}

// paramJSON is the standard named JSON parameter descriptor.
func paramJSON(name string) typert.InvocationParameterDescriptor {
	return typert.InvocationParameterDescriptor{
		Name:   name,
		Wire:   name,
		Source: typert.SourceJSON,
		Codec:  typert.Codec{Mode: typert.CodecSrcJSON},
	}
}

// ancestryCrumbs walks from the filesystem root to target inclusive, each a
// breadcrumb jump target (official ancestryCrumbs).
func ancestryCrumbs(target string) []any {
	var crumbs []any
	current := filepath.Clean(target)
	for {
		parent := filepath.Dir(current)
		name := filepath.Base(current)
		if parent == current {
			name = current
		}
		crumbs = append([]any{pickerEntry{Name: name, Path: current, Hidden: false}}, crumbs...)
		if parent == current {
			return crumbs
		}
		current = parent
	}
}

// Contribution is the strict typert definition of the directoryPicker
// namespace. Wire methods stay lowercase (official endpoint grammar);
// Implementation names the Go receiver method the reflection dispatcher
// calls. The list path parameter carries acceptsUndefined (the official
// schema is string | undefined; an absent wire field decodes to the typed
// zero, which List treats as the home directory).
func (c *DirectoryPickerController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	inv := typert.InvocationReceiver{Kind: typert.ReceiverDirect}
	descriptor := func(id, method, implementation string, params ...typert.InvocationParameterDescriptor) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID:                    id,
			Service:               "directoryPickerController",
			Namespace:             "directoryPicker",
			Method:                method,
			Implementation:        implementation,
			Invocation:            inv,
			CancellationParameter: "signal",
			Parameters:            params,
			Result:                jsonCodec,
		}
	}
	listParam := typert.InvocationParameterDescriptor{
		Name:            "path",
		Wire:            "path",
		Source:          typert.SourceJSON,
		AcceptsUndefined: true,
		Codec:           typert.Codec{Mode: typert.CodecSrcJSON},
	}
	return typert.Contribution{
		Package: "directory-picker-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			descriptor("directoryPicker.pick", "pick", "Pick"),
			descriptor("directoryPicker.list", "list", "List", listParam),
			descriptor("directoryPicker.createDirectory", "createDirectory", "CreateDirectory", paramJSON("path"), paramJSON("name")),
		},
	}
}
