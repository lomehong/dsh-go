package fslocal

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/fs"
)

// probeToInfo converts one stat into the seam's metadata shape.
func probeToInfo(path string, info os.FileInfo) *fs.Info {
	var size *int64
	if info.Mode().IsRegular() {
		s := info.Size()
		size = &s
	}
	probeType := fs.TypeOther
	if info.Mode().IsRegular() {
		probeType = fs.TypeFile
	} else if info.IsDir() {
		probeType = fs.TypeDirectory
	}
	return &fs.Info{
		Version: versionOf(info),
		Type:    probeType,
		Size:    size,
	}
}

// lstatType maps a non-following stat into the path-entry type vocabulary.
func lstatType(info os.FileInfo) string {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fs.TypeSymlink
	case info.Mode().IsRegular():
		return fs.TypeFile
	case info.IsDir():
		return fs.TypeDirectory
	default:
		return fs.TypeOther
	}
}

// readError converts one read failure into the seam taxonomy.
func readError(displayPath string, err error) error {
	if os.IsNotExist(err) {
		return fs.NewError(fs.CodeNotFound, fmt.Sprintf("cannot read %q: file not found", displayPath), err)
	}
	return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot read %q", displayPath), err)
}

// trailingPartialBytes reports how many trailing bytes of chunk are a
// possibly-split UTF-8 rune prefix (max 3).
func trailingPartialBytes(chunk []byte) int {
	limit := 3
	if len(chunk) < limit {
		limit = len(chunk)
	}
	for i := 1; i <= limit; i++ {
		b := chunk[len(chunk)-i]
		if b&0xC0 != 0x80 { // not a continuation byte
			if b&0x80 == 0 {
				return 0 // ASCII
			}
			// Incomplete multi-byte sequence at the tail.
			expected := 0
			switch {
			case b&0xE0 == 0xC0:
				expected = 2
			case b&0xF0 == 0xE0:
				expected = 3
			case b&0xF8 == 0xF0:
				expected = 4
			default:
				return 0
			}
			if i < expected {
				return i
			}
			return 0
		}
	}
	return limit
}

// normalizeLineEndings folds CRLF and lone CR into LF so edits and diff
// bases share one line-ending vocabulary.
func normalizeLineEndings(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

// applyLiteralEdit performs the literal search/replace with the official
// uniqueness discipline: zero matches is FS_EDIT_NOT_FOUND, more than one
// without replace_all is FS_AMBIGUOUS_EDIT.
func applyLiteralEdit(content string, edit fs.EditRequest, displayPath string) (string, int, error) {
	oldNorm := normalizeLineEndings(edit.OldString)
	if oldNorm == "" {
		return "", 0, fs.NewError(fs.CodeEditNotFound, "old_string must be a non-empty string", nil)
	}
	newNorm := normalizeLineEndings(edit.NewString)
	replacements := strings.Count(content, oldNorm)
	if replacements == 0 {
		return "", 0, fs.NewError(fs.CodeEditNotFound, fmt.Sprintf("old_string was not found in %q", displayPath), nil)
	}
	if !edit.ReplaceAll && replacements > 1 {
		return "", 0, fs.NewError(fs.CodeAmbiguousEdit, fmt.Sprintf("old_string matched %d times in %q; provide a more specific old_string or set replace_all to true", replacements, displayPath), nil)
	}
	return strings.ReplaceAll(content, oldNorm, newNorm), replacements, nil
}

// readDiffBasis is the best-effort overwrite diff basis: binary, invalid
// UTF-8, or a file at/above the byte limit returns an error so the write
// still succeeds and presentation falls back to a whole-file diff. The bound
// is enforced on the opened descriptor rather than a prior path stat.
func readDiffBasis(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() >= maxBytes {
		return nil, fmt.Errorf("fslocal: basis not diffable")
	}
	raw := make([]byte, info.Size())
	if _, err := file.Read(raw); err != nil && info.Size() > 0 {
		return nil, err
	}
	if bytes.ContainsRune(raw, 0) || !utf8Valid(raw) {
		return nil, fmt.Errorf("fslocal: basis not text")
	}
	return raw, nil
}

// writeFileAtomic publishes content through a same-directory temp file and a
// rename, preserving the prior mode.
func writeFileAtomic(path string, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".fslocal-*")
	if err != nil {
		return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot stage write to %q", path), err)
	}
	tempName := temp.Name()
	defer func() {
		if tempName != "" {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.WriteString(content); err != nil {
		temp.Close()
		return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot write %q", path), err)
	}
	if err := temp.Close(); err != nil {
		return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot flush %q", path), err)
	}
	if err := os.Chmod(tempName, mode); err != nil {
		return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot set mode on %q", path), err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot publish %q", path), err)
	}
	tempName = ""
	return nil
}

// utf8Valid mirrors unicode/utf8 for the local helper set.
func utf8Valid(raw []byte) bool {
	for len(raw) > 0 {
		r, size := decodeRune(raw)
		if r == 0xFFFD && size == 1 {
			return false
		}
		raw = raw[size:]
	}
	return true
}

// decodeRune decodes one rune with the standard replacement semantics.
func decodeRune(raw []byte) (rune, int) {
	b := raw[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b&0xE0 == 0xC0 && len(raw) >= 2:
		r, size := rune(b&0x1F), 2
		if cont(raw[1]) {
			return r<<6 | rune(raw[1]&0x3F), size
		}
	case b&0xF0 == 0xE0 && len(raw) >= 3:
		r, size := rune(b&0x0F), 3
		if cont(raw[1]) && cont(raw[2]) {
			return r<<12 | rune(raw[1]&0x3F)<<6 | rune(raw[2]&0x3F), size
		}
	case b&0xF8 == 0xF0 && len(raw) >= 4:
		r, size := rune(b&0x07), 4
		if cont(raw[1]) && cont(raw[2]) && cont(raw[3]) {
			return r<<18 | rune(raw[1]&0x3F)<<12 | rune(raw[2]&0x3F)<<6 | rune(raw[3]&0x3F), size
		}
	}
	return 0xFFFD, 1
}

func cont(b byte) bool { return b&0xC0 == 0x80 }
