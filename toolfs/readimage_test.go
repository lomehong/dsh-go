package toolfs

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/attachment"
	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/fslocal"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

type fakeAttachmentStore struct {
	limits   attachment.ImageAttachmentLimits
	refusal  *attachment.AttachmentError
	lastSave attachment.SaveImageAttachment
}

func (s *fakeAttachmentStore) ImageLimits() attachment.ImageAttachmentLimits { return s.limits }

func (s *fakeAttachmentStore) SaveImage(input attachment.SaveImageAttachment) (attachment.ImageAttachmentRef, error) {
	if s.refusal != nil {
		return attachment.ImageAttachmentRef{}, s.refusal
	}
	s.lastSave = input
	return attachment.ImageAttachmentRef{
		AttachmentID: "att-1",
		MediaType:    input.MediaType,
		Bytes:        len(input.Data),
		Width:        2,
		Height:       2,
		Name:         input.Name,
	}, nil
}

type fakeLlmRoutes struct {
	modalities []string
}

func (f fakeLlmRoutes) ResolveModelInfo(provider, model string) (llm.LlmResolvedModelInfo, error) {
	if provider == "unknown" {
		return llm.LlmResolvedModelInfo{}, fmt.Errorf("no such provider")
	}
	return llm.LlmResolvedModelInfo{LlmModelInfo: llm.LlmModelInfo{Provider: provider, ID: model, InputModalities: f.modalities}}, nil
}

type noopSessionNotifications struct{}

func (noopSessionNotifications) Inserted(llm.Message)       {}
func (noopSessionNotifications) Discarded(llm.Message)      {}
func (noopSessionNotifications) Claimed(llm.Message, int64) {}

// newRoutedAgent builds one live scoped agent whose options name a route.
func newRoutedAgent(t *testing.T, registry *agent.AgentRegistry, provider, model string) *agent.Agent {
	t.Helper()
	id := session.SessionID(fmt.Sprintf("read-image-%d", registryCount(registry)))
	sess, err := session.NewDetached(id, nil, &session.SessionHeader{ID: id, CWD: "D:\\work"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopSessionNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{
		ID: sess.ID(), Options: agent.AgentOptions{Provider: provider, Model: model},
		Session: sess, Inbox: inbox,
	}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built
}

func registryCount(registry *agent.AgentRegistry) int {
	return len(registry.List())
}

func testImageLimits() attachment.ImageAttachmentLimits {
	return attachment.ImageAttachmentLimits{
		MaxImageBytes:        1 << 20,
		MaxImagesPerMessage:  5,
		MaxMessageImageBytes: 1 << 21,
		MaxImagePixels:       1 << 22,
		MaxImageDimension:    8000,
		MediaTypes:           []string{attachment.MediaPNG, attachment.MediaJPEG, attachment.MediaWebP, attachment.MediaGIF},
	}
}

func writeTinyPNG(t *testing.T, dir string) string {
	t.Helper()
	file := filepath.Join(dir, "picture.png")
	handle, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := png.Encode(handle, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestReadImageRegistersOnlyWithStore(t *testing.T) {
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	undo, err := Register(runtime, RegisterDeps{Backend: backend, Ctx: root}, DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if _, ok := runtime.Get("read_image", nil); ok {
		t.Fatal("read_image registered without an attachment store")
	}
}

func TestReadImageRoundTripAndEnvelope(t *testing.T) {
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	store := &fakeAttachmentStore{limits: testImageLimits()}
	undo, err := Register(runtime, RegisterDeps{
		Backend: backend, Ctx: root,
		Agents:      RegistryAgentSource{Registry: registry},
		Attachments: store,
		Llm:         fakeLlmRoutes{modalities: []string{"text", "image"}},
	}, DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	definition, ok := runtime.Get("read_image", nil)
	if !ok {
		t.Fatal("read_image not registered with a store")
	}

	agent1 := newRoutedAgent(t, registry, "deepseek", "vision-model")
	file := writeTinyPNG(t, backend.Cwd())
	outcome, err := definition.Execute(map[string]any{"file_path": file}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: agent1.Scope}, Signal: context.Background()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	value, ok := outcome.(map[string]any)
	if !ok || value["path"] != file {
		t.Fatalf("outcome: %+v", outcome)
	}
	ref, ok := value["image"].(attachment.ImageAttachmentRef)
	if !ok || ref.MediaType != attachment.MediaPNG || ref.Width != 2 {
		t.Fatalf("ref: %+v", ref)
	}
	if store.lastSave.MediaType != attachment.MediaPNG || filepath.Base(store.lastSave.Name) != "picture.png" {
		t.Fatalf("save: %+v", store.lastSave)
	}

	// The render carries the text envelope and the image block.
	blocks := definition.Render(nil, value)
	if len(blocks) != 2 || blocks[0].Type != llm.BlockText || blocks[1].Type != llm.BlockImage {
		t.Fatalf("blocks: %+v", blocks)
	}
	if !strings.Contains(blocks[0].Text, "<path>"+file+"</path>") ||
		!strings.Contains(blocks[0].Text, "image/png image, 2x2 px") {
		t.Fatalf("envelope: %q", blocks[0].Text)
	}
	if _, ok := blocks[1].Attachment.(attachment.ImageAttachmentRef); !ok {
		t.Fatalf("image block attachment: %+v", blocks[1].Attachment)
	}
}

func TestReadImageRefusalsBeforeAnyIO(t *testing.T) {
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	store := &fakeAttachmentStore{limits: testImageLimits()}
	undo, err := Register(runtime, RegisterDeps{
		Backend: backend, Ctx: root,
		Agents:      RegistryAgentSource{Registry: registry},
		Attachments: store,
		Llm:         fakeLlmRoutes{modalities: []string{"text"}},
	}, DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	definition, _ := runtime.Get("read_image", nil)
	agent1 := newRoutedAgent(t, registry, "deepseek", "text-only")
	run := func(args map[string]any) error {
		_, err := definition.Execute(args, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: agent1.Scope}, Signal: context.Background()})
		return err
	}

	file := writeTinyPNG(t, backend.Cwd())
	if err := run(map[string]any{"file_path": "   "}); err == nil || !strings.Contains(err.Error(), "file_path must be a non-empty string") {
		t.Fatalf("blank path: %v", err)
	}
	if err := run(map[string]any{"file_path": file + ".txt"}); err == nil ||
		!strings.Contains(err.Error(), "extension does not declare a supported image format") {
		t.Fatalf("bad extension: %v", err)
	}
	if err := run(map[string]any{"file_path": file}); err == nil ||
		!strings.Contains(err.Error(), `model "text-only" does not declare image input`) {
		t.Fatalf("modality gate: %v", err)
	}
	if store.lastSave.Data != nil {
		t.Fatal("refused read still reached the store")
	}
	if _, statErr := os.Stat(file); statErr != nil {
		t.Fatalf("source file touched: %v", statErr)
	}
}

func TestReadImageRouteUnresolvedWording(t *testing.T) {
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	undo, err := Register(runtime, RegisterDeps{
		Backend: backend, Ctx: root,
		Agents:      RegistryAgentSource{Registry: registry},
		Attachments: &fakeAttachmentStore{limits: testImageLimits()},
	}, DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	definition, _ := runtime.Get("read_image", nil)
	// Agent-less execution: no calling agent → route unresolved.
	_, err = definition.Execute(map[string]any{"file_path": "x.png"}, &tools.ToolRunContext{Signal: context.Background()})
	if err == nil || !strings.Contains(err.Error(), "the current model route could not be resolved") {
		t.Fatalf("route gate: %v", err)
	}
}

func TestReadImageRefusalMapping(t *testing.T) {
	displayPath := "D:\\work\\picture.png"
	limits := testImageLimits()
	cases := []struct {
		code string
		want string
	}{
		{attachment.CodeImageDimTooLarge, "at least one image side exceeds the 8000px limit"},
		{attachment.CodeImageTooManyPixels, "exceeds the 4194304-pixel decoded-size limit"},
		{attachment.CodeImageTooLarge, "cannot be stored within the deployment's byte limits"},
		{attachment.CodeImageTypeMismatch, "the .png extension declares image/png"},
	}
	for _, tc := range cases {
		err := mapAttachmentRefusal(&attachment.AttachmentError{Code: tc.code, Message: "nope"}, displayPath, attachment.MediaPNG, limits)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.code, err)
		}
	}
	if err := mapAttachmentRefusal(&attachment.AttachmentError{Code: attachment.CodeAttachmentWriteFailed, Message: "failed writing 16-bit PNG sample"}, displayPath, attachment.MediaPNG, limits); err == nil ||
		!strings.Contains(err.Error(), "16-bit PNG could not be converted") {
		t.Fatalf("16-bit png: %v", err)
	}
	if err := mapAttachmentRefusal(&attachment.AttachmentError{Code: attachment.CodeAttachmentWriteFailed, Message: "disk full"}, displayPath, attachment.MediaPNG, limits); err == nil ||
		strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("unmapped write failure must pass through: %v", err)
	}
	if err := mapAttachmentRefusal(errors.New("plain"), displayPath, attachment.MediaPNG, limits); err == nil ||
		!strings.Contains(err.Error(), "plain") {
		t.Fatalf("non-attachment error: %v", err)
	}
}

var _ = fs.CodeNotFound

func TestSniffImageMediaType(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00}, attachment.MediaPNG},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}, attachment.MediaJPEG},
		{"gif87", []byte("GIF87a..."), attachment.MediaGIF},
		{"gif89", []byte("GIF89a..."), attachment.MediaGIF},
		{"webp", append([]byte("RIFF"), append(make([]byte, 4), []byte("WEBP")...)...), attachment.MediaWebP},
		{"empty", nil, ""},
		{"garbage", []byte("not an image"), ""},
		{"truncated-png", []byte{0x89, 0x50, 0x4e}, ""},
	}
	for _, tc := range cases {
		if got := sniffImageMediaType(tc.data); got != tc.want {
			t.Fatalf("%s: sniff = %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadImageExtensionlessPathSniffs(t *testing.T) {
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	store := &fakeAttachmentStore{limits: testImageLimits()}
	undo, err := Register(runtime, RegisterDeps{
		Backend: backend, Ctx: root,
		Agents:      RegistryAgentSource{Registry: registry},
		Attachments: store,
		Llm:         fakeLlmRoutes{modalities: []string{"text", "image"}},
	}, DefaultCaps())
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	definition, ok := runtime.Get("read_image", nil)
	if !ok {
		t.Fatal("read_image not registered with a store")
	}
	agent1 := newRoutedAgent(t, registry, "deepseek", "vision-model")
	// An extension-less file is accepted and identified from its signature.
	extless := filepath.Join(backend.Cwd(), "attachment-object")
	pngBytes, err := os.ReadFile(writeTinyPNG(t, backend.Cwd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extless, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	outcome, err := definition.Execute(map[string]any{"file_path": extless}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: agent1.Scope}, Signal: context.Background()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	value, ok := outcome.(map[string]any)
	if !ok {
		t.Fatalf("outcome: %+v", outcome)
	}
	ref, ok := value["image"].(attachment.ImageAttachmentRef)
	if !ok || ref.MediaType != attachment.MediaPNG {
		t.Fatalf("ref: %+v", ref)
	}
	if store.lastSave.MediaType != attachment.MediaPNG {
		t.Fatalf("save: %+v", store.lastSave)
	}

	// An extension-less file with unrecognized bytes is refused.
	junk := filepath.Join(backend.Cwd(), "not-an-image")
	if err := os.WriteFile(junk, []byte("definitely not an image signature"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Execute(map[string]any{"file_path": junk}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: agent1.Scope}, Signal: context.Background()}); err == nil ||
		!strings.Contains(err.Error(), "file content is not a supported image format") {
		t.Fatalf("junk extension-less: %v", err)
	}
}
