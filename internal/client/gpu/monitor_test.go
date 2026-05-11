package gpu

import (
	"context"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// mockProber is a controllable Prober for unit tests.
type mockProber struct {
	util int
	err  error
}

func (p *mockProber) Supported() (bool, string) { return true, "" }
func (p *mockProber) DriverInfo() (string, string) { return "test-driver", "1.0" }
func (p *mockProber) Utilization() (int, error)    { return p.util, p.err }

// newTestMonitor returns a Monitor wired with a mockProber and no real ticker.
func newTestMonitor(thresh, baseline int) (*Monitor, *mockProber) {
	p := &mockProber{}
	m := &Monitor{
		interval:         time.Second,
		contentionThresh: thresh,
		resumeDelay:      time.Millisecond, // short so tests don't have to sleep long
		prober:           p,
		state:            StateAvailable,
		StateChangeCh:    make(chan StateChange, 5),
		baseline:         baseline,
	}
	return m, p
}

// inject feeds values directly into the rolling window, bypassing the prober.
// This mirrors exactly what check() does when recording a sample.
func inject(m *Monitor, values []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range values {
		m.windowBuf[m.windowHead] = v
		m.windowHead = (m.windowHead + 1) % len(m.windowBuf)
		if m.windowHead == 0 {
			m.windowFull = true
		}
	}
}

// repeat returns a slice of n copies of v.
func repeat(v, n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// ── Stats() ───────────────────────────────────────────────────────────────────

func TestStats_EmptyWindow_ReturnsZeroStruct(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	s := m.Stats()
	if s.SampleCount != 0 || s.Mean != 0 || s.BaselineMean != 0 {
		t.Errorf("expected zero GPUStats for empty window, got %+v", s)
	}
}

func TestStats_SingleSample_AllFieldsEqual(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	inject(m, []int{42})
	s := m.Stats()
	if s.SampleCount != 1 {
		t.Errorf("SampleCount: want 1, got %d", s.SampleCount)
	}
	if s.Mean != 42 || s.BaselineMean != 42 || s.Min != 42 || s.Max != 42 {
		t.Errorf("unexpected stats for single sample 42: %+v", s)
	}
}

func TestStats_UniformSamples_MeanEqualsBaselineMean(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	inject(m, repeat(30, 50))
	s := m.Stats()
	if s.Mean != 30 || s.BaselineMean != 30 {
		t.Errorf("want mean=baseline=30, got mean=%d baseline=%d", s.Mean, s.BaselineMean)
	}
	if s.Min != 30 || s.Max != 30 {
		t.Errorf("want min=max=30, got min=%d max=%d", s.Min, s.Max)
	}
}

func TestStats_MeanAndMinMax(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	// Known values: 10,20,30,40,50 → mean=30, min=10, max=50
	inject(m, []int{10, 20, 30, 40, 50})
	s := m.Stats()
	if s.Mean != 30 {
		t.Errorf("mean: want 30, got %d", s.Mean)
	}
	if s.Min != 10 {
		t.Errorf("min: want 10, got %d", s.Min)
	}
	if s.Max != 50 {
		t.Errorf("max: want 50, got %d", s.Max)
	}
}

func TestStats_BaselineMean_LowerThanMeanWhenPeaksPresent(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	// 90 samples at 10 (idle load) + 10 samples at 100 (gaming spikes)
	// Mean ≈ (90*10 + 10*100)/100 = 19
	// Sorted: 10×90 then 100×10; cutoff = 85 → all bottom 85 are value 10
	// BaselineMean = 10
	inject(m, repeat(10, 90))
	inject(m, repeat(100, 10))
	s := m.Stats()
	if s.SampleCount != 100 {
		t.Errorf("SampleCount: want 100, got %d", s.SampleCount)
	}
	if s.Mean != 19 {
		t.Errorf("Mean: want 19, got %d", s.Mean)
	}
	if s.BaselineMean != 10 {
		t.Errorf("BaselineMean: want 10, got %d", s.BaselineMean)
	}
	if s.BaselineMean >= s.Mean {
		t.Errorf("BaselineMean (%d) should be less than Mean (%d)", s.BaselineMean, s.Mean)
	}
}

func TestStats_SampleCount_PartialWindow(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	inject(m, repeat(5, 60))
	s := m.Stats()
	if s.SampleCount != 60 {
		t.Errorf("SampleCount: want 60, got %d", s.SampleCount)
	}
}

func TestStats_SampleCount_ExactlyFull(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	inject(m, repeat(5, 180)) // exactly fills the window
	s := m.Stats()
	if s.SampleCount != 180 {
		t.Errorf("SampleCount: want 180, got %d", s.SampleCount)
	}
}

func TestStats_WindowWraps_OldestSamplesEvicted(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	// Fill with 180 samples of value 5, then add 10 samples of value 50.
	// After wrap: window holds 170×5 + 10×50; oldest 10 (value 5) are gone.
	inject(m, repeat(5, 180))
	inject(m, repeat(50, 10))

	s := m.Stats()
	if s.SampleCount != 180 {
		t.Errorf("SampleCount: want 180, got %d", s.SampleCount)
	}
	if s.Min != 5 {
		t.Errorf("Min: want 5, got %d", s.Min)
	}
	if s.Max != 50 {
		t.Errorf("Max: want 50, got %d", s.Max)
	}
	// Mean: (170*5 + 10*50) / 180 = (850+500)/180 = 7
	if s.Mean != 7 {
		t.Errorf("Mean: want 7, got %d", s.Mean)
	}
	// cutoff = 180*85/100 = 153; bottom 153 are all value 5 → BaselineMean = 5
	if s.BaselineMean != 5 {
		t.Errorf("BaselineMean: want 5, got %d", s.BaselineMean)
	}
}

func TestStats_WindowWraps_EvictsAllOldValues(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	// Fill with 180 values of 100, then overwrite the whole window with 180 values of 20.
	inject(m, repeat(100, 180))
	inject(m, repeat(20, 180))
	s := m.Stats()
	if s.SampleCount != 180 {
		t.Errorf("SampleCount: want 180, got %d", s.SampleCount)
	}
	if s.Mean != 20 || s.Min != 20 || s.Max != 20 {
		t.Errorf("want all 20 after full overwrite, got %+v", s)
	}
}

// ── check() — utilization recording ──────────────────────────────────────────

func TestCheck_RecordsUtilizationToWindow(t *testing.T) {
	m, p := newTestMonitor(10, 0)
	p.util = 55
	m.check()
	if got := m.GetUtilization(); got != 55 {
		t.Errorf("GetUtilization: want 55, got %d", got)
	}
	s := m.Stats()
	if s.SampleCount != 1 || s.Mean != 55 {
		t.Errorf("Stats after one check: want count=1 mean=55, got %+v", s)
	}
}

func TestCheck_MultipleValues_BuildsWindow(t *testing.T) {
	m, p := newTestMonitor(10, 0)
	for _, v := range []int{10, 20, 30} {
		p.util = v
		m.check()
	}
	s := m.Stats()
	if s.SampleCount != 3 {
		t.Errorf("SampleCount: want 3, got %d", s.SampleCount)
	}
	if s.Mean != 20 {
		t.Errorf("Mean: want 20, got %d", s.Mean)
	}
}

// ── state transitions ─────────────────────────────────────────────────────────

func TestStateTransition_AvailableToBusy_OnHighContention(t *testing.T) {
	// baseline=10, threshold=15 → external load must exceed 15 → utilization > 25
	m, p := newTestMonitor(15, 10)
	p.util = 50 // external = 50-10 = 40 > 15 → contention
	m.check()

	if m.GetState() != StateBusy {
		t.Error("expected state to transition to Busy on high contention")
	}
	select {
	case change := <-m.StateChangeCh:
		if change.From != StateAvailable || change.To != StateBusy {
			t.Errorf("unexpected state change: %+v", change)
		}
	default:
		t.Error("expected StateChange event on StateChangeCh")
	}
}

func TestStateTransition_StaysAvailable_BelowThreshold(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	p.util = 20 // external = 20-10 = 10, not > 15 → no contention
	m.check()

	if m.GetState() != StateAvailable {
		t.Errorf("want StateAvailable, got %v", m.GetState())
	}
	if len(m.StateChangeCh) != 0 {
		t.Error("unexpected StateChange event")
	}
}

func TestStateTransition_BusyToAvailable_AfterContentionClears(t *testing.T) {
	m, p := newTestMonitor(15, 10)

	// Drive into Idle.
	p.util = 50
	m.check()
	if m.GetState() != StateBusy {
		t.Fatal("precondition: state must be Idle")
	}
	<-m.StateChangeCh // drain Idle event

	// Back-date contentionSince so the resume delay has already elapsed.
	m.mu.Lock()
	m.contentionSince = time.Now().Add(-time.Hour)
	m.mu.Unlock()

	// Low utilization — contention cleared.
	p.util = 5
	m.check()

	if m.GetState() != StateAvailable {
		t.Errorf("expected transition back to Available, got %v", m.GetState())
	}
	select {
	case change := <-m.StateChangeCh:
		if change.From != StateBusy || change.To != StateAvailable {
			t.Errorf("unexpected state change: %+v", change)
		}
	default:
		t.Error("expected StateChange event on StateChangeCh")
	}
}

func TestStateTransition_BusyStays_WhenStillContended(t *testing.T) {
	m, p := newTestMonitor(15, 10)

	// Drive into Idle.
	p.util = 50
	m.check()
	<-m.StateChangeCh

	// Still high — should remain Idle.
	m.mu.Lock()
	m.contentionSince = time.Now().Add(-time.Hour)
	m.mu.Unlock()

	p.util = 50
	m.check()

	if m.GetState() != StateBusy {
		t.Errorf("want StateBusy (contention ongoing), got %v", m.GetState())
	}
	if len(m.StateChangeCh) != 0 {
		t.Error("unexpected StateChange event while still contended")
	}
}

func TestStateTransition_BusyStays_ResumeDelayNotElapsed(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	m.resumeDelay = time.Hour // effectively never expires during this test

	// Drive into Idle.
	p.util = 50
	m.check()
	<-m.StateChangeCh

	// Utilization drops but resume delay hasn't passed.
	p.util = 5
	m.check()

	if m.GetState() != StateBusy {
		t.Errorf("want StateBusy (resume delay not elapsed), got %v", m.GetState())
	}
}

// ── SetBaseline feedback loop ─────────────────────────────────────────────────

func TestSetBaseline_LowBaseline_TriggersIdleEarlier(t *testing.T) {
	// With baseline=0 and threshold=10, any util >10 triggers idle.
	m, p := newTestMonitor(10, 0)
	p.util = 15 // external=15 > 10 → contention
	m.check()
	if m.GetState() != StateBusy {
		t.Fatal("expected Idle with low baseline and util=15")
	}
	<-m.StateChangeCh
}

func TestSetBaseline_HighBaseline_SuppressesContention(t *testing.T) {
	// After SetBaseline(80), threshold=10 → contention only when util > 90.
	m, p := newTestMonitor(10, 0)
	p.util = 15 // would trigger with baseline=0
	m.check()
	if m.GetState() != StateBusy {
		t.Fatal("precondition failed")
	}
	<-m.StateChangeCh

	// Reset to Available and raise baseline.
	m.mu.Lock()
	m.state = StateAvailable
	m.mu.Unlock()
	m.SetBaseline(80)

	p.util = 15 // external = 15-80 < 0 → no contention
	m.check()
	if m.GetState() != StateAvailable {
		t.Errorf("want Available after SetBaseline(80) with util=15, got %v", m.GetState())
	}
	if len(m.StateChangeCh) != 0 {
		t.Error("unexpected state change event")
	}
}

func TestBaselineMean_ReadyForFeedback_After60Samples(t *testing.T) {
	m, _ := newTestMonitor(10, 0)
	inject(m, repeat(20, 59))
	if s := m.Stats(); s.SampleCount >= 60 {
		t.Fatalf("precondition: want <60 samples, got %d", s.SampleCount)
	}
	inject(m, []int{20}) // 60th sample
	s := m.Stats()
	if s.SampleCount < 60 {
		t.Fatalf("want ≥60 samples after 60 injects, got %d", s.SampleCount)
	}
	if s.BaselineMean != 20 {
		t.Errorf("BaselineMean: want 20, got %d", s.BaselineMean)
	}
}

func TestSetBaseline_FromBaselineMean_AffectsNextContention(t *testing.T) {
	// Simulate the feedback loop: inject 60 samples of value 10 (idle load),
	// then call SetBaseline(BaselineMean) and verify a util=20 no longer triggers
	// idle (external load = 20-10 = 10, not > threshold 15).
	m, p := newTestMonitor(15, 0)
	inject(m, repeat(10, 60))
	stats := m.Stats()
	if stats.SampleCount < 60 {
		t.Fatalf("need 60 samples, got %d", stats.SampleCount)
	}
	m.SetBaseline(stats.BaselineMean) // baseline now = 10

	p.util = 20 // external = 20-10 = 10, threshold = 15 → no contention
	m.check()
	if m.GetState() != StateAvailable {
		t.Errorf("want Available (external load 10 ≤ threshold 15), got %v", m.GetState())
	}

	// Push well above threshold — should still trigger.
	p.util = 30 // external = 30-10 = 20 > 15 → contention
	m.check()
	if m.GetState() != StateBusy {
		t.Errorf("want Idle (external load 20 > threshold 15), got %v", m.GetState())
	}
}

// ── WaitForBaseline ───────────────────────────────────────────────────────────

func TestWaitForBaseline_ReturnsTrueWhenGPUDrops(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	// Start with high external load (above threshold).
	p.util = 80

	// After 200ms, drop utilisation below baseline+threshold.
	go func() {
		time.Sleep(200 * time.Millisecond)
		p.util = 5 // external = 5-10 = 0 ≤ 15 → below threshold
	}()

	ctx := context.Background()
	if !m.WaitForBaseline(ctx, 3*time.Second) {
		t.Error("WaitForBaseline should return true once GPU drops to baseline")
	}
}

func TestWaitForBaseline_ReturnsFalseOnTimeout(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	// GPU stays high — never drops below threshold.
	p.util = 80

	ctx := context.Background()
	start := time.Now()
	if m.WaitForBaseline(ctx, 600*time.Millisecond) {
		t.Error("WaitForBaseline should return false when GPU never drops")
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("expected to wait at least 500 ms, returned after %v", elapsed)
	}
}

func TestWaitForBaseline_ReturnsFalseOnCtxCancel(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	p.util = 80 // high — would normally wait

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	start := time.Now()
	if m.WaitForBaseline(ctx, 5*time.Second) {
		t.Error("WaitForBaseline should return false when ctx is already cancelled")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("WaitForBaseline should exit fast on cancelled ctx, took %v", elapsed)
	}
}

func TestWaitForBaseline_UpdatesLastUtilization(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	p.util = 42

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	m.WaitForBaseline(ctx, 600*time.Millisecond)

	if got := m.GetUtilization(); got != 42 {
		t.Errorf("WaitForBaseline should update lastUtilization; want 42, got %d", got)
	}
}

func TestWaitForBaseline_ReturnsTrueAlreadyAtBaseline(t *testing.T) {
	m, p := newTestMonitor(15, 10)
	p.util = 5 // external = 5-10 = 0 ≤ 15 — already at baseline

	if !m.WaitForBaseline(context.Background(), time.Second) {
		t.Error("WaitForBaseline should return true immediately when GPU is already at baseline")
	}
}

// ── State.String() ────────────────────────────────────────────────────────────

func TestStateString(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  string
	}{
		{StateAvailable, "available"},
		{StateBusy, "busy"},
		{State(99), "unknown"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String(): want %q, got %q", tc.state, tc.want, got)
		}
	}
}
