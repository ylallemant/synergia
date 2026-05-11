package store

import "testing"

func TestSetWorkerGPUAvg_StoresValue(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-gpu-1", "")

	if err := s.SetWorkerGPUAvg("fp-gpu-1", 42); err != nil {
		t.Fatalf("SetWorkerGPUAvg: %v", err)
	}
	w, err := s.GetWorker("fp-gpu-1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.GPUAvg != 42 {
		t.Errorf("want GPUAvg=42, got %d", w.GPUAvg)
	}
}

func TestSetWorkerGPUAvg_UpdatesExistingValue(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-gpu-2", "")

	s.SetWorkerGPUAvg("fp-gpu-2", 30) //nolint:errcheck
	if err := s.SetWorkerGPUAvg("fp-gpu-2", 55); err != nil {
		t.Fatalf("second SetWorkerGPUAvg: %v", err)
	}
	w, _ := s.GetWorker("fp-gpu-2")
	if w.GPUAvg != 55 {
		t.Errorf("want GPUAvg=55 after update, got %d", w.GPUAvg)
	}
}

func TestSetWorkerGPUAvg_ZeroValue_Stored(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-gpu-3", "")
	s.SetWorkerGPUAvg("fp-gpu-3", 75) //nolint:errcheck

	if err := s.SetWorkerGPUAvg("fp-gpu-3", 0); err != nil {
		t.Fatalf("SetWorkerGPUAvg(0): %v", err)
	}
	w, _ := s.GetWorker("fp-gpu-3")
	if w.GPUAvg != 0 {
		t.Errorf("want GPUAvg=0, got %d", w.GPUAvg)
	}
}

func TestSetWorkerGPUAvg_UnknownFingerprint_ReturnsNoError(t *testing.T) {
	s := openTestStore(t)
	// GORM Update on a missing row is not an error — it just affects 0 rows.
	if err := s.SetWorkerGPUAvg("nonexistent-fp", 50); err != nil {
		t.Errorf("unexpected error for unknown fingerprint: %v", err)
	}
}
