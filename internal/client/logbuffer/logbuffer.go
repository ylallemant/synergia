package logbuffer

import (
	"bytes"
	"sync"
)

// Buffer is a thread-safe ring buffer that captures zerolog JSON output.
// It implements io.Writer so it can be added to zerolog's multi-writer.
// Subscribers receive new log lines in real-time via a channel (for SSE).
type Buffer struct {
	mu      sync.RWMutex
	entries [][]byte
	maxSize int
	subs    map[int]chan []byte
	nextID  int
}

func New(maxSize int) *Buffer {
	return &Buffer{
		maxSize: maxSize,
		subs:    make(map[int]chan []byte),
	}
}

// Write implements io.Writer. Each call is one zerolog JSON event (one line).
func (b *Buffer) Write(p []byte) (int, error) {
	line := bytes.TrimRight(p, "\n")
	if len(line) == 0 {
		return len(p), nil
	}
	entry := make([]byte, len(line))
	copy(entry, line)

	b.mu.Lock()
	b.entries = append(b.entries, entry)
	if len(b.entries) > b.maxSize {
		b.entries = b.entries[len(b.entries)-b.maxSize:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- entry:
		default: // subscriber is slow — drop rather than block
		}
	}
	b.mu.Unlock()
	return len(p), nil
}

// GetAll returns a snapshot of all buffered entries in order.
func (b *Buffer) GetAll() [][]byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([][]byte, len(b.entries))
	copy(result, b.entries)
	return result
}

// Subscribe registers a channel that receives each new log entry.
// Returns the subscriber ID needed for Unsubscribe.
func (b *Buffer) Subscribe() (int, <-chan []byte) {
	ch := make(chan []byte, 256)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()
	return id, ch
}

// Unsubscribe removes the subscriber and closes its channel.
func (b *Buffer) Unsubscribe(id int) {
	b.mu.Lock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
	b.mu.Unlock()
}
