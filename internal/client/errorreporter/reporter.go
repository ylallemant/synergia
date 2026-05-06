package errorreporter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Reporter catches and deduplicates errors, then reports them to the cluster manager.
type Reporter struct {
	mu          sync.Mutex
	seen        map[string]time.Time // hash → last reported time
	managerURL  string
	workerKey   string
	fingerprint string
	version     string
}

// New creates an error reporter.
func New(managerBaseURL, workerKey, fingerprint, version string) *Reporter {
	return &Reporter{
		seen:        make(map[string]time.Time),
		managerURL:  managerBaseURL,
		workerKey:   workerKey,
		fingerprint: fingerprint,
		version:     version,
	}
}

// ErrorReport is sent to the manager.
type ErrorReport struct {
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
	Error       string `json:"error"`
	Stack       string `json:"stack"`
	Timestamp   string `json:"timestamp"`
}

// Report captures an error with its stack trace and sends it to the manager.
// Deduplicates by error message hash — same error is only sent once per hour.
func (r *Reporter) Report(err error) {
	if err == nil {
		return
	}
	r.ReportMessage(err.Error())
}

// ReportMessage captures an error message string with stack trace.
func (r *Reporter) ReportMessage(msg string) {
	stack := captureStack(3) // skip ReportMessage, Report, and caller
	hash := hashError(msg)

	r.mu.Lock()
	if lastSent, exists := r.seen[hash]; exists && time.Since(lastSent) < time.Hour {
		r.mu.Unlock()
		return // already reported recently
	}
	r.seen[hash] = time.Now()
	r.mu.Unlock()

	report := ErrorReport{
		Fingerprint: r.fingerprint,
		Version:     r.version,
		Error:       msg,
		Stack:       stack,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	go r.send(report)
}

// ReportWithStack captures an error with an explicit stack trace string.
func (r *Reporter) ReportWithStack(err error, stack string) {
	if err == nil {
		return
	}

	hash := hashError(err.Error())

	r.mu.Lock()
	if lastSent, exists := r.seen[hash]; exists && time.Since(lastSent) < time.Hour {
		r.mu.Unlock()
		return
	}
	r.seen[hash] = time.Now()
	r.mu.Unlock()

	report := ErrorReport{
		Fingerprint: r.fingerprint,
		Version:     r.version,
		Error:       err.Error(),
		Stack:       stack,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	go r.send(report)
}

func (r *Reporter) send(report ErrorReport) {
	body, err := json.Marshal(report)
	if err != nil {
		log.Debug().Err(err).Msg("failed to marshal error report")
		return
	}

	url := r.managerURL + "/v1/errors"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Debug().Err(err).Msg("failed to create error report request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.workerKey)
	req.Header.Set("X-Worker-Fingerprint", r.fingerprint)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debug().Err(err).Msg("failed to send error report")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		log.Debug().Int("status", resp.StatusCode).Msg("error report rejected by manager")
	} else {
		log.Debug().Str("error", report.Error).Msg("error report sent to manager")
	}
}

func hashError(msg string) string {
	h := sha256.Sum256([]byte(msg))
	return hex.EncodeToString(h[:16]) // 128-bit hash is sufficient for dedup
}

func captureStack(skip int) string {
	var sb strings.Builder
	pcs := make([]uintptr, 32)
	n := runtime.Callers(skip, pcs)
	frames := runtime.CallersFrames(pcs[:n])

	for {
		frame, more := frames.Next()
		// Skip runtime internals
		if strings.Contains(frame.Function, "runtime.") {
			if !more {
				break
			}
			continue
		}
		sb.WriteString(fmt.Sprintf("%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line))
		if !more {
			break
		}
	}
	return sb.String()
}
