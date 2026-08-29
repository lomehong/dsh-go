package fssandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/fs"
	"dshgo/fslocal"
	"dshgo/sandboxpolicy"
)

func newFence(t *testing.T, mode string, root string) (*Sandboxed, string) {
	t.Helper()
	local, err := fslocal.New(fslocal.Config{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	service, err := sandboxpolicy.NewService(sandboxpolicy.Config{Mode: mode, WorkspaceRoot: root}, "")
	if err != nil {
		t.Fatal(err)
	}
	return New(local, service), root
}

func TestReadOnlyDeniesEveryMutation(t *testing.T) {
	root := t.TempDir()
	fenced, _ := newFence(t, "read-only", root)
	target, err := fenced.Resolve(context.Background(), filepath.Join(root, "doc.txt"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fenced.WriteText(context.Background(), target, "content", nil, nil)
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeSandboxDenied {
		t.Fatalf("read-only write must be FS_SANDBOX_DENIED: %v", err)
	}
	if !strings.Contains(err.Error(), "file access denied under read-only mode") {
		t.Fatalf("denial text: %v", err)
	}
	// Reads pass through untouched: every mode permits reading.
	if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := fenced.ReadText(context.Background(), target)
	if err != nil || content != "seed" {
		t.Fatalf("read under read-only: %q, %v", content, err)
	}
}

func TestWorkspaceWriteContainsAndAllowsTemp(t *testing.T) {
	root := t.TempDir()
	fenced, _ := newFence(t, "workspace-write", root)
	ctx := context.Background()

	// Inside the workspace root: allowed.
	inside, err := fenced.Resolve(ctx, filepath.Join(root, "doc.txt"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fenced.WriteText(ctx, inside, "content", nil, nil); err != nil {
		t.Fatalf("inside-root write: %v", err)
	}

	// The per-user platform temp dir is writable by the mode's promise.
	tempFile := filepath.Join(t.TempDir(), "scratch.txt")
	outside, err := fenced.Resolve(ctx, tempFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fenced.WriteText(ctx, outside, "temp", nil, nil); err != nil {
		t.Fatalf("platform-temp write: %v", err)
	}

	// A path outside every writable root is denied with the mode marker.
	// The drive/filesystem root is never beneath the workspace root or the
	// platform temp areas.
	volume := filepath.VolumeName(root)
	deniedPath := volume + string(os.PathSeparator) + "dsh-sandbox-denied-target.txt"
	if volume == "" {
		deniedPath = "/dsh-sandbox-denied-target.txt"
	}
	denied, err := fenced.Resolve(ctx, deniedPath, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fenced.WriteText(ctx, denied, "x", nil, nil)
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeSandboxDenied {
		t.Fatalf("outside-root write must be FS_SANDBOX_DENIED: %v", err)
	}
	if !strings.Contains(err.Error(), "file access denied under workspace-write mode") {
		t.Fatalf("denial text: %v", err)
	}
}

func TestDangerFullAccessDelegatesUnfenced(t *testing.T) {
	root := t.TempDir()
	fenced, _ := newFence(t, "danger-full-access", root)
	ctx := context.Background()
	foreign := filepath.Join(t.TempDir(), "anywhere.txt")
	target, err := fenced.Resolve(ctx, foreign, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fenced.WriteText(ctx, target, "unfenced", nil, nil); err != nil {
		t.Fatalf("danger-full-access write: %v", err)
	}
}

func TestPerCallPolicyOutranksDeploymentDefault(t *testing.T) {
	root := t.TempDir()
	fenced, _ := newFence(t, "read-only", root)
	ctx := context.Background()
	target, err := fenced.Resolve(ctx, filepath.Join(root, "doc.txt"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fenced.WriteText(ctx, target, "x", nil, nil); err == nil {
		t.Fatal("deployment read-only must deny")
	}
	wide := fs.SandboxExecutionPolicy{Mode: "danger-full-access", WorkspaceRoot: root}
	if _, err := fenced.WriteText(ctx, target, "escalated", nil, &wide); err != nil {
		t.Fatalf("per-call danger-full-access must pass: %v", err)
	}
}

func TestCasingAliasMatchesThroughIdentity(t *testing.T) {
	root := t.TempDir()
	fenced, _ := newFence(t, "workspace-write", root)
	ctx := context.Background()
	// Same root, different spelling: containment must still hold via the
	// case-insensitive lexical path or the identity fallback.
	flipped := root
	if len(flipped) > 0 {
		first := strings.ToUpper(flipped[:1])
		flipped = first + flipped[1:]
		if flipped == root {
			flipped = strings.ToLower(root[:1]) + root[1:]
		}
	}
	policy := sandboxpolicy.Config{Mode: "workspace-write", WorkspaceRoot: flipped}
	service, err := sandboxpolicy.NewService(policy, "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := fenced.Resolve(ctx, filepath.Join(root, "alias.txt"), "")
	if err != nil {
		t.Fatal(err)
	}
	perCall := service.Resolve("", "", "")
	if _, err := fenced.WriteText(ctx, target, "alias", nil, &perCall); err != nil {
		t.Fatalf("alias-spelled root write: %v", err)
	}
}

func TestContainmentIdentityFallbackAndRejection(t *testing.T) {
	root := t.TempDir()
	if !isPathUnder(filepath.Join(root, "deep", "file.txt"), root) {
		t.Fatal("containment must hold beneath the root")
	}
	if isPathUnder(filepath.Join(filepath.Dir(root), "elsewhere.txt"), root) {
		t.Fatal("an unrelated sibling must not match")
	}
	// A missing target under the root still contains lexically (the
	// missing suffix rides the canonical spelling).
	if !isPathUnder(filepath.Join(root, "not-yet-created", "new.txt"), root) {
		t.Fatal("missing suffix beneath the root must contain")
	}
}
