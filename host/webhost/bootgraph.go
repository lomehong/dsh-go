package webhost

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The official boot wire (@deepseek-ai/dsh-client-modules
// src/client/manifest.ts): the composed client entry graph the host injects
// as window.__DSH_BOOT__.
type bootEntry struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Rev         string   `json:"rev"`
	Inject      []string `json:"inject,omitempty"`
	Immediately bool     `json:"immediately,omitempty"`
	External    []string `json:"external,omitempty"`
}

type bootBatch struct {
	Phase   string   `json:"phase"`
	URL     string   `json:"url"`
	Rev     string   `json:"rev"`
	Entries []string `json:"entries"`
}

type bootGraph struct {
	Rev     string      `json:"rev"`
	Entries []bootEntry `json:"entries"`
	Batches []bootBatch `json:"batches"`
}

// dshClientDecl is package.json `dsh.client` (official DshClientDeclaration).
type dshClientDecl struct {
	Inject      *[]string `json:"inject"`
	External    *[]string `json:"external"`
	Immediately bool      `json:"immediately"`
	Platform    string    `json:"platform"`
}

type dshPackageJSON struct {
	Dsh *struct {
		Client *dshClientDecl `json:"client"`
	} `json:"dsh"`
	Exports map[string]json.RawMessage `json:"exports"`
}

// pluginRecord is one scanned client package (official WebPluginRecord).
type pluginRecord struct {
	entry  bootEntry
	bundle []byte
}

// Official constants (client-modules src/index.ts).
const (
	bootMaxComboURLBytes    = 3 * 1024
	bootHashRevisionLength  = 12
	bootComboRevPlaceholder = "000000000000"
	bootClientModulesID     = "@deepseek-ai/dsh-client-modules"
	bootSourceMapContType   = "application/json; charset=utf-8"
	bootScriptContType      = "text/javascript; charset=utf-8"
)

// bootTrailersRe strips bundle-local debug directives (official
// SOURCE_URL_TRAILER / SOURCE_MAP_TRAILER).
var bootTrailersRe = regexp.MustCompile(`(?m)^//# (?:sourceMappingURL|sourceMapURL)=.*(?:\r?\n)?`)

// framedHash hashes parts without bytes moving across field boundaries
// (official framedHash): sha1 over the domain, then length-prefixed parts.
func framedHash(domain string, parts ...[]byte) string {
	h := sha1.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	for _, part := range parts {
		h.Write([]byte(strconv.Itoa(len(part)) + ":"))
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))[:bootHashRevisionLength]
}

// comboURL addresses one ordered plugin-file list through the combo route.
func comboURL(ids []string, rev string, sourceMap bool) string {
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = id + "/client.js"
		if sourceMap {
			list[i] += ".map"
		}
	}
	return "/plugins/??" + strings.Join(list, ",") + "&rev=" + rev
}

// comboArtifact is one built combo script (official ComboArtifact).
type comboArtifact struct {
	url       string
	rev       string
	entries   []string
	script    []byte
	sourceMap []byte
	mapURL    string
}

// comboSection is one indexed-map section.
type comboSection struct {
	Offset struct {
		Line   int `json:"line"`
		Column int `json:"column"`
	} `json:"offset"`
	Map map[string]any `json:"map"`
}

// buildCombo concatenates factory registrations and composes indexed-section
// maps. The installed rc packages ship no client.js.map, so the official
// identitySectionMap fallback applies per record.
func buildCombo(records []*pluginRecord, revision string) comboArtifact {
	var source strings.Builder
	sections := make([]comboSection, 0, len(records))
	for _, record := range records {
		prepared := bootTrailersRe.ReplaceAllString(string(record.bundle), "")
		if !strings.HasSuffix(prepared, "\n") {
			prepared += "\n"
		}
		var mappings strings.Builder
		for i := 0; i < strings.Count(prepared, "\n"); i++ {
			if i > 0 {
				mappings.WriteString(";")
			}
			mappings.WriteString("AAAA")
		}
		sections = append(sections, comboSection{Map: map[string]any{
			"version":        3,
			"names":          []any{},
			"sources":        []string{"/" + record.entry.ID + "/client.js"},
			"sourcesContent": []string{prepared},
			"mappings":       mappings.String(),
		}})
		source.WriteString(prepared + ";\n")
	}
	sourceBytes := []byte(source.String())
	sourceMapBytes, err := json.Marshal(map[string]any{
		"version":  3,
		"file":     "client.js",
		"sections": sections,
	})
	if err != nil {
		sourceMapBytes = []byte("{}")
	}
	sourceMapBytes = append(sourceMapBytes, '\n')
	ids := make([]string, len(records))
	for i, record := range records {
		ids[i] = record.entry.ID
	}
	rev := revision
	if rev == "" {
		rev = framedHash("combo", sourceBytes, sourceMapBytes)
	}
	url := comboURL(ids, rev, false)
	mapURL := comboURL(ids, rev, true)
	script := append(sourceBytes, []byte("//# sourceMappingURL="+mapURL+"\n")...)
	return comboArtifact{url: url, rev: rev, entries: ids, script: script, sourceMap: sourceMapBytes, mapURL: mapURL}
}

// clientExportPath reads the ./client exports subpath: object {default} or
// bare string form (official require.resolve semantics, lenient).
func clientExportPath(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var obj struct {
		Default string `json:"default"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Default
	}
	return ""
}

// scanClientPackages reads every node_modules package declaring
// dsh.client.platform == "web" (official activation scan; a web-declaring
// package with a missing bundle fails loud).
func scanClientPackages(nodeModules string) ([]*pluginRecord, error) {
	dirs, err := filepath.Glob(filepath.Join(nodeModules, "@*", "*"))
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	var records []*pluginRecord
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			continue
		}
		var pkg dshPackageJSON
		if err := json.Unmarshal(raw, &pkg); err != nil {
			// Over-broad walk: third-party packages outside the official
			// layout are skipped, while dsh.client packages below fail
			// loud on a missing bundle.
			continue
		}
		if pkg.Dsh == nil || pkg.Dsh.Client == nil || pkg.Dsh.Client.Platform != "web" {
			continue
		}
		name := filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
		decl := pkg.Dsh.Client
		clientPath := "lib/client.js"
		if sub := clientExportPath(pkg.Exports["./client"]); sub != "" {
			clientPath = sub
		}
		bundle, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(clientPath)))
		if err != nil {
			return nil, fmt.Errorf("web: %s dsh.client bundle missing: %w", name, err)
		}
		entry := bootEntry{
			ID:          name,
			URL:         comboURL([]string{name}, framedHash("plugin-artifact", bundle), false),
			Rev:         framedHash("plugin-artifact", bundle),
			Immediately: decl.Immediately,
		}
		if decl.Inject != nil {
			entry.Inject = *decl.Inject
		}
		if decl.External != nil {
			entry.External = *decl.External
		}
		records = append(records, &pluginRecord{entry: entry, bundle: bundle})
	}
	return records, nil
}

// orderByModuleGraph topologically orders rows so every requested dynamic
// package precedes its consumers; scan order breaks ties (official
// orderByModuleGraph). `<pkg>/client` aliases the bare package.
func orderByModuleGraph(entries []bootEntry) ([]bootEntry, error) {
	rows := make(map[string]*bootEntry, len(entries))
	for i := range entries {
		rows[entries[i].ID] = &entries[i]
	}
	var ordered []bootEntry
	placed := make(map[string]bool)
	var open []string
	var visit func(entry *bootEntry) error
	visit = func(entry *bootEntry) error {
		if placed[entry.ID] {
			return nil
		}
		for i, id := range open {
			if id == entry.ID {
				return fmt.Errorf("web: client module graph cycle %s",
					strings.Join(append(append([]string{}, open[i:]...), entry.ID), " -> "))
			}
		}
		open = append(open, entry.ID)
		for _, name := range entry.External {
			dependency := rows[strings.TrimSuffix(name, "/client")]
			if dependency == nil {
				continue
			}
			if dependency.ID == entry.ID {
				return fmt.Errorf("web: %s requests module %q that it answers itself", entry.ID, name)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		open = open[:len(open)-1]
		placed[entry.ID] = true
		ordered = append(ordered, *entry)
		return nil
	}
	for i := range entries {
		if err := visit(&entries[i]); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// partitionComboChunks splits records so no combo URL exceeds the protocol
// limit (official partitionComboRecords, measured on the longer map form).
func partitionComboChunks(records []*pluginRecord) [][]*pluginRecord {
	projected := func(n int) int {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = bootComboRevPlaceholder
		}
		return len(comboURL(ids, bootComboRevPlaceholder, true))
	}
	var chunks [][]*pluginRecord
	var current []*pluginRecord
	for _, record := range records {
		if projected(len(current)+1) <= bootMaxComboURLBytes {
			current = append(current, record)
			continue
		}
		if len(current) > 0 {
			chunks = append(chunks, current)
		}
		current = []*pluginRecord{record}
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// servedResponse is one exact-URL plugin response.
type servedResponse struct {
	body        []byte
	contentType string
}

// composeBootGraph scans, orders, and batches the client graph, returning it
// with the exact-URL response table (official compose()). The bootstrap
// batch owns the module-system package; every other entry lands in
// application combos in graph order.
func composeBootGraph(nodeModules string) (*bootGraph, map[string]servedResponse, error) {
	records, err := scanClientPackages(nodeModules)
	if err != nil {
		return nil, nil, err
	}
	entries := make([]bootEntry, len(records))
	for i, record := range records {
		entries[i] = record.entry
	}
	ordered, err := orderByModuleGraph(entries)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]*pluginRecord, len(records))
	for _, record := range records {
		byID[record.entry.ID] = record
	}
	var bootstrap, application []*pluginRecord
	for i := range ordered {
		if ordered[i].ID == bootClientModulesID {
			bootstrap = append(bootstrap, byID[ordered[i].ID])
		} else {
			application = append(application, byID[ordered[i].ID])
		}
	}
	var batches []bootBatch
	responses := make(map[string]servedResponse)
	emit := func(phase string, chunked []*pluginRecord) {
		for _, chunk := range partitionComboChunks(chunked) {
			artifact := buildCombo(chunk, "")
			batches = append(batches, bootBatch{
				Phase: phase, URL: artifact.url, Rev: artifact.rev, Entries: artifact.entries,
			})
			responses[artifact.url] = servedResponse{body: artifact.script, contentType: bootScriptContType}
			responses[artifact.mapURL] = servedResponse{body: artifact.sourceMap, contentType: bootSourceMapContType}
		}
	}
	emit("bootstrap", bootstrap)
	emit("application", application)
	for _, record := range records {
		artifact := buildCombo([]*pluginRecord{record}, record.entry.Rev)
		responses[artifact.url] = servedResponse{body: artifact.script, contentType: bootScriptContType}
		responses[artifact.mapURL] = servedResponse{body: artifact.sourceMap, contentType: bootSourceMapContType}
	}
	wire, err := json.Marshal(struct {
		Entries []bootEntry `json:"entries"`
		Batches []bootBatch `json:"batches"`
	}{ordered, batches})
	if err != nil {
		return nil, nil, err
	}
	graph := &bootGraph{
		Rev:     framedHash("graph", wire),
		Entries: ordered,
		Batches: batches,
	}
	return graph, responses, nil
}

// bootQueueScript is the official inline registration queue
// (bootInjections), verbatim with the bootstrap id interpolated.
func bootQueueScript() string {
	id := strconv.Quote(bootClientModulesID)
	return "(()=>{\n" +
		"const pendingQueue=[]\n" +
		"window.__ModuleLoader__={\n" +
		"  mode:\"queue\",\n" +
		"  pendingQueue,\n" +
		"  load(registration){pendingQueue.push(registration)},\n" +
		"  create(options){\n" +
		"    if(this.mode!==\"queue\")throw new Error(\"client-modules: window.__ModuleLoader__.create called after module-system boot\")\n" +
		"    const index=pendingQueue.findIndex(registration=>registration.id===" + id + ")\n" +
		"    const registration=pendingQueue[index]\n" +
		"    if(registration===undefined)throw new Error(\"client-modules: HTML did not preload " + bootClientModulesID + "/client.js\")\n" +
		"    pendingQueue.splice(index,1)\n" +
		"    const exports=registration.factory(specifier=>{\n" +
		"      throw new Error('client-modules: " + bootClientModulesID + "/client.js requested external \"'+specifier+'\" before the module system existed')\n" +
		"    })\n" +
		"    if(typeof exports!==\"object\"||exports===null||typeof exports.createClientModuleSystem!==\"function\"||typeof exports.apply!==\"function\"){\n" +
		"      throw new Error(\"client-modules: " + bootClientModulesID + "/client.js did not export the bootstrap module face\")\n" +
		"    }\n" +
		"    return exports.createClientModuleSystem(this,{id:registration.id,exports},options)\n" +
		"  }\n" +
		"}\n" +
		"})()"
}

// escapeHTMLAttribute escapes a value for a quoted HTML attribute.
func escapeHTMLAttribute(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

// jsonScriptSafe escapes `<` in JSON so row-controlled strings cannot break
// out of the script element (official global row rendering).
func jsonScriptSafe(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "undefined"
	}
	return strings.ReplaceAll(string(raw), "<", `<\u003c`)
}

// bootInjectionRows renders the boot protocol as head rows in execution
// order: queue script, application preloads, blocking bootstrap scripts,
// graph global (official bootInjections).
func bootInjectionRows(graph *bootGraph) string {
	var head strings.Builder
	head.WriteString("<script>" + bootQueueScript() + "</script>")
	for _, batch := range graph.Batches {
		if batch.Phase == "application" {
			head.WriteString(`<link rel="preload" as="script" href="` + escapeHTMLAttribute(batch.URL) + `">`)
		}
	}
	for _, batch := range graph.Batches {
		if batch.Phase == "bootstrap" {
			head.WriteString(`<script src="` + escapeHTMLAttribute(batch.URL) + `"></script>`)
		}
	}
	head.WriteString(`<script>globalThis["__DSH_BOOT__"] = ` + jsonScriptSafe(graph) + `</script>`)
	return head.String()
}

// nodeModulesFromDist walks up from the frontend dist to the node_modules
// root: <root>/node_modules/@deepseek-ai/dsh-frontend/dist.
func nodeModulesFromDist(dist string) string {
	return filepath.Clean(filepath.Join(dist, "..", "..", ".."))
}

// servePlugins answers exact-URL combo lookups. The map key is the raw
// request target, which keeps the double-question combo marker verbatim:
// browsers serialize `/plugins/??a&rev=b` with both question marks (the
// second is query content), so RequestURI matches the composed key exactly
// (official plugin route is a `/plugins` prefix serving the same table).
func servePlugins(w http.ResponseWriter, r *http.Request, responses map[string]servedResponse) bool {
	response, ok := responses[r.URL.RequestURI()]
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", response.contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response.body)
	return true
}
