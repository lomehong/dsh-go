package persistence

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/session"
)

// --- revision / errors ----------------------------------------------------

func TestRevisionBranding(t *testing.T) {
	r := NewRevision("dev:ino:5:1:1")
	if r != "dev:ino:5:1:1" || string(r) == "" {
		t.Fatalf("revision = %v", r)
	}
}

func TestSessionFormatVersionRefusalDirections(t *testing.T) {
	newer := SessionFormatVersionRefusal("s1", session.SESSION_FORMAT_VERSION+1)
	if !strings.Contains(newer, "newer harness") || !strings.Contains(newer, "upgrade the harness") {
		t.Fatalf("newer = %q", newer)
	}
	older := SessionFormatVersionRefusal("s1", session.SESSION_FORMAT_VERSION-1)
	if !strings.Contains(older, "older than the supported") || strings.Contains(older, "corrupt") {
		t.Fatalf("older = %q", older)
	}
}

func TestNotFoundErrorAndCorruptionUnwrap(t *testing.T) {
	notFound := &NotFoundError{SessionID: "s1"}
	if notFound.Error() != `session "s1" not found` {
		t.Fatalf("not found = %q", notFound.Error())
	}
	cause := errors.New("seq 3 dangling")
	corruption := &CorruptionError{Message: "bad log", Cause: cause}
	if !errors.Is(corruption, cause) {
		t.Fatal("corruption must unwrap to its cause")
	}
	var unsupported *FormatUnsupportedError
	unsupported = &FormatUnsupportedError{Message: "refusal", Location: &Location{Path: "C:\\logs\\s1.session"}}
	if unsupported.Location.Path == "" {
		t.Fatal("location lost")
	}
}

// --- write-behind -----------------------------------------------------------

func wbEvent(seq int64) session.Event {
	data, _ := json.Marshal(map[string]any{"content": "m"})
	return session.Event{Type: "user/message", Seq: seq, Time: 1, Data: data}
}

func TestWriteBehindCoalescesWithinFixedWindow(t *testing.T) {
	var writes [][]int64
	var mu sync.Mutex
	ready := make(chan struct{}, 1)
	w := NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: 40 * time.Millisecond,
		Write: func(events []session.Event) error {
			mu.Lock()
			var seqs []int64
			for _, e := range events {
				seqs = append(seqs, e.Seq)
			}
			writes = append(writes, seqs)
			mu.Unlock()
			select {
			case ready <- struct{}{}:
			default:
			}
			return nil
		},
	})
	w.Enqueue(wbEvent(0))
	w.Enqueue(wbEvent(1))
	w.Enqueue(wbEvent(2))
	<-ready
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 {
		t.Fatalf("writes = %v", writes)
	}
	if len(writes[0]) != 3 || writes[0][0] != 0 || writes[0][2] != 2 {
		t.Fatalf("batch = %v", writes[0])
	}
}

func TestWriteBehindFlushDrainsAndJoins(t *testing.T) {
	var durable atomic.Int64
	release := make(chan struct{})
	w := NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: time.Hour, // never fires; flush must beat the window
		Write: func(events []session.Event) error {
			<-release
			durable.Add(int64(len(events)))
			return nil
		},
	})
	w.Enqueue(wbEvent(0))
	w.Enqueue(wbEvent(1))
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errCh <- w.Flush() }()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("flush 2: %v", err)
	}
	if durable.Load() != 2 {
		t.Fatalf("durable = %d", durable.Load())
	}
	if w.HasWork() {
		t.Fatal("quiesced controller still reports work")
	}
}

func TestWriteBehindForegroundFailureRetainsAndRetries(t *testing.T) {
	attempts := atomic.Int64{}
	w := NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: time.Hour,
		Write: func(events []session.Event) error {
			if attempts.Add(1) == 1 {
				return errors.New("disk full")
			}
			return nil
		},
	})
	w.Enqueue(wbEvent(0))
	w.Enqueue(wbEvent(1))
	if err := w.Flush(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("first flush err = %v", err)
	}
	// The failed batch stays retained in order for the retry.
	if !w.HasWork() {
		t.Fatal("failed batch was dropped")
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestWriteBehindBackgroundFailureReportsAndPauses(t *testing.T) {
	var failures atomic.Int64
	var mu sync.Mutex
	var secondGate = make(chan struct{})
	attempts := atomic.Int64{}
	w := NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: 20 * time.Millisecond,
		Write: func(events []session.Event) error {
			if attempts.Add(1) == 1 {
				return errors.New("provider 500")
			}
			<-secondGate
			return nil
		},
		ReportBackgroundFailure: func(err error) { failures.Add(1) },
	})
	w.Enqueue(wbEvent(0))
	// First background write fails after the deadline; automatic path pauses.
	time.Sleep(60 * time.Millisecond)
	if failures.Load() != 1 {
		t.Fatalf("failures = %d", failures.Load())
	}
	// The failed batch is retained.
	if !w.HasWork() {
		t.Fatal("retained batch lost")
	}
	// A fresh enqueue resumes the automatic path.
	mu.Lock()
	w.Enqueue(wbEvent(1))
	mu.Unlock()
	close(secondGate)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !w.HasWork() && attempts.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if attempts.Load() < 2 {
		t.Fatalf("attempts = %d; automatic path never resumed", attempts.Load())
	}
}

func TestWriteBehindEnqueueDuringBarrierStartsOwnWindow(t *testing.T) {
	gate := make(chan struct{})
	var firstBatch int
	w := NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: time.Hour,
		Write: func(events []session.Event) error {
			<-gate
			firstBatch += len(events)
			return nil
		},
	})
	w.Enqueue(wbEvent(0))
	flushErr := make(chan error, 1)
	go func() { flushErr <- w.Flush() }()
	time.Sleep(20 * time.Millisecond)
	// Enqueue while the barrier is active must not join the settled barrier.
	w.Enqueue(wbEvent(1))
	close(gate)
	if err := <-flushErr; err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if firstBatch != 2 {
		t.Fatalf("firstBatch = %d", firstBatch)
	}
}

func TestWriteBehindEnqueueClonesProducerEvent(t *testing.T) {
	var got []session.Event
	gate := make(chan struct{})
	w := NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: time.Hour,
		Write: func(events []session.Event) error {
			got = events
			<-gate
			return nil
		},
	})
	event := wbEvent(7)
	w.Enqueue(event)
	// Producer-side mutation after enqueue must not corrupt retained state.
	event.Data = json.RawMessage(`{"tampered":true}`)
	close(gate)
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if strings.Contains(string(got[0].Data), "tampered") {
		t.Fatalf("retained event aliased the producer's: %s", got[0].Data)
	}
}
