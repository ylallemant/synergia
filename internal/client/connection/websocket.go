package connection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/identity"
	"github.com/ylallemant/synergia/internal/protocol"
	"github.com/ylallemant/synergia/internal/client/version"
)

// Connection manages the WebSocket connection to the cluster manager.
type Connection struct {
	url          string
	workerKey    string
	identity     *identity.Identity
	model        string
	quantisation string
	llmHash      string
	backendHash  string
	gpuAvg       int
	dialer       *websocket.Dialer

	mu   sync.Mutex
	conn *websocket.Conn

	// Channels for incoming messages
	WorkUnitCh      chan *protocol.WorkUnit
	ModelUpdateCh   chan *protocol.ModelUpdate
	BinaryUpdateCh  chan *protocol.BinaryUpdate
	BackendUpdateCh chan *protocol.BackendUpdate
	done            chan struct{}
	connectedCh     chan struct{} // closed on first successful connection
	connOnce        sync.Once
}

func New(url, workerKey string, id *identity.Identity, model, quantisation, tlsCACert string) *Connection {
	dialer := websocket.DefaultDialer

	if tlsCACert != "" {
		caCert, err := os.ReadFile(tlsCACert)
		if err != nil {
			log.Fatal().Err(err).Str("path", tlsCACert).Msg("failed to read TLS CA certificate")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			log.Fatal().Str("path", tlsCACert).Msg("failed to parse TLS CA certificate")
		}
		dialer = &websocket.Dialer{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
			HandshakeTimeout: 10 * time.Second,
		}
	}

	return &Connection{
		url:             url,
		workerKey:       workerKey,
		identity:        id,
		model:           model,
		quantisation:    quantisation,
		dialer:          dialer,
		WorkUnitCh:      make(chan *protocol.WorkUnit, 10),
		ModelUpdateCh:   make(chan *protocol.ModelUpdate, 5),
		BinaryUpdateCh:  make(chan *protocol.BinaryUpdate, 1),
		BackendUpdateCh: make(chan *protocol.BackendUpdate, 1),
		done:            make(chan struct{}),
		connectedCh:     make(chan struct{}),
	}
}

// Connected returns a channel that is closed once the first WebSocket connection
// to the manager has been established.
func (c *Connection) Connected() <-chan struct{} {
	return c.connectedCh
}

// Run connects to the cluster manager and reconnects with exponential backoff.
// Blocks until ctx is cancelled.
func (c *Connection) Run(ctx context.Context) {
	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := c.connect(ctx)
		if err != nil {
			log.Warn().Err(err).Dur("retry_in", backoff).Msg("connection failed")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Exponential backoff: 1s, 2s, 4s, 8s, ... max 60s
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

func (c *Connection) connect(ctx context.Context) error {
	headers := http.Header{
		"X-Worker-Fingerprint":  []string{c.identity.Fingerprint},
		"X-Worker-Public-Key":   []string{base64.StdEncoding.EncodeToString(c.identity.PublicKey)},
		"X-Worker-Model":        []string{c.model},
		"X-Worker-Quantisation": []string{c.quantisation},
		"X-Worker-Version":      []string{version.Version},
		"X-Worker-OS":           []string{runtime.GOOS},
		"X-Worker-Arch":         []string{runtime.GOARCH},
	}
	// Key-auth mode: send Bearer token. Empty workerKey = TOFU mode, no header sent.
	if c.workerKey != "" {
		headers.Set("Authorization", "Bearer "+c.workerKey)
		log.Debug().Msg("handshake: using key-auth mode")
	} else {
		log.Debug().Msg("handshake: using TOFU mode — awaiting challenge from manager")
	}

	c.mu.Lock()
	if c.llmHash != "" {
		headers.Set("X-Worker-LLM-Hash", c.llmHash)
	}
	if c.backendHash != "" {
		headers.Set("X-Worker-Backend-Hash", c.backendHash)
	}
	c.mu.Unlock()

	conn, _, err := c.dialer.DialContext(ctx, c.url, headers)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	log.Info().Str("url", c.url).Msg("connected to cluster manager")

	// Signal first connection
	c.connOnce.Do(func() { close(c.connectedCh) })
	// Start heartbeat
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go c.heartbeat(heartbeatCtx)

	// Read loop
	err = c.readLoop(ctx)

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()

	log.Info().Err(err).Msg("disconnected from cluster manager")
	return err
}

func (c *Connection) readLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, message, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}

		var envelope protocol.Envelope
		if err := json.Unmarshal(message, &envelope); err != nil {
			log.Warn().Err(err).Msg("invalid message from manager")
			continue
		}

		switch envelope.Type {
		case protocol.TypeWorkUnit:
			var wu protocol.WorkUnit
			if err := json.Unmarshal(message, &wu); err != nil {
				log.Warn().Err(err).Msg("invalid work_unit message")
				continue
			}
			c.WorkUnitCh <- &wu

		case protocol.TypeModelUpdate:
			var mu protocol.ModelUpdate
			if err := json.Unmarshal(message, &mu); err != nil {
				log.Warn().Err(err).Msg("invalid model_update message")
				continue
			}
			log.Info().
				Str("role", mu.Role).
				Str("model", mu.Model).
				Str("quantisation", mu.Quantisation).
				Str("llm_hash", mu.LLMHash).
				Msg("received model update from manager")
			c.ModelUpdateCh <- &mu

		case protocol.TypeBinaryUpdate:
			var bu protocol.BinaryUpdate
			if err := json.Unmarshal(message, &bu); err != nil {
				log.Warn().Err(err).Msg("invalid binary_update message")
				continue
			}
			log.Info().
				Str("version", bu.Version).
				Str("download_url", bu.DownloadURL).
				Msg("received binary update from manager")
			c.BinaryUpdateCh <- &bu

		case protocol.TypeBackendUpdate:
			var bu protocol.BackendUpdate
			if err := json.Unmarshal(message, &bu); err != nil {
				log.Warn().Err(err).Msg("invalid backend_update message")
				continue
			}
			log.Info().
				Str("version", bu.Version).
				Str("download_url", bu.DownloadURL).
				Msg("received backend update from manager")
			c.BackendUpdateCh <- &bu

		case protocol.TypeChallenge:
			// TOFU mode: manager sent a nonce immediately after upgrade.
			// Build the response without writing, then send through c.Send so the
			// write goes through the connection mutex — preventing a concurrent-write
			// panic when another goroutine (e.g. the initial "available" sender) is
			// also trying to write at the same time.
			resp, ok := buildChallengeResponse(message, c.identity.PrivateKey)
			if !ok {
				return fmt.Errorf("TOFU challenge-response failed")
			}
			if err := c.Send(resp); err != nil {
				return fmt.Errorf("TOFU challenge-response failed: %w", err)
			}
			log.Debug().Msg("handshake: challenge-response completed")

		case protocol.TypeHeartbeat:
			// Manager acknowledged our heartbeat, nothing to do
			log.Debug().Msg("heartbeat ack received")

		default:
			log.Warn().Str("type", string(envelope.Type)).Msg("unknown message type from manager")
		}
	}
}

func (c *Connection) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Send(&protocol.Heartbeat{Type: protocol.TypeHeartbeat}); err != nil {
				log.Warn().Err(err).Msg("heartbeat send failed")
				return
			}
		}
	}
}

// Send writes a JSON message to the WebSocket connection.
func (c *Connection) Send(msg any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return ErrNotConnected
	}

	return c.conn.WriteJSON(msg)
}

// SendStatus sends a status update to the cluster manager.
func (c *Connection) SendStatus(state string) error {
	log.Debug().Str("state", state).Msg("sending status update")
	c.mu.Lock()
	hash := c.llmHash
	avg := c.gpuAvg
	c.mu.Unlock()
	return c.Send(&protocol.Status{
		Type:    protocol.TypeStatus,
		State:   state,
		LLMHash: hash,
		GPUAvg:  avg,
	})
}

// SetGPUAvg stores the rolling GPU baseline mean for inclusion in status messages.
// Call this periodically (e.g., every minute) from the GPU monitor stats.
func (c *Connection) SetGPUAvg(avg int) {
	c.mu.Lock()
	c.gpuAvg = avg
	c.mu.Unlock()
}

// GetGPUAvg returns the baseline mean last reported to the manager (0 = not yet reported).
func (c *Connection) GetGPUAvg() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gpuAvg
}

// SetLLMHash updates the connection's LLM hash (thread-safe).
func (c *Connection) SetLLMHash(hash string) {
	c.mu.Lock()
	c.llmHash = hash
	c.mu.Unlock()
}

// GetLLMHash returns the current LLM hash.
func (c *Connection) GetLLMHash() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.llmHash
}

// SetBackendHash updates the connection's backend binary hash (thread-safe).
func (c *Connection) SetBackendHash(hash string) {
	c.mu.Lock()
	c.backendHash = hash
	c.mu.Unlock()
}

// GetBackendHash returns the current backend hash.
func (c *Connection) GetBackendHash() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backendHash
}

// SendLLMHashReport sends an explicit LLM hash report to the cluster manager.
func (c *Connection) SendLLMHashReport(hash string) error {
	log.Info().Str("llm_hash", hash).Msg("sending LLM hash report")
	return c.Send(&protocol.LLMHashReport{
		Type:    protocol.TypeLLMHashReport,
		LLMHash: hash,
	})
}

// Close gracefully closes the WebSocket connection.
func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil
	}

	return c.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
}

// IsConnected returns true if the WebSocket is currently connected.
func (c *Connection) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

var ErrNotConnected = &NotConnectedError{}

type NotConnectedError struct{}

func (e *NotConnectedError) Error() string { return "not connected to cluster manager" }
