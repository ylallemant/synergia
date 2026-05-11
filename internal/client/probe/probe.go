// Package probe contains all background goroutines that sample external
// subsystems and push results into the central state.State.
//
// Probe intervals:
//   - Connection + GPUSentAvg : 1 s  (in-memory reads, negligible cost)
//   - GPU state/util/stats    : 1 s  (reads gpu.Monitor cache, updated at 5 s)
//   - LLM reachability        : 3 s  (HTTP probe with 2 s timeout)
package probe

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/connection"
	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/llm"
	"github.com/ylallemant/synergia/internal/client/state"
)

// RunAll starts all probe goroutines. Call once after all components are ready.
// Returns immediately; goroutines run until ctx is cancelled.
func RunAll(ctx context.Context, st *state.State, conn *connection.Connection, monitor *gpu.Monitor, llmClient *llm.Client) {
	go runConnection(ctx, st, conn)
	go runGPU(ctx, st, monitor)
	go runLLM(ctx, st, llmClient)
}

// runConnection writes connection state and GPU sent-avg to state every 1 s.
// These are in-memory reads on the connection struct — no I/O.
func runConnection(ctx context.Context, st *state.State, conn *connection.Connection) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st.SetConnected(conn.IsConnected(), conn.GetGPUAvg())
		}
	}
}

// runGPU reads from gpu.Monitor's internal cache every 1 s and writes to
// state. The monitor's own probe runs every 5 s; polling at 1 s ensures
// the central state reflects any change within 1 s of the monitor's tick.
// GPU supported / driver info are static after startup — read once.
func runGPU(ctx context.Context, st *state.State, monitor *gpu.Monitor) {
	supported, reason := monitor.GPUSupported()
	driver, driverVer := monitor.GPUDriverInfo()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st.SetGPU(
				monitor.GetState(),
				monitor.GetUtilization(),
				monitor.Stats(),
				supported, reason, driver, driverVer,
			)
		}
	}
}

// runLLM probes llama-server reachability every 3 s with a 2 s HTTP timeout
// and pushes the result to state. One immediate probe fires at startup.
func runLLM(ctx context.Context, st *state.State, client *llm.Client) {
	probe := func() {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := client.Health(probeCtx); err != nil {
			st.SetLLM(false, err.Error())
			log.Debug().Err(err).Msg("llama-server unreachable")
		} else {
			st.SetLLM(true, "")
		}
	}

	probe()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			probe()
		}
	}
}
