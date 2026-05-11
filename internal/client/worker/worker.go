package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/backend"
	"github.com/ylallemant/synergia/internal/client/connection"
	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/identity"
	"github.com/ylallemant/synergia/internal/client/llm"
	"github.com/ylallemant/synergia/internal/client/updater"
	"github.com/ylallemant/synergia/internal/protocol"
)

// Trigger payloads for testing error reporting.
const (
	TriggerPanic = "##############PANIC##############"
	TriggerError = "##############ERROR##############"
	TriggerPause = "##############PAUSE##############"
)

// UnitCounter is called after each successful work unit completion.
type UnitCounter interface {
	IncrementUnits()
}

// ProcessingTracker tracks whether the worker is actively processing a work unit.
type ProcessingTracker interface {
	SetProcessing(bool)
}

// PauseChecker reports whether the worker has been paused by the user.
type PauseChecker interface {
	IsPaused() bool
	SetPaused(bool)
}

// ErrorReporter reports errors to the cluster manager.
type ErrorReporter interface {
	Report(err error)
	ReportMessage(msg string)
	ReportWithStack(err error, stack string)
}

// ConsentChecker reports whether the worker has accepted data collection terms.
type ConsentChecker interface {
	IsAccepted() bool
}

// Worker processes work units received from the cluster manager.
type Worker struct {
	conn           *connection.Connection
	llm            *llm.Client
	id             *identity.Identity
	monitor        *gpu.Monitor
	counter        UnitCounter
	processing     ProcessingTracker
	pause          PauseChecker
	reporter       ErrorReporter
	consent        ConsentChecker
	updater        *updater.Updater
	backendMgr     *backend.Manager
	restartFn      func()       // called after successful binary update
	backendRestart func() error // called after backend binary update (restart llama-server)
	restartLLM     func(modelPath string, p backend.LlamaParams) error // called after model update
	role           string
	modelsDir      string
	managerHTTPURL string
	workerKey      string
}

func New(conn *connection.Connection, llmClient *llm.Client, id *identity.Identity, monitor *gpu.Monitor, counter UnitCounter, processing ProcessingTracker, pause PauseChecker, reporter ErrorReporter, consent ConsentChecker) *Worker {
	return &Worker{
		conn:       conn,
		llm:        llmClient,
		id:         id,
		monitor:    monitor,
		counter:    counter,
		processing: processing,
		pause:      pause,
		reporter:   reporter,
		consent:    consent,
	}
}

// SetModelDownloadConfig configures the worker for model downloads from the manager.
func (w *Worker) SetModelDownloadConfig(role, modelsDir, managerHTTPURL, workerKey string) {
	w.role = role
	w.modelsDir = modelsDir
	w.managerHTTPURL = managerHTTPURL
	w.workerKey = workerKey
}

// SetUpdater configures the binary auto-updater. restartFn is called after a successful update.
func (w *Worker) SetUpdater(u *updater.Updater, restartFn func()) {
	w.updater = u
	w.restartFn = restartFn
}

// SetBackendManager configures the backend binary manager.
// backendRestart is called after a successful backend binary update (should restart llama-server).
func (w *Worker) SetBackendManager(mgr *backend.Manager, backendRestart func() error) {
	w.backendMgr = mgr
	w.backendRestart = backendRestart
}

// SetLLMRestarter configures the callback invoked after a model update to restart llama-server
// with the new model file and parameters.
func (w *Worker) SetLLMRestarter(fn func(modelPath string, p backend.LlamaParams) error) {
	w.restartLLM = fn
}

// Run starts the work processing loop. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Info().Msg("worker processing loop started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("worker processing loop stopped")
			return

		case wu := <-w.conn.WorkUnitCh:
			w.process(ctx, wu)

		case mu := <-w.conn.ModelUpdateCh:
			w.handleModelUpdate(mu)

		case bu := <-w.conn.BinaryUpdateCh:
			w.handleBinaryUpdate(bu)

		case bu := <-w.conn.BackendUpdateCh:
			w.handleBackendUpdate(bu)

		case change := <-w.monitor.StateChangeCh:
			w.handleStateChange(change)
		}
	}
}

func (w *Worker) process(ctx context.Context, wu *protocol.WorkUnit) {
	// Recover from panics — report to manager and send error back
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			errMsg := fmt.Sprintf("panic in worker: %v", r)
			log.Error().Str("id", wu.ID).Str("panic", fmt.Sprint(r)).Msg("recovered from panic")
			if w.reporter != nil {
				w.reporter.ReportWithStack(fmt.Errorf("%s", errMsg), stack)
			}
			_ = w.conn.Send(&protocol.Error{
				Type:  protocol.TypeError,
				ID:    wu.ID,
				Error: errMsg,
			})
			w.processing.SetProcessing(false)
			w.monitor.SetProcessing(false)
			_ = w.conn.SendStatus("available")
		}
	}()

	// Check if worker is paused (allow PAUSE trigger through to toggle state)
	if w.pause.IsPaused() && detectTrigger(wu) != TriggerPause {
		log.Warn().Str("id", wu.ID).Msg("rejecting work unit — worker is paused")
		_ = w.conn.Send(&protocol.Error{
			Type:  protocol.TypeError,
			ID:    wu.ID,
			Error: "worker paused by user",
		})
		return
	}

	// Check consent — reject if revoked
	if w.consent != nil && !w.consent.IsAccepted() {
		log.Warn().Str("id", wu.ID).Msg("rejecting work unit — consent revoked")
		_ = w.conn.Send(&protocol.Error{
			Type:  protocol.TypeError,
			ID:    wu.ID,
			Error: "worker consent revoked",
		})
		return
	}

	// Check GPU state — if idle, reject work
	if w.monitor.GetState() == gpu.StateBusy {
		log.Warn().Str("id", wu.ID).Msg("rejecting work unit — GPU contention active")
		_ = w.conn.Send(&protocol.Error{
			Type:  protocol.TypeError,
			ID:    wu.ID,
			Error: "worker idle: GPU contention detected",
		})
		return
	}

	w.processing.SetProcessing(true)
	w.monitor.SetProcessing(true)
	_ = w.conn.SendStatus("processing")

	log.Info().Str("id", wu.ID).Str("model", wu.Model).Msg("processing work unit")

	// Check for test trigger payloads
	if trigger := detectTrigger(wu); trigger != "" {
		switch trigger {
		case TriggerPause:
			wasPaused := w.pause.IsPaused()
			newState := !wasPaused
			w.pause.SetPaused(newState)
			stateStr := "paused"
			if !newState {
				stateStr = "available"
			}
			log.Warn().Str("id", wu.ID).Bool("paused", newState).Msg("trigger payload detected: PAUSE toggle")
			_ = w.conn.SendStatus(stateStr)

			// Return a result acknowledging the toggle
			output, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{
					{"index": 0, "message": map[string]string{"role": "assistant", "content": fmt.Sprintf("pause toggled: %s", stateStr)}, "finish_reason": "stop"},
				},
			})
			result := &protocol.Result{
				Type:             protocol.TypeResult,
				ID:               wu.ID,
				Fingerprint:      w.id.Fingerprint,
				Output:           output,
				ProcessingTimeMs: 0,
				Signature:        w.id.Sign(canonicalSignaturePayload(wu.ID, output, 0)),
			}
			_ = w.conn.Send(result)
			w.processing.SetProcessing(false)
			w.monitor.SetProcessing(false)
			return
		case TriggerPanic:
			log.Warn().Str("id", wu.ID).Msg("trigger payload detected: PANIC")
			panic("intentional panic triggered by test payload")
		case TriggerError:
			log.Warn().Str("id", wu.ID).Msg("trigger payload detected: ERROR")
			errMsg := "intentional error triggered by test payload"
			if w.reporter != nil {
				w.reporter.ReportMessage(errMsg)
			}
			_ = w.conn.Send(&protocol.Error{
				Type:  protocol.TypeError,
				ID:    wu.ID,
				Error: errMsg,
			})
			w.processing.SetProcessing(false)
			w.monitor.SetProcessing(false)
			_ = w.conn.SendStatus("available")
			return
		}
	}

	start := time.Now()

	// Forward to local llama-server
	output, err := w.llm.Complete(ctx, wu)
	if err != nil {
		log.Error().Str("id", wu.ID).Err(err).Msg("llm processing failed")
		if w.reporter != nil {
			w.reporter.Report(err)
		}
		_ = w.conn.Send(&protocol.Error{
			Type:  protocol.TypeError,
			ID:    wu.ID,
			Error: err.Error(),
		})
		w.processing.SetProcessing(false)
		w.monitor.SetProcessing(false)
		_ = w.conn.SendStatus("available")
		return
	}

	processingTime := time.Since(start).Milliseconds()

	// Sign the result
	sigData := canonicalSignaturePayload(wu.ID, output, processingTime)
	signature := w.id.Sign(sigData)

	result := &protocol.Result{
		Type:             protocol.TypeResult,
		ID:               wu.ID,
		Fingerprint:      w.id.Fingerprint,
		Output:           output,
		ProcessingTimeMs: processingTime,
		Signature:        signature,
	}

	if err := w.conn.Send(result); err != nil {
		log.Error().Str("id", wu.ID).Err(err).Msg("failed to send result")
		if w.reporter != nil {
			w.reporter.Report(err)
		}
		w.processing.SetProcessing(false)
		w.monitor.SetProcessing(false)
		_ = w.conn.SendStatus("available")
		return
	}

	if w.counter != nil {
		w.counter.IncrementUnits()
	}

	// Wait for the GPU to cool back down to baseline before declaring the
	// worker available again. This prevents the monitor from seeing residual
	// inference heat on its next 5 s tick and falsely flagging it as external
	// contention. Cap at 15 s: if the GPU is still hot then the user started
	// something GPU-intensive during inference, so we report available anyway
	// and let the monitor detect real contention on the next tick.
	w.monitor.WaitForBaseline(ctx, 15*time.Second)

	w.processing.SetProcessing(false)
	w.monitor.SetProcessing(false)
	_ = w.conn.SendStatus("available")

	log.Info().Str("id", wu.ID).Int64("processing_time_ms", processingTime).Msg("work unit completed")
}

func (w *Worker) handleStateChange(change gpu.StateChange) {
	log.Info().Str("from", change.From.String()).Str("to", change.To.String()).Msg("GPU state change")

	if err := w.conn.SendStatus(change.To.String()); err != nil {
		log.Warn().Err(err).Msg("failed to send status update")
	}
}

func (w *Worker) handleModelUpdate(mu *protocol.ModelUpdate) {
	log.Info().
		Str("role", mu.Role).
		Str("model", mu.Model).
		Str("quantisation", mu.Quantisation).
		Str("filename", mu.Filename).
		Str("expected_hash", mu.LLMHash).
		Str("endpoint_type", mu.EndpointType).
		Int("context_size", mu.ContextSize).
		Int("parallel_slots", mu.ParallelSlots).
		Int("gpu_layers", mu.GPULayers).
		Bool("flash_attention", mu.FlashAttention).
		Msg("received model update — downloading and hashing model file")

	// Signal that the worker is updating (unavailable for work dispatch)
	if err := w.conn.SendStatus("updating"); err != nil {
		log.Warn().Err(err).Msg("failed to send updating status")
	}

	// Download the model file from the manager's model store
	if mu.Filename == "" || w.managerHTTPURL == "" || w.modelsDir == "" {
		log.Warn().Msg("model update: missing filename or download config, reporting expected hash")
		// Fallback: trust the manager's expected hash (insecure, but allows testing without download infra)
		w.conn.SetLLMHash(mu.LLMHash)
		if err := w.conn.SendLLMHashReport(mu.LLMHash); err != nil {
			log.Error().Err(err).Msg("failed to send LLM hash report")
		}
		_ = w.conn.SendStatus("available")
		return
	}

	modelPath, err := w.downloadModel(mu.Filename)
	if err != nil {
		log.Error().Err(err).Str("filename", mu.Filename).Msg("failed to download model file")
		if w.reporter != nil {
			w.reporter.Report(err)
		}
		_ = w.conn.SendStatus("available")
		return
	}

	// Compute SHA256 of the downloaded file
	fileHash, err := protocol.HashFile(modelPath)
	if err != nil {
		log.Error().Err(err).Str("path", modelPath).Msg("failed to hash downloaded model file")
		if w.reporter != nil {
			w.reporter.Report(err)
		}
		_ = w.conn.SendStatus("available")
		return
	}

	// Verify file hash matches what the manager told us to expect
	if mu.ModelFileHash != "" && fileHash != mu.ModelFileHash {
		log.Error().
			Str("expected", mu.ModelFileHash[:16]).
			Str("got", fileHash[:16]).
			Msg("model file hash mismatch — possible tampering or corruption")
		if w.reporter != nil {
			w.reporter.ReportMessage(fmt.Sprintf("model file hash mismatch: expected %s, got %s", mu.ModelFileHash[:16], fileHash[:16]))
		}
		_ = w.conn.SendStatus("available")
		return
	}

	// Compute the llmHash from role + actual file hash
	newHash := protocol.ComputeLLMHash(mu.Role, fileHash)
	w.conn.SetLLMHash(newHash)

	log.Info().
		Str("file_hash", fileHash[:16]+"...").
		Str("llm_hash", newHash[:16]+"...").
		Msg("model file verified, reporting new LLM hash")

	if err := w.conn.SendLLMHashReport(newHash); err != nil {
		log.Error().Err(err).Msg("failed to send LLM hash report after model update")
	}

	// Restart llama-server with the new model file and parameters from the manager.
	if w.restartLLM != nil {
		p := backend.LlamaParams{
			ContextSize:    mu.ContextSize,
			ParallelSlots:  mu.ParallelSlots,
			GPULayers:      mu.GPULayers,
			EndpointType:   mu.EndpointType,
			FlashAttention: mu.FlashAttention,
		}
		if err := w.restartLLM(modelPath, p); err != nil {
			log.Warn().Err(err).Msg("failed to restart llama-server after model update")
		}
	}

	_ = w.conn.SendStatus("available")
}

// downloadModel downloads a model file from the manager's model store.
func (w *Worker) downloadModel(filename string) (string, error) {
	destPath := w.modelsDir + "/" + filename

	// Check if file already exists
	if _, err := os.Stat(destPath); err == nil {
		log.Info().Str("path", destPath).Msg("model file already exists, skipping download")
		return destPath, nil
	}

	url := strings.TrimSuffix(w.managerHTTPURL, "/") + "/v1/models/download/" + filename
	log.Info().Str("url", url).Str("dest", destPath).Msg("downloading model file")

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+w.workerKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Write to temp file then rename (atomic)
	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("download interrupted: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename model file: %w", err)
	}

	log.Info().Str("path", destPath).Msg("model file downloaded")
	return destPath, nil
}

// canonicalSignaturePayload creates a deterministic byte representation for signing.
func canonicalSignaturePayload(id string, output json.RawMessage, processingTimeMs int64) []byte {
	payload := fmt.Sprintf("%s:%s:%d", id, string(output), processingTimeMs)
	return []byte(payload)
}

// detectTrigger checks if any message in the work unit contains a test trigger payload.
func detectTrigger(wu *protocol.WorkUnit) string {
	for _, msg := range wu.Messages {
		if strings.Contains(msg.Content, TriggerPause) {
			return TriggerPause
		}
		if strings.Contains(msg.Content, TriggerPanic) {
			return TriggerPanic
		}
		if strings.Contains(msg.Content, TriggerError) {
			return TriggerError
		}
	}
	return ""
}

func (w *Worker) handleBinaryUpdate(bu *protocol.BinaryUpdate) {
	if w.updater == nil {
		log.Warn().Msg("received binary update but no updater configured")
		return
	}

	log.Info().
		Str("version", bu.Version).
		Msg("received binary update notification")

	if err := w.conn.SendStatus("updating"); err != nil {
		log.Warn().Err(err).Msg("failed to send updating status")
	}

	replaced, err := w.updater.Apply(bu)
	if err != nil {
		log.Error().Err(err).Msg("binary update failed")
		if w.reporter != nil {
			w.reporter.Report(err)
		}
		_ = w.conn.SendStatus("available")
		return
	}

	if replaced && w.restartFn != nil {
		log.Info().Msg("binary replaced — triggering restart")
		w.restartFn()
		return
	}

	_ = w.conn.SendStatus("available")
}

func (w *Worker) handleBackendUpdate(bu *protocol.BackendUpdate) {
	if w.backendMgr == nil {
		log.Warn().Msg("received backend update but no backend manager configured")
		return
	}

	log.Info().
		Str("version", bu.Version).
		Msg("received backend update notification")

	if err := w.conn.SendStatus("updating"); err != nil {
		log.Warn().Err(err).Msg("failed to send updating status")
	}

	updated, err := w.backendMgr.Apply(bu)
	if err != nil {
		log.Error().Err(err).Msg("backend update failed")
		if w.reporter != nil {
			w.reporter.Report(err)
		}
		_ = w.conn.SendStatus("available")
		return
	}

	if updated {
		// Update the connection's backend hash so the manager knows we're synced
		w.conn.SetBackendHash(w.backendMgr.Hash())

		// Restart llama-server with the new binary
		if w.backendRestart != nil {
			if err := w.backendRestart(); err != nil {
				log.Error().Err(err).Msg("failed to restart backend after update")
				if w.reporter != nil {
					w.reporter.Report(err)
				}
			} else {
				log.Info().Msg("backend restarted with new binary")
			}
		}
	}

	_ = w.conn.SendStatus("available")
}
