package event

import "sync"

// Bus is a fan-out event bus. It delivers published events to every
// active subscriber on a best-effort, non-blocking basis.
type Bus struct {
	mu    sync.Mutex
	subs  map[int]chan Event
	next  int
	store *FileStore // may be nil
}

// NewBus creates a new Bus. If store is non-nil every published event
// is also persisted to the store.
func NewBus(store *FileStore) *Bus {
	return &Bus{
		subs:  make(map[int]chan Event),
		store: store,
	}
}

// Publish sends e to all subscribers using a non-blocking send and
// persists the event to the store (if configured).
func (b *Bus) Publish(e Event) {
	if b.store != nil {
		// Best-effort persistence; callers should not block on store errors.
		_ = b.store.Append(e)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// subscriber is slow — drop the event for this subscriber
		}
	}
}

// Subscribe registers a new subscriber and returns a unique id together
// with a channel that will receive events. bufSize controls the channel
// buffer size.
func (b *Bus) Subscribe(bufSize int) (int, <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.next
	b.next++
	ch := make(chan Event, bufSize)
	b.subs[id] = ch
	return id, ch
}

// Unsubscribe removes the subscriber identified by id and closes its
// channel.
func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
	}
}
