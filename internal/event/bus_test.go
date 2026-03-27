package event

import (
	"testing"
	"time"
)

func TestBusPublishSubscribe(t *testing.T) {
	bus := NewBus(nil)

	id, ch := bus.Subscribe(8)
	defer bus.Unsubscribe(id)

	e := Event{
		Time: time.Now(),
		Type: EventRunnerSpawned,
		Repo: "org/repo",
	}
	bus.Publish(e)

	select {
	case got := <-ch:
		if got.Type != e.Type {
			t.Fatalf("got type %q, want %q", got.Type, e.Type)
		}
		if got.Repo != e.Repo {
			t.Fatalf("got repo %q, want %q", got.Repo, e.Repo)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBusFanOut(t *testing.T) {
	bus := NewBus(nil)

	id1, ch1 := bus.Subscribe(4)
	defer bus.Unsubscribe(id1)
	id2, ch2 := bus.Subscribe(4)
	defer bus.Unsubscribe(id2)

	e := Event{Time: time.Now(), Type: EventDaemonStarted}
	bus.Publish(e)

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Type != e.Type {
				t.Fatalf("subscriber %d: got type %q, want %q", i, got.Type, e.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out", i)
		}
	}
}

func TestBusNonBlockingPublish(t *testing.T) {
	bus := NewBus(nil)

	// Buffer of 1, publish 3 — should not block.
	_, _ = bus.Subscribe(1)

	for i := 0; i < 3; i++ {
		bus.Publish(Event{Time: time.Now(), Type: EventError})
	}
	// If we get here without hanging, the test passes.
}

func TestBusUnsubscribe(t *testing.T) {
	bus := NewBus(nil)

	id, ch := bus.Subscribe(4)
	bus.Unsubscribe(id)

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}

	// Publishing after unsubscribe should not panic.
	bus.Publish(Event{Time: time.Now(), Type: EventError})
}
