package preset

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/scope"
)

// fakeTree is the assembled standing composition stand-in.
type fakeTree struct {
	pending  [][]string
	disposed bool
}

func (f *fakeTree) PendingInjections() [][]string { return f.pending }
func (f *fakeTree) Dispose() error                { f.disposed = true; return nil }

// mountFixture wires one Mounts over a temp roster and a counting assembler.
type mountFixture struct {
	host         *cordis.Context
	mounts       *Mounts
	composed     int
	trees        []*fakeTree
	failAssemble error
	pending      [][]string
	lastCtx      *cordis.Context
}

const testScopeService = "test.agentScope"

func testScopeOf(ctx *cordis.Context) scope.ScopeKey {
	if key, ok := ctx.Get(testScopeService).(scope.ScopeKey); ok {
		return key
	}
	return nil
}

func newMountFixture(t *testing.T, presets ...string) *mountFixture {
	t.Helper()
	root := t.TempDir()
	for _, id := range presets {
		writeComposition(t, root, id, "[]")
	}
	roster := NewRoster(Config{Default: presets[0], Roots: []PresetRoot{{Path: root, Trust: TrustUser}}}, RosterOptions{})
	host := cordis.NewRoot(cordis.Discard{})
	fx := &mountFixture{host: host}
	mounts, err := NewMounts(host, roster, MountOptions{
		Assemble: func(ctx *cordis.Context, p AgentPreset) (StandingTree, error) {
			fx.composed++
			fx.lastCtx = ctx
			if fx.failAssemble != nil {
				return nil, fx.failAssemble
			}
			tree := &fakeTree{pending: fx.pending}
			fx.trees = append(fx.trees, tree)
			return tree, nil
		},
		ScopeOf: testScopeOf,
	})
	if err != nil {
		t.Fatalf("new mounts: %v", err)
	}
	fx.mounts = mounts
	return fx
}

func writeComposition(t *testing.T, root, id, content string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, CompositionFile)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write composition: %v", err)
	}
	return path
}

// scopedAgentCtx mints one agent context carrying its scope key.
func scopedAgentCtx(host *cordis.Context, key scope.ScopeKey) *cordis.Context {
	ctx := host.Child()
	ctx.Provide(testScopeService, key)
	return ctx
}

func TestMountJoinsAgentToStanding(t *testing.T) {
	fx := newMountFixture(t, "base")
	agentKey := scope.NewScopeKey(nil)
	preset, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "base")
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if preset.ID != "base" {
		t.Fatalf("mounted preset %q", preset.ID)
	}
	standingKey := scope.ParentOf(agentKey)
	if standingKey == nil {
		t.Fatal("agent key has no standing parent after mount")
	}
	// The standing context carries the standing key to its entries.
	if carried, _ := StandingScopeService.From(fx.lastCtx); carried != standingKey {
		t.Fatalf("standing ctx carries %v, want %v", carried, standingKey)
	}
	mount := fx.mounts.StandingMountFor(agentKey)
	if mount == nil || mount.Key != standingKey || mount.PresetID != "base" {
		t.Fatalf("StandingMountFor = %+v", mount)
	}
	if live := fx.mounts.LiveMounts(); len(live) != 1 {
		t.Fatalf("LiveMounts = %d", len(live))
	}
}

func TestMountRefusesUnscopedContextVerbatim(t *testing.T) {
	fx := newMountFixture(t, "base")
	_, err := fx.mounts.Mount(fx.host.Child(), "base")
	want := "agent-presets: refusing to compose an unscoped context; the scope key is what joins an agent to its preset"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %v, want %q", err, want)
	}
}

func TestMountRefusesBrokenAndUnknownPresets(t *testing.T) {
	fx := newMountFixture(t, "base")
	agentKey := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "ghost"); err == nil {
		t.Fatal("unknown preset accepted")
	} else {
		var unknown *UnknownPresetError
		if !errors.As(err, &unknown) {
			t.Fatalf("unknown refusal = %v", err)
		}
	}
}

func TestMountIsSingleFlightAndReused(t *testing.T) {
	fx := newMountFixture(t, "base")
	first := scope.NewScopeKey(nil)
	second := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, first), "base"); err != nil {
		t.Fatalf("mount first: %v", err)
	}
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, second), "base"); err != nil {
		t.Fatalf("mount second: %v", err)
	}
	if fx.composed != 1 {
		t.Fatalf("composed %d times, want 1", fx.composed)
	}
	if scope.ParentOf(first) != scope.ParentOf(second) {
		t.Fatal("two agents joined different standings for one preset")
	}
}

func TestMountRefusesSecondBindOfOneAgent(t *testing.T) {
	fx := newMountFixture(t, "base", "other")
	agentKey := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "base"); err != nil {
		t.Fatalf("mount: %v", err)
	}
	_, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "other")
	if err == nil || !strings.Contains(err.Error(), "already bound to a parent") {
		t.Fatalf("second bind = %v", err)
	}
}

func TestMountEmptyIDMountsTheDefault(t *testing.T) {
	fx := newMountFixture(t, "base")
	agentKey := scope.NewScopeKey(nil)
	preset, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "")
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if preset.ID != "base" {
		t.Fatalf("default mount = %q", preset.ID)
	}
}

func TestStampChangeStartsTheNextGeneration(t *testing.T) {
	root := t.TempDir()
	path := writeComposition(t, root, "base", "[]")
	roster := NewRoster(Config{Default: "base", Roots: []PresetRoot{{Path: root, Trust: TrustUser}}}, RosterOptions{})
	host := cordis.NewRoot(cordis.Discard{})
	composed := 0
	mounts, err := NewMounts(host, roster, MountOptions{
		Assemble: func(ctx *cordis.Context, p AgentPreset) (StandingTree, error) {
			composed++
			return &fakeTree{}, nil
		},
		ScopeOf: testScopeOf,
	})
	if err != nil {
		t.Fatalf("new mounts: %v", err)
	}
	first, err := mounts.StandingKeyFor("base")
	if err != nil {
		t.Fatalf("standing key: %v", err)
	}
	// A size-changing edit (both texts stay valid compositions) starts the
	// next generation for the next reader.
	if err := os.WriteFile(path, []byte("[]\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	second, err := mounts.StandingKeyFor("base")
	if err != nil {
		t.Fatalf("standing key after edit: %v", err)
	}
	if first == second {
		t.Fatal("stamp change kept the old generation")
	}
	if composed != 2 {
		t.Fatalf("composed %d times, want 2", composed)
	}
}

func TestUnreadableCompositionFailsLoud(t *testing.T) {
	fx := newMountFixture(t, "base")
	missing := &PresetMountError{}
	_, err := fx.mounts.buildStanding(AgentPreset{ID: "gone", Path: filepath.Join(t.TempDir(), "no-such-file.yml")})
	if !errors.As(err, &missing) {
		t.Fatalf("unreadable refusal = %v", err)
	}
	if !strings.Contains(missing.Reason, "composition file is unreadable") {
		t.Fatalf("reason = %q", missing.Reason)
	}
}

func TestAssembleFailureFailsLoudWithThePath(t *testing.T) {
	fx := newMountFixture(t, "base")
	fx.failAssemble = errors.New("failed to apply loader entry row (name): boom")
	agentKey := scope.NewScopeKey(nil)
	_, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "base")
	var mountErr *PresetMountError
	if !errors.As(err, &mountErr) {
		t.Fatalf("assemble refusal = %v", err)
	}
	if !strings.Contains(mountErr.Reason, "boom") || !strings.Contains(mountErr.Reason, CompositionFile) {
		t.Fatalf("reason = %q", mountErr.Reason)
	}
}

func TestStrandedInjectionFailsTheMount(t *testing.T) {
	fx := newMountFixture(t, "base")
	fx.pending = [][]string{{"missingService"}}
	agentKey := scope.NewScopeKey(nil)
	_, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "base")
	var mountErr *PresetMountError
	if !errors.As(err, &mountErr) {
		t.Fatalf("audit refusal = %v", err)
	}
	if !strings.Contains(mountErr.Reason, "composition rows never activated") || !strings.Contains(mountErr.Reason, "missingService") {
		t.Fatalf("reason = %q", mountErr.Reason)
	}
	// The failed tree was torn down and no record survives.
	if len(fx.trees) != 1 || !fx.trees[0].disposed {
		t.Fatal("failed standing tree not disposed")
	}
	if live := fx.mounts.LiveMounts(); len(live) != 0 {
		t.Fatalf("live mounts after refusal = %d", len(live))
	}
}

func TestComposeFromJoinsTheParentGeneration(t *testing.T) {
	fx := newMountFixture(t, "base")
	parentKey := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, parentKey), "base"); err != nil {
		t.Fatalf("mount parent: %v", err)
	}
	childKey := scope.NewScopeKey(nil)
	id, joined, err := fx.mounts.ComposeFrom(scopedAgentCtx(fx.host, childKey), scopedAgentCtx(fx.host, parentKey))
	if err != nil || !joined || id != "base" {
		t.Fatalf("composeFrom = %q/%v/%v", id, joined, err)
	}
	// The child joined the parent's EXACT generation: no new composition.
	if fx.composed != 1 {
		t.Fatalf("composed %d times, want 1", fx.composed)
	}
	if scope.ParentOf(childKey) != scope.ParentOf(parentKey) {
		t.Fatal("child joined a different standing than its parent")
	}
	if got, ok := fx.mounts.ComposedPreset(scopedAgentCtx(fx.host, childKey)); !ok || got != "base" {
		t.Fatalf("ComposedPreset = %q/%v", got, ok)
	}
}

func TestComposeFromYieldsNothingForAnUnjoinedParent(t *testing.T) {
	fx := newMountFixture(t, "base")
	parentKey := scope.NewScopeKey(nil)
	childKey := scope.NewScopeKey(nil)
	id, joined, err := fx.mounts.ComposeFrom(scopedAgentCtx(fx.host, childKey), scopedAgentCtx(fx.host, parentKey))
	if err != nil || joined || id != "" {
		t.Fatalf("composeFrom = %q/%v/%v, want no join and no error", id, joined, err)
	}
}

func TestComposeFromRefusesUnscopedChildVerbatim(t *testing.T) {
	fx := newMountFixture(t, "base")
	parentKey := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, parentKey), "base"); err != nil {
		t.Fatalf("mount parent: %v", err)
	}
	_, _, err := fx.mounts.ComposeFrom(fx.host.Child(), scopedAgentCtx(fx.host, parentKey))
	want := "agent-presets: refusing to compose an unscoped context; the scope key is what joins an agent to its preset"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %v, want %q", err, want)
	}
}

func TestStandingMountForUnjoinedAgent(t *testing.T) {
	fx := newMountFixture(t, "base")
	if mount := fx.mounts.StandingMountFor(nil); mount != nil {
		t.Fatalf("nil key resolved %+v", mount)
	}
	if mount := fx.mounts.StandingMountFor(scope.NewScopeKey(nil)); mount != nil {
		t.Fatalf("unjoined key resolved %+v", mount)
	}
}

func TestHostTeardownDisposesStandingTrees(t *testing.T) {
	fx := newMountFixture(t, "base")
	agentKey := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "base"); err != nil {
		t.Fatalf("mount: %v", err)
	}
	if err := fx.host.Dispose(); err != nil {
		t.Fatalf("dispose host: %v", err)
	}
	if len(fx.trees) != 1 || !fx.trees[0].disposed {
		t.Fatal("standing tree survived host teardown")
	}
}

func TestInvalidateDropsTheRecordWithoutDisposal(t *testing.T) {
	fx := newMountFixture(t, "base")
	agentKey := scope.NewScopeKey(nil)
	if _, err := fx.mounts.Mount(scopedAgentCtx(fx.host, agentKey), "base"); err != nil {
		t.Fatalf("mount: %v", err)
	}
	fx.mounts.Invalidate("base")
	if live := fx.mounts.LiveMounts(); len(live) != 0 {
		t.Fatalf("live mounts after invalidate = %d", len(live))
	}
	// Joined agents keep their generation: the tree is not disposed.
	if fx.trees[0].disposed {
		t.Fatal("invalidate disposed a joined generation")
	}
}
