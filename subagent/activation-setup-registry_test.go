package subagent

import (
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
)

// recordingContribution builds a contribution that appends one setup effect
// to the child context and records its lifecycle.
type recordingContribution struct {
	name      string
	installed []string
	disposed  []string
	// disposeErr, when set, fails the returned disposer.
	disposeErr error
	// selfRevoke makes the installer revoke its own registration before
	// returning (the escaped-installation race).
	selfRevoke func()
}

func (r *recordingContribution) contribution() ContinuableSetupContribution {
	return func(childCtx *cordis.Context) func() {
		r.installed = append(r.installed, r.name)
		if r.selfRevoke != nil {
			r.selfRevoke()
		}
		return func() {
			r.disposed = append(r.disposed, r.name)
			if r.disposeErr != nil {
				panic(r.disposeErr)
			}
		}
	}
}

func TestApplyInstallsAndCommits(t *testing.T) {
	registry := NewActivationSetupRegistry()
	first := &recordingContribution{name: "a"}
	second := &recordingContribution{name: "b"}
	for _, contribution := range []*recordingContribution{first, second} {
		undo, err := registry.Register(contribution.contribution())
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(undo)
	}
	child := cordis.NewRoot(cordis.Discard{}).Child()
	commit, err := registry.Apply(child)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(first.installed) != 1 || len(second.installed) != 1 {
		t.Fatalf("installed = %v + %v", first.installed, second.installed)
	}
	if err := commit.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Residency: the child scope disposal still releases the installations.
	if err := child.Dispose(); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if len(first.disposed) != 1 || len(second.disposed) != 1 {
		t.Fatalf("disposed = %v + %v", first.disposed, second.disposed)
	}
}

func TestCommitFailsWhenContributionRevokedDuringBuild(t *testing.T) {
	registry := NewActivationSetupRegistry()
	// The second contribution revokes the first while the child is built.
	var firstUndo func()
	first := &recordingContribution{name: "a"}
	second := &recordingContribution{name: "b", selfRevoke: func() { firstUndo() }}
	undo, err := registry.Register(first.contribution())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	firstUndo = undo
	if _, err := registry.Register(second.contribution()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	child := cordis.NewRoot(cordis.Discard{}).Child()
	commit, err := registry.Apply(child)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	err = commit.Commit()
	var subagentErr SubagentError
	if !asSubagentError(err, &subagentErr) || subagentErr.Code() != CodeActivationSetupRevoked {
		t.Fatalf("commit err = %v, want ACTIVATION_SETUP_REVOKED", err)
	}
	if !strings.Contains(subagentErr.Error(), "the child was not established") {
		t.Fatalf("message = %q", subagentErr.Error())
	}
	// The escaped installation was disposed during apply, not at commit.
	if len(first.disposed) != 1 {
		t.Fatalf("first disposed = %v, want the escaped record released immediately", first.disposed)
	}
}

func TestInstallerPanicRollsBackEveryInstallation(t *testing.T) {
	registry := NewActivationSetupRegistry()
	first := &recordingContribution{name: "a"}
	second := &recordingContribution{name: "b"}
	third := &recordingContribution{name: "c"}
	// A later installer panics AFTER `third` returned its disposer: the
	// whole completed batch must roll back, keeping the panic authoritative.
	exploding := func(childCtx *cordis.Context) func() {
		panic("setup blew up")
	}
	for _, registration := range []struct {
		contribution ContinuableSetupContribution
	}{
		{first.contribution()}, {second.contribution()}, {third.contribution()}, {exploding},
	} {
		undo, err := registry.Register(registration.contribution)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(undo)
	}
	child := cordis.NewRoot(cordis.Discard{}).Child()
	if _, err := registry.Apply(child); err == nil || !strings.Contains(err.Error(), "setup blew up") {
		t.Fatalf("Apply err = %v, want the installer failure authoritative", err)
	}
	// Every completed installation in the batch rolled back.
	if len(first.disposed) != 1 || len(second.disposed) != 1 || len(third.disposed) != 1 {
		t.Fatalf("disposed = %v + %v + %v", first.disposed, second.disposed, third.disposed)
	}
}

func TestContributionRemovalReleasesLiveInstallations(t *testing.T) {
	registry := NewActivationSetupRegistry()
	contribution := &recordingContribution{name: "a"}
	undo, err := registry.Register(contribution.contribution())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	child := cordis.NewRoot(cordis.Discard{}).Child()
	if _, err := registry.Apply(child); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Idempotent undo.
	undo()
	undo()
	if len(contribution.disposed) != 1 {
		t.Fatalf("disposed = %v, want exactly one release", contribution.disposed)
	}
	// A removed contribution cannot install into a later child.
	late := cordis.NewRoot(cordis.Discard{}).Child()
	if _, err := registry.Apply(late); err != nil {
		t.Fatalf("Apply late: %v", err)
	}
	if len(contribution.installed) != 1 {
		t.Fatalf("installed = %v, want no install after revocation", contribution.installed)
	}
}

func TestFailingDisposerPanicsWithReleaseFailure(t *testing.T) {
	registry := NewActivationSetupRegistry()
	contribution := &recordingContribution{name: "a", disposeErr: errors.New("revoke exploded")}
	undo, err := registry.Register(contribution.contribution())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(undo)
	child := cordis.NewRoot(cordis.Discard{}).Child()
	if _, err := registry.Apply(child); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected the release failure to surface")
		}
		subagentErr, ok := rec.(SubagentError)
		if !ok || subagentErr.Code() != CodeActivationSetupReleaseFailed {
			t.Fatalf("recovered %v, want ACTIVATION_SETUP_RELEASE_FAILED", rec)
		}
		if !strings.Contains(subagentErr.Error(), "failed to release 1 installation(s): revoke exploded") {
			t.Fatalf("message = %q", subagentErr.Error())
		}
	}()
	undo()
}

func TestChildScopeDisposalReleasesEverything(t *testing.T) {
	registry := NewActivationSetupRegistry()
	first := &recordingContribution{name: "a"}
	second := &recordingContribution{name: "b"}
	for _, contribution := range []*recordingContribution{first, second} {
		undo, err := registry.Register(contribution.contribution())
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		t.Cleanup(undo)
	}
	root := cordis.NewRoot(cordis.Discard{})
	child := root.Child()
	if _, err := registry.Apply(child); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Rolling the child back without commit (a failed setup elsewhere)
	// releases every installation.
	if err := child.Dispose(); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if len(first.disposed) != 1 || len(second.disposed) != 1 {
		t.Fatalf("disposed = %v + %v", first.disposed, second.disposed)
	}
	// Disposing again does not double-release.
	if err := child.Dispose(); err != nil {
		t.Fatalf("second Dispose: %v", err)
	}
	if len(first.disposed) != 1 {
		t.Fatalf("first disposed = %v, want exactly once", first.disposed)
	}
}
