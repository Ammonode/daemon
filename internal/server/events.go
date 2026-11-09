package server

import "sync"

// Event types published to websocket subscribers. Names match the Pterodactyl
// websocket protocol so existing tooling keeps working.
const (
	EventConsoleOutput  = "console output"
	EventInstallOutput  = "install output"
	EventInstallStarted = "install started"
	EventInstallDone    = "install completed"
	EventStatus         = "status"
	EventStats          = "stats"
	EventDaemonMessage  = "daemon message"
	EventDeleted        = "deleted"
)

type Event struct {
	Name string   `json:"event"`
	Args []string `json:"args,omitempty"`
}

// Bus fans events out to subscribers without blocking the publisher.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

func (b *Bus) Subscribe() chan Event {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Bus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *Bus) Publish(name string, args ...string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- Event{Name: name, Args: args}:
		default: // slow consumer, drop
		}
	}
}

// RingBuffer keeps the last N console lines for clients that connect later.
type RingBuffer struct {
	mu    sync.Mutex
	lines []string
	size  int
}

func NewRingBuffer(size int) *RingBuffer { return &RingBuffer{size: size} }

func (r *RingBuffer) Add(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.size {
		r.lines = r.lines[len(r.lines)-r.size:]
	}
	r.mu.Unlock()
}

func (r *RingBuffer) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
