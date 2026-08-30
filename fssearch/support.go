package fssearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// errArgs is a plain tool-argument error (ordinary argument validation).
func errArgs(message string) error { return errors.New(message) }

func contextBackground() context.Context { return context.Background() }

func processCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func sep() string { return string(filepath.Separator) }

func isAbsPath(path string) bool { return filepath.IsAbs(path) }

func relPath(base, target string) (string, error) {
	return filepath.Rel(base, target)
}

func signalLabel(signal string) string {
	if signal == "" {
		return "(unknown)"
	}
	return signal
}

// headBytes bounds text to maxBytes, preserving UTF-8 boundaries.
func headBytes(text string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}
