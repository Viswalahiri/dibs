package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
)

const (
	defaultAPIBase = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"

	// maxTokens bounds the response. The schema is small and every free-text
	// field is capped at 20 words, so this is headroom rather than a target.
	maxTokens = 1200
)

// reciteInstruction is appended on the retry after a response fails
// validation, which in practice means a dimension scored 5 with a justification
// too short to be a judgment.
const reciteInstruction = `Your previous response was rejected. Every dimension ` +
	`scoring 5 must quote the specific text from the issue that earned it, in at ` +
	`most 20 words. Score lower rather than inventing a justification.`

// Client calls the Anthropic Messages API. It is hand-rolled on net/http for
// the same reason the GitHub client is: dibs needs exact control over retries
// and wants no dependency it does not have a specific need for.
type Client struct {
	http    *http.Client
	key     string
	baseURL string
	cfg     *config.Config
	sleep   func(context.Context, time.Duration) error
}

type ClientOption func(*Client)

// WithAPIBase points the client at a test server.
func WithAPIBase(u string) ClientOption {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithSleep replaces the wait between retries so tests do not spend it.
func WithSleep(f func(context.Context, time.Duration) error) ClientOption {
	return func(c *Client) { c.sleep = f }
}

func NewClient(key string, cfg *config.Config, opts ...ClientOption) *Client {
	c := &Client{
		http:    &http.Client{Timeout: cfg.Triage.Timeout()},
		key:     key,
		baseURL: defaultAPIBase,
		cfg:     cfg,
		sleep:   sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Attempt is one request to the model, successful or not. Every attempt is
// recorded, because a retry that succeeded still cost tokens and a run of
// failures is what the spend and outage warnings are built on.
type Attempt struct {
	Model     string
	InputTok  int
	OutputTok int
	LatencyMS int
	OK        bool
	Err       string
}

// Result is a validated response and the raw JSON it was decoded from. The raw
// text is stored so `dibs replay` can re-score under a different config without
// calling the model again.
type Result struct {
	Response Response
	Raw      string
}

// Score calls the model and returns the validated verdict along with every
// attempt it took to get there.
//
// A response that fails validation is retried once with a sharper instruction.
// If it fails again the unjustified fives are downgraded to fours and the
// response is used anyway, because throwing the issue away over a formatting
// quarrel would be the one error that actually costs something.
func (c *Client) Score(ctx context.Context, system, user string) (Result, []Attempt, error) {
	var attempts []Attempt
	message := user
	var lastGood *Response

	for attempt := 0; ; attempt++ {
		started := time.Now()
		raw, usage, err := c.call(ctx, system, message)
		rec := Attempt{
			Model:     c.cfg.Triage.Model,
			LatencyMS: int(time.Since(started).Milliseconds()),
			InputTok:  usage.InputTokens,
			OutputTok: usage.OutputTokens,
		}

		if err == nil {
			resp, decodeErr := Decode([]byte(raw))
			validErr := decodeErr
			if decodeErr == nil {
				validErr = resp.Validate()
			}
			if validErr == nil {
				rec.OK = true
				return Result{Response: resp, Raw: raw}, append(attempts, rec), nil
			}
			if decodeErr == nil {
				// The shape is right and only the discipline is missing, so the
				// response is worth keeping as a fallback.
				lastGood = &resp
				message = user + "\n\n" + reciteInstruction
			}
			err = validErr
		}

		rec.Err = err.Error()
		attempts = append(attempts, rec)

		if attempt >= c.cfg.Triage.MaxRetries || !retryable(err) {
			if lastGood != nil {
				lastGood.DowngradeUnjustifiedFives()
				encoded, encErr := json.Marshal(lastGood)
				if encErr == nil {
					return Result{Response: *lastGood, Raw: string(encoded)}, attempts, nil
				}
			}
			return Result{}, attempts, err
		}
		if err := c.sleep(ctx, backoff(attempt)); err != nil {
			return Result{}, attempts, err
		}
	}
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      usage  `json:"usage"`
}

// APIError is a non-2xx from the Messages API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("anthropic API: HTTP %d: %s", e.Status, body)
}

// call performs one request. Sampling parameters are never sent: temperature,
// top_p, and top_k are rejected with a 400 on every current model generation.
func (c *Client) call(ctx context.Context, system, user string) (string, usage, error) {
	body := map[string]any{
		"model":      c.cfg.Triage.Model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
		"thinking": map[string]string{"type": c.cfg.Triage.Thinking},
		"output_config": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"schema": json.RawMessage(responseSchema),
			},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", usage{}, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/messages", bytes.NewReader(encoded))
	if err != nil {
		return "", usage{}, err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", usage{}, fmt.Errorf("call anthropic: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", usage{}, fmt.Errorf("read anthropic response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", usage{}, &APIError{Status: resp.StatusCode, Body: string(raw)}
	}

	var decoded messagesResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", usage{}, fmt.Errorf("decode anthropic response: %w", err)
	}
	switch decoded.StopReason {
	case "max_tokens":
		return "", decoded.Usage, errors.New("anthropic response was cut off at max_tokens")
	case "refusal":
		return "", decoded.Usage, errors.New("anthropic declined to score this issue")
	}
	for _, block := range decoded.Content {
		if block.Type == "text" {
			return block.Text, decoded.Usage, nil
		}
	}
	return "", decoded.Usage, errors.New("anthropic response contained no text block")
}

// retryable reports whether another attempt could plausibly succeed. A 400 or a
// 401 means the request or the key is wrong and repeating it only wastes time.
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	// Everything else is a transport failure or a response that did not hold
	// up, both of which are worth one more try.
	return true
}

// backoff returns 1s, 2s, or 4s with full jitter.
func backoff(attempt int) time.Duration {
	base := time.Second << attempt
	if base > 4*time.Second {
		base = 4 * time.Second
	}
	return time.Duration(rand.Int63n(int64(base)) + 1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
