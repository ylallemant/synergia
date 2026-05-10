package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/protocol"
)

// Client communicates with the local llama-server via OpenAI-compatible API.
type Client struct {
	baseURL    string
	httpClient *http.Client

	mu        sync.RWMutex
	reachable bool
	lastError string
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// IsReachable returns the last known reachability state and error message.
func (c *Client) IsReachable() (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reachable, c.lastError
}

// MonitorHealth periodically checks llama-server reachability.
// Blocks until ctx is cancelled.
func (c *Client) MonitorHealth(ctx context.Context, interval time.Duration) {
	// Immediate first check
	c.checkHealth(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkHealth(ctx)
		}
	}
}

func (c *Client) checkHealth(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := c.Health(checkCtx)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		if c.reachable {
			log.Warn().Err(err).Str("url", c.baseURL).Msg("llama-server became unreachable")
		}
		c.reachable = false
		c.lastError = err.Error()
	} else {
		if !c.reachable {
			log.Info().Str("url", c.baseURL).Msg("llama-server is reachable")
		}
		c.reachable = true
		c.lastError = ""
	}
}

// Complete sends a chat completion request to the local llama-server and returns the raw response.
func (c *Client) Complete(ctx context.Context, wu *protocol.WorkUnit) (json.RawMessage, error) {
	req := protocol.ChatCompletionRequest{
		Model:          wu.Model,
		Messages:       wu.Messages,
		Temperature:    wu.Params.Temperature,
		MaxTokens:      wu.Params.MaxTokens,
		ResponseFormat: wu.Params.ResponseFormat,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llama-server request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llama-server returned %d: %s", resp.StatusCode, string(respBody))
	}

	return json.RawMessage(respBody), nil
}

// Health checks if the local llama-server is reachable.
func (c *Client) Health(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("llama-server unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama-server health check returned %d", resp.StatusCode)
	}
	return nil
}
