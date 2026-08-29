package subprocess

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// spillDirOverride is the injectable spill directory for tests.
var spillDirOverride string

// outputCollector collects one stream with a bounded in-memory tail. With a
// spill cap, on first overflow a spill file is created and every chunk
// (including those already collected) is appended there while the full
// stream remains within the cap; without one, only the in-memory tail is
// ever retained (the diagnostic-tail shape — a language server's stderr).
//
// Tail-keep rationale (pi/OpenCode): errors and final results cluster at the
// end of command output; the spill file covers the head.
type outputCollector struct {
	mu             sync.Mutex
	maxBytes       int
	maxSpillBytes  int
	label          string
	spillDir       string
	chunks         [][]byte
	bytes          int
	dropped        bool
	spillFile      *os.File
	spillPath      string
	spillDisabled  bool
	total          int // total bytes ever pushed, not just retained
	spillCounter   int
	spillDirUsed   string
	spillPathAlive bool
}

func newOutputCollector(maxBytes, maxSpillBytes int, label, spillDir string) *outputCollector {
	return &outputCollector{
		maxBytes:       maxBytes,
		maxSpillBytes:  maxSpillBytes,
		label:          label,
		spillDir:       spillDir,
		spillDisabled:  maxSpillBytes <= 0,
		spillPathAlive: true,
	}
}

// push ingests one stream chunk, counting it toward the whole-stream total.
// On first overflow of the in-memory cap a spill file is opened (when
// spilling is enabled) and every chunk (already-collected ones included) is
// appended there from then on; the in-memory tail then drops whole chunks
// from its head (or the head of a single over-cap chunk) until it fits the
// cap again.
func (c *outputCollector) push(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += len(chunk)
	overflows := c.bytes+len(chunk) > c.maxBytes
	if !c.spillDisabled && (overflows || c.spillFile != nil) {
		c.spillAllLocked(chunk)
	}
	tail := make([]byte, len(chunk))
	copy(tail, chunk)
	c.chunks = append(c.chunks, tail)
	c.bytes += len(chunk)
	for c.bytes > c.maxBytes {
		head := c.chunks[0]
		excess := c.bytes - c.maxBytes
		if len(head) <= excess {
			// Drop the whole head chunk (length ≥ 1 is guaranteed while over cap).
			c.chunks = c.chunks[1:]
			c.bytes -= len(head)
		} else {
			// Trim the head so the retained window is byte-exact at the cap —
			// a diagnostic tail must hold the LAST maxBytes regardless of how
			// the stream was chunked.
			c.chunks[0] = head[excess:]
			c.bytes -= excess
		}
		c.dropped = true
	}
}

// spillAllLocked opens the spill file lazily and appends chunk (and any
// prior chunks once).
func (c *outputCollector) spillAllLocked(chunk []byte) {
	if c.maxSpillBytes > 0 && c.total > c.maxSpillBytes {
		c.discardSpillLocked()
		return
	}
	if c.spillFile == nil {
		dir := c.spillDir
		if spillDirOverride != "" {
			dir = spillDirOverride
		}
		if dir == "" {
			dir = defaultSpillDir()
		}
		// Random suffix + O_EXCL (CreateTemp): defeats spill-path prediction
		// and symlink planting in shared tmp dirs.
		file, err := os.CreateTemp(dir, fmt.Sprintf("dsh-subprocess-%d-%s-*.log", os.Getpid(), c.label))
		if err != nil {
			// A failed open leaves the in-memory tail authoritative.
			c.spillDisabled = true
			return
		}
		c.spillCounter++
		c.spillFile = file
		c.spillPath = file.Name()
		for _, prior := range c.chunks {
			_, _ = file.Write(prior)
		}
	}
	_, _ = c.spillFile.Write(chunk)
}

// discardSpillLocked stops spilling and removes the file once it can no
// longer hold the complete stream.
func (c *outputCollector) discardSpillLocked() {
	if c.spillFile != nil {
		_ = c.spillFile.Close()
		_ = os.Remove(c.spillPath)
	}
	c.spillFile = nil
	c.spillPath = ""
	c.spillDisabled = true
	c.spillPathAlive = false
}

// readFrom implements the incremental read in whole-stream byte
// coordinates: returns everything pushed since fromByte. When fromByte has
// already slid out of the in-memory tail window, the read is lossy — it
// returns the whole retained tail and the gap is only recoverable from the
// spill file.
func (c *outputCollector) readFrom(fromByte int) OutputRead {
	c.mu.Lock()
	defer c.mu.Unlock()
	windowStart := c.total - c.bytes
	var window []byte
	for _, chunk := range c.chunks {
		window = append(window, chunk...)
	}
	lossy := fromByte < windowStart
	var text []byte
	if lossy {
		text = window
	} else if fromByte-windowStart < len(window) {
		text = window[fromByte-windowStart:]
	}
	out := OutputRead{
		Text:       string(text),
		NextOffset: c.total,
		Lossy:      lossy,
	}
	if c.spillPath != "" && c.spillPathAlive {
		out.SpillPath = c.spillPath
	}
	return out
}

// seal closes the spill file once the stream has ended. A failed close
// stops advertising the spill path — the file may be missing its tail —
// while every in-memory read keeps working. Idempotent; the spawn path
// seals both collectors at settlement so reads after exit never point at a
// still-open file.
func (c *outputCollector) seal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spillFile == nil {
		return
	}
	if err := c.spillFile.Close(); err != nil {
		// A delayed writeback failure makes the spill unreliable; keep the
		// in-memory result but stop advertising that file.
		c.spillPathAlive = false
	}
	c.spillFile = nil
}

// finalize seals the spill file and returns the final output.
func (c *outputCollector) finalize() CollectedOutput {
	c.seal()
	c.mu.Lock()
	defer c.mu.Unlock()
	var window []byte
	for _, chunk := range c.chunks {
		window = append(window, chunk...)
	}
	out := CollectedOutput{
		Text:      string(window),
		Truncated: c.dropped,
	}
	if c.spillPath != "" && c.spillPathAlive {
		out.SpillPath = c.spillPath
	}
	return out
}

// defaultSpillDir creates the per-process private spill directory.
func defaultSpillDir() string {
	dir, err := os.MkdirTemp("", "dsh-subprocess-")
	if err != nil {
		return os.TempDir()
	}
	return dir
}

// randomHex is unused placeholder for the historical naming note.
var _ = hex.EncodeToString
var _ = rand.Read
var _ = filepath.Join
