package gatewaystream

import (
	"testing"
)

func TestRemoteEventQueueFIFOAndEnd(t *testing.T) {
	queue := NewRemoteEventQueue()
	queue.Push(WireFrame{Type: "emit", Event: "e1"})
	queue.Push(WireFrame{Type: "emit", Event: "e2"})
	frame, done := queue.Next()
	if done || frame.Event != "e1" {
		t.Fatalf("first = %+v done=%v", frame, done)
	}
	frame, done = queue.Next()
	if done || frame.Event != "e2" {
		t.Fatalf("second = %+v done=%v", frame, done)
	}
	queue.End()
	if _, done := queue.Next(); !done {
		t.Fatal("queue must end after End")
	}
	queue.Push(WireFrame{Type: "emit", Event: "late"})
	if _, done := queue.Next(); !done {
		t.Fatal("push after end must be dropped")
	}
}

func TestRemoteEventQueueConcurrentPush(t *testing.T) {
	queue := NewRemoteEventQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			queue.Push(WireFrame{Type: "emit", Event: "e"})
		}
		queue.End()
	}()
	count := 0
	for {
		_, ended := queue.Next()
		if ended {
			break
		}
		count++
	}
	<-done
	if count != 100 {
		t.Fatalf("count = %d, want 100", count)
	}
}
