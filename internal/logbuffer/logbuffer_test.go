package logbuffer

import (
	"fmt"
	"testing"
	"time"
)

func TestWrite_StoresEntries(t *testing.T) {
	b := New(10)
	b.Write([]byte(`{"level":"info","message":"hello"}`))
	b.Write([]byte(`{"level":"warn","message":"world"}`))

	got := b.GetAll()
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if string(got[0]) != `{"level":"info","message":"hello"}` {
		t.Errorf("unexpected first entry: %s", got[0])
	}
}

func TestWrite_StripsTrailingNewline(t *testing.T) {
	b := New(10)
	b.Write([]byte("{\"msg\":\"hi\"}\n"))

	got := b.GetAll()
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	if string(got[0]) != `{"msg":"hi"}` {
		t.Errorf("trailing newline not stripped: %q", got[0])
	}
}

func TestWrite_EmptyLineIgnored(t *testing.T) {
	b := New(10)
	b.Write([]byte("\n"))
	b.Write([]byte(""))

	if len(b.GetAll()) != 0 {
		t.Errorf("empty writes should not add entries")
	}
}

func TestRingBuffer_EvictsOldestWhenFull(t *testing.T) {
	b := New(3)
	for i := range 5 {
		b.Write([]byte(fmt.Sprintf(`{"n":%d}`, i)))
	}

	got := b.GetAll()
	if len(got) != 3 {
		t.Fatalf("want 3 entries (ring size), got %d", len(got))
	}
	// Should contain entries 2, 3, 4
	if string(got[0]) != `{"n":2}` {
		t.Errorf("oldest entry not evicted: got %s", got[0])
	}
	if string(got[2]) != `{"n":4}` {
		t.Errorf("newest entry wrong: got %s", got[2])
	}
}

func TestSubscribe_ReceivesNewEntries(t *testing.T) {
	b := New(10)
	id, ch := b.Subscribe()
	defer b.Unsubscribe(id)

	b.Write([]byte(`{"level":"info","message":"streamed"}`))

	select {
	case line := <-ch:
		if string(line) != `{"level":"info","message":"streamed"}` {
			t.Errorf("unexpected line: %s", line)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber did not receive entry within 100ms")
	}
}

func TestUnsubscribe_ClosesChannel(t *testing.T) {
	b := New(10)
	id, ch := b.Subscribe()
	b.Unsubscribe(id)

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after Unsubscribe")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after Unsubscribe")
	}
}

func TestGetAll_ReturnsCopy(t *testing.T) {
	b := New(10)
	b.Write([]byte(`{"a":1}`))

	snap1 := b.GetAll()
	b.Write([]byte(`{"a":2}`))
	snap2 := b.GetAll()

	if len(snap1) != 1 {
		t.Errorf("first snapshot should have 1 entry, got %d", len(snap1))
	}
	if len(snap2) != 2 {
		t.Errorf("second snapshot should have 2 entries, got %d", len(snap2))
	}
}
