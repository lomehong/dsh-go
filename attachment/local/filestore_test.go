package local

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"dshgo/attachment"
)

func TestFileLeafNameSanitizes(t *testing.T) {
	cases := map[string]string{
		"report.pdf":          "report.pdf",
		"":                    "file",
		`C:\Users\me\note.md`: "note.md",
		"/etc/passwd":         "passwd",
		"con.txt":             "_con.txt",
		"v1.0 . ":             "v1.0",
		"..":                  "file",
		"a<b>:c|d?e*f\"g.txt": "a_b__c_d_e_f_g.txt",
	}
	for input, want := range cases {
		if got := FileLeafName(input); got != want {
			t.Fatalf("FileLeafName(%q) = %q, want %q", input, got, want)
		}
	}
}

func newFileStore(t *testing.T) *AttachmentStore {
	t.Helper()
	return New(Config{DSHHome: t.TempDir()})
}

func TestSaveFileVerbatimRoundTrip(t *testing.T) {
	store := newFileStore(t)
	content := []byte("verbatim bytes \x00\x01\x02")
	ref, err := store.SaveFile(attachment.SaveFileAttachment{Data: content, Name: `dir\report.txt`})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	sum := sha256.Sum256(content)
	if ref.AttachmentID != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("reference digest: %s", ref.AttachmentID)
	}
	if ref.Name != "report.txt" || ref.Bytes != len(content) {
		t.Fatalf("reference meta: %+v", ref)
	}
	// The display-name path exists and ends in the real filename.
	host, ok, err := store.FileHostPath(ref)
	if err != nil || !ok {
		t.Fatalf("host path: %v ok=%v", err, ok)
	}
	if !strings.HasSuffix(host, `files`) && !strings.Contains(host, "report.txt") {
		t.Fatalf("host path shape: %s", host)
	}
	reader, err := ReadFileStreamVerbatim(store.Root(), ref)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	buffer := make([]byte, len(content)+8)
	total := 0
	for {
		n, readErr := reader.Read(buffer[total:])
		total += n
		if readErr != nil {
			break
		}
	}
	reader.Close()
	if string(buffer[:total]) != string(content) {
		t.Fatalf("verbatim round-trip mismatch: %q", buffer[:total])
	}
}

func TestSaveFileDedupSharesObject(t *testing.T) {
	store := newFileStore(t)
	content := []byte("shared bytes")
	first, err := store.SaveFile(attachment.SaveFileAttachment{Data: content, Name: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveFile(attachment.SaveFileAttachment{Data: content, Name: "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if first.AttachmentID != second.AttachmentID {
		t.Fatal("same bytes must share one digest")
	}
	if _, _, statErr := store.FileHostPath(first); statErr != nil {
		t.Fatalf("first alias: %v", statErr)
	}
	if _, _, statErr := store.FileHostPath(second); statErr != nil {
		t.Fatalf("second alias: %v", statErr)
	}
}

func TestSaveFileRefusesCorruptReference(t *testing.T) {
	store := newFileStore(t)
	ref := attachment.FileAttachmentRef{AttachmentID: "sha256:not-a-digest", Name: "x.txt", Bytes: 1}
	if _, _, err := store.FileHostPath(ref); err == nil {
		t.Fatal("an invalid reference must fail loud")
	}
}

func TestSaveFileStreamVerbatim(t *testing.T) {
	store := newFileStore(t)
	parts := [][]byte{[]byte("chunk-one "), []byte("chunk-two"), []byte(" chunk-three")}
	stream := &sliceReader{parts: parts}
	ref, err := store.SaveFileStream(attachment.SaveFileStreamAttachment{Data: stream, Name: "streamed.bin"})
	if err != nil {
		t.Fatalf("stream save: %v", err)
	}
	want := "chunk-one chunk-two chunk-three"
	if ref.Bytes != len(want) {
		t.Fatalf("streamed bytes: %d", ref.Bytes)
	}
	reader, err := ReadFileStreamVerbatim(store.Root(), ref)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(want)+4)
	total := 0
	for {
		n, readErr := reader.Read(buffer[total:])
		total += n
		if readErr != nil {
			break
		}
	}
	reader.Close()
	if string(buffer[:total]) != want {
		t.Fatalf("stream content: %q", buffer[:total])
	}
}

type sliceReader struct {
	parts [][]byte
	index int
	offset int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.index >= len(r.parts) {
		return 0, io.EOF
	}
	part := r.parts[r.index][r.offset:]
	n := copy(p, part)
	r.offset += n
	if r.offset >= len(r.parts[r.index]) {
		r.index++
		r.offset = 0
	}
	return n, nil
}
