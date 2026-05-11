package cache

import (
	"testing"

	"github.com/ylallemant/synergia/internal/manager/store"
)

// openTestStore opens a fully-migrated in-memory SQLite store for cache tests.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("openTestStore: %v", err)
	}
	return s
}

// seedActiveWorker creates a worker with a non-offline status and a gpu_avg value.
func seedActiveWorker(t *testing.T, s *store.Store, fingerprint string, gpuAvg int) {
	t.Helper()
	if err := s.UpsertWorker(fingerprint, "pubkey", "M", "Q4", "0.0.1", "linux", "amd64"); err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}
	if err := s.SetWorkerStatus(fingerprint, "available"); err != nil {
		t.Fatalf("SetWorkerStatus: %v", err)
	}
	if err := s.SetWorkerGPUAvg(fingerprint, gpuAvg); err != nil {
		t.Fatalf("SetWorkerGPUAvg: %v", err)
	}
}

// ── AvgWorkerGPU ──────────────────────────────────────────────────────────────

func TestAvgWorkerGPU_NoWorkers_ReturnsZero(t *testing.T) {
	s := openTestStore(t)
	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 0 {
		t.Errorf("want 0 with no workers, got %d", got)
	}
}

func TestAvgWorkerGPU_SingleWorker(t *testing.T) {
	s := openTestStore(t)
	seedActiveWorker(t, s, "fp-1", 40)
	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 40 {
		t.Errorf("want 40, got %d", got)
	}
}

func TestAvgWorkerGPU_MultipleWorkers_ComputesMean(t *testing.T) {
	s := openTestStore(t)
	seedActiveWorker(t, s, "fp-1", 20)
	seedActiveWorker(t, s, "fp-2", 40)
	seedActiveWorker(t, s, "fp-3", 60)
	// mean of 20, 40, 60 = 40
	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 40 {
		t.Errorf("want 40 (mean of 20,40,60), got %d", got)
	}
}

func TestAvgWorkerGPU_IgnoresOfflineWorkers(t *testing.T) {
	s := openTestStore(t)
	seedActiveWorker(t, s, "fp-active", 60)

	// Offline worker — should not influence the average.
	s.UpsertWorker("fp-offline", "pk", "M", "Q4", "0.0.1", "linux", "amd64") //nolint:errcheck
	// UpsertWorker leaves status as "online"; set it to offline explicitly.
	s.SetWorkerStatus("fp-offline", "offline")      //nolint:errcheck
	s.SetWorkerGPUAvg("fp-offline", 100)            //nolint:errcheck

	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 60 {
		t.Errorf("want 60 (offline worker excluded), got %d", got)
	}
}

func TestAvgWorkerGPU_IgnoresDeletedWorkers(t *testing.T) {
	s := openTestStore(t)
	seedActiveWorker(t, s, "fp-active", 30)

	s.UpsertWorker("fp-deleted", "pk", "M", "Q4", "0.0.1", "linux", "amd64") //nolint:errcheck
	s.SetWorkerStatus("fp-deleted", "deleted")  //nolint:errcheck
	s.SetWorkerGPUAvg("fp-deleted", 100)        //nolint:errcheck

	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 30 {
		t.Errorf("want 30 (deleted worker excluded), got %d", got)
	}
}

func TestAvgWorkerGPU_IgnoresZeroValues(t *testing.T) {
	s := openTestStore(t)
	// Worker with gpu_avg=0 has not reported yet — must not pull the average down.
	seedActiveWorker(t, s, "fp-reported", 80)
	seedActiveWorker(t, s, "fp-unreported", 0)

	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 80 {
		t.Errorf("want 80 (zero-value worker excluded), got %d", got)
	}
}

func TestAvgWorkerGPU_AllZero_ReturnsZero(t *testing.T) {
	s := openTestStore(t)
	seedActiveWorker(t, s, "fp-1", 0)
	seedActiveWorker(t, s, "fp-2", 0)

	c := New(s)
	if got := c.GetStats().AvgWorkerGPU; got != 0 {
		t.Errorf("want 0 when all workers have gpu_avg=0, got %d", got)
	}
}
