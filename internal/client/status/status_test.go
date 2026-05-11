package status

import (
	"context"
	"testing"
	"time"

	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/state"
)

// newState builds a fully-initialised State with sensible defaults so each
// test only needs to set the field(s) it cares about.
//   - connected=true, LLM reachable, GPU supported+available, not paused, not processing
func newState() *state.State {
	st := state.New("fp", "SmolLM2", "Q4_K_M")
	st.SetConnected(true, 0)
	st.SetLLM(true, "")
	st.SetGPU(gpu.StateAvailable, 0, gpu.GPUStats{}, true, "", "metal", "3.0")
	return st
}

// ── Compute() — priority chain ────────────────────────────────────────────────

func TestCompute_Disconnected(t *testing.T) {
	st := newState()
	st.SetConnected(false, 0)
	if got := New(st).Compute(); got != StatusDisconnected {
		t.Errorf("want %s, got %s", StatusDisconnected, got)
	}
}

func TestCompute_GPUUnsupported_BeatsLLMUnreachable(t *testing.T) {
	st := newState()
	// Both GPU unsupported AND LLM unreachable — GPU wins (priority 2 > 3).
	st.SetGPU(gpu.StateAvailable, 0, gpu.GPUStats{}, false, "no driver", "", "")
	st.SetLLM(false, "timeout")
	if got := New(st).Compute(); got != StatusGPUUnsupported {
		t.Errorf("want %s, got %s", StatusGPUUnsupported, got)
	}
}

func TestCompute_LLMUnreachable(t *testing.T) {
	st := newState()
	st.SetLLM(false, "connection refused")
	if got := New(st).Compute(); got != StatusLLMUnreachable {
		t.Errorf("want %s, got %s", StatusLLMUnreachable, got)
	}
}

func TestCompute_Paused_BeatsGPUBusy(t *testing.T) {
	st := newState()
	// Both paused AND GPU busy — paused wins (priority 4 > 6).
	st.SetPaused(true)
	st.SetGPU(gpu.StateBusy, 80, gpu.GPUStats{}, true, "", "metal", "3.0")
	if got := New(st).Compute(); got != StatusPaused {
		t.Errorf("want %s, got %s", StatusPaused, got)
	}
}

func TestCompute_Processing_BeatsGPUBusy(t *testing.T) {
	st := newState()
	// GPU is busy from our own inference — processing wins (priority 5 > 6).
	st.SetProcessing(true)
	st.SetGPU(gpu.StateBusy, 90, gpu.GPUStats{}, true, "", "metal", "3.0")
	if got := New(st).Compute(); got != StatusProcessing {
		t.Errorf("want %s, got %s", StatusProcessing, got)
	}
}

func TestCompute_GPUBusy(t *testing.T) {
	st := newState()
	st.SetGPU(gpu.StateBusy, 80, gpu.GPUStats{}, true, "", "metal", "3.0")
	if got := New(st).Compute(); got != StatusGPUBusy {
		t.Errorf("want %s, got %s", StatusGPUBusy, got)
	}
}

func TestCompute_Ready(t *testing.T) {
	st := newState()
	if got := New(st).Compute(); got != StatusReady {
		t.Errorf("want %s, got %s", StatusReady, got)
	}
}

// ── AddHandler + Run() ────────────────────────────────────────────────────────

func TestAddHandler_InitialFireWithEmptyOld(t *testing.T) {
	st := newState()
	p := New(st)

	var gotOld, gotNew string
	p.AddHandler(func(old, current string) {
		gotOld = old
		gotNew = current
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// Initial fire happens synchronously before the ticker starts.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if gotOld != "" {
		t.Errorf("initial fire: want old=%q, got %q", "", gotOld)
	}
	if gotNew != StatusReady {
		t.Errorf("initial fire: want new=%s, got %s", StatusReady, gotNew)
	}
}

func TestRun_FiresHandlerOnStatusChange(t *testing.T) {
	st := newState()
	p := New(st)

	changes := make(chan [2]string, 4)
	p.AddHandler(func(old, current string) {
		if old != "" {
			changes <- [2]string{old, current}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go p.Run(ctx)

	time.Sleep(50 * time.Millisecond) // let initial fire settle

	// Trigger a status change.
	st.SetConnected(false, 0)

	select {
	case change := <-changes:
		if change[0] != StatusReady || change[1] != StatusDisconnected {
			t.Errorf("want ready→disconnected, got %v→%v", change[0], change[1])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: no status change event received")
	}
}

func TestRun_DoesNotFireWhenStatusUnchanged(t *testing.T) {
	st := newState()
	p := New(st)

	fired := 0
	p.AddHandler(func(old, current string) {
		if old != "" {
			fired++
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go p.Run(ctx)
	<-ctx.Done()

	if fired != 0 {
		t.Errorf("expected no change events (status stayed ready), got %d", fired)
	}
}

func TestRun_MultipleHandlers_AllCalled(t *testing.T) {
	st := newState()
	p := New(st)

	var called [3]bool
	for i := range called {
		i := i
		p.AddHandler(func(old, current string) {
			if old == "" {
				called[i] = true
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	for i, c := range called {
		if !c {
			t.Errorf("handler %d was not called on initial fire", i)
		}
	}
}

func TestRun_MultipleTransitions(t *testing.T) {
	st := newState()
	p := New(st)

	var log []string
	p.AddHandler(func(old, current string) {
		if old != "" {
			log = append(log, old+"→"+current)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go p.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// Pause then resume.
	st.SetPaused(true)
	time.Sleep(1500 * time.Millisecond)
	st.SetPaused(false)
	time.Sleep(1500 * time.Millisecond)
	cancel()

	if len(log) < 2 {
		t.Fatalf("expected at least 2 transitions, got %d: %v", len(log), log)
	}
	if log[0] != StatusReady+"→"+StatusPaused {
		t.Errorf("first transition: want ready→paused, got %s", log[0])
	}
	if log[1] != StatusPaused+"→"+StatusReady {
		t.Errorf("second transition: want paused→ready, got %s", log[1])
	}
}
