package queue

import (
	"fmt"
	"sync"
	"time"

	"github.com/ylallemant/synergia/internal/manager/protocol"
)

// PendingUnit tracks a dispatched work unit awaiting a result.
type PendingUnit struct {
	ResultCh chan *protocol.Result
	ErrorCh  chan *protocol.Error
}

// Queue manages in-memory work unit dispatch and result routing.
type Queue struct {
	mu      sync.Mutex
	pending map[string]*PendingUnit
	counter uint64
}

func New() *Queue {
	return &Queue{
		pending: make(map[string]*PendingUnit),
	}
}

// NextID returns a unique work unit ID.
func (q *Queue) NextID() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.counter++
	return fmt.Sprintf("wu-%d-%d", time.Now().UnixMilli(), q.counter)
}

// Register creates a pending unit slot and returns channels to wait on.
func (q *Queue) Register(id string) *PendingUnit {
	q.mu.Lock()
	defer q.mu.Unlock()

	pu := &PendingUnit{
		ResultCh: make(chan *protocol.Result, 1),
		ErrorCh:  make(chan *protocol.Error, 1),
	}
	q.pending[id] = pu
	return pu
}

// Resolve delivers a result to the waiting caller. Returns false if no pending unit exists.
func (q *Queue) Resolve(id string, result *protocol.Result) bool {
	q.mu.Lock()
	pu, ok := q.pending[id]
	if ok {
		delete(q.pending, id)
	}
	q.mu.Unlock()

	if !ok {
		return false
	}
	pu.ResultCh <- result
	return true
}

// Reject delivers an error to the waiting caller. Returns false if no pending unit exists.
func (q *Queue) Reject(id string, err *protocol.Error) bool {
	q.mu.Lock()
	pu, ok := q.pending[id]
	if ok {
		delete(q.pending, id)
	}
	q.mu.Unlock()

	if !ok {
		return false
	}
	pu.ErrorCh <- err
	return true
}

// Cancel removes a pending unit (e.g., on timeout).
func (q *Queue) Cancel(id string) {
	q.mu.Lock()
	delete(q.pending, id)
	q.mu.Unlock()
}
