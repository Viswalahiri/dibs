package triage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
)

func noSleep(context.Context, time.Duration) error { return nil }

// testTriageConfig is the shipped triage defaults with the retry budget under
// the test's control and no waiting.
func testTriageConfig(retries int) *config.Config {
	cfg := baseConfig()
	cfg.Triage = config.Triage{
		Model: "claude-sonnet-5", Thinking: "disabled",
		MaxBodyChars: 4000, MaxThreadChars: 2500, MaxDocChars: 1500,
		MaxRetries: retries, TimeoutSec: 10, DailyCallCap: 100,
		Cost: config.Cost{InputPerMTokUSD: 2, OutputPerMTokUSD: 10, MonthlyBudgetUSD: 10},
	}
	return cfg
}

func modelReply(t *testing.T, w http.ResponseWriter, r Response, in, out int) {
	t.Helper()
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"stop_reason": "end_turn",
		"content":     []any{map[string]string{"type": "text", "text": string(encoded)}},
		"usage":       map[string]int{"input_tokens": in, "output_tokens": out},
	})
}

func TestScoreRecordsUsage(t *testing.T) {
	c := newTestClient(t, 2, func(w http.ResponseWriter, _ *http.Request, _ int) {
		modelReply(t, w, dims(4), 3300, 350)
	})
	result, attempts, err := c.Score(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || !attempts[0].OK {
		t.Fatalf("got %d attempts, want one successful: %+v", len(attempts), attempts)
	}
	if attempts[0].InputTok != 3300 || attempts[0].OutputTok != 350 {
		t.Errorf("usage not recorded: %+v", attempts[0])
	}
	if result.Response.Dimensions.ScopeClarity.Score != 4 {
		t.Errorf("decoded the wrong response: %+v", result.Response)
	}
}

func TestScoreRetriesRateLimits(t *testing.T) {
	c := newTestClient(t, 2, func(w http.ResponseWriter, _ *http.Request, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		modelReply(t, w, dims(3), 100, 20)
	})
	_, attempts, err := c.Score(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	// Both the failure and the success are recorded. A retry that worked still
	// cost tokens, and a run of failures is what the outage warning reads.
	if len(attempts) != 2 {
		t.Fatalf("got %d attempts, want 2: %+v", len(attempts), attempts)
	}
	if attempts[0].OK || !attempts[1].OK {
		t.Errorf("attempts recorded wrongly: %+v", attempts)
	}
}

// A bad key is not a transient failure. Repeating it only wastes time, and the
// caller needs the error promptly so it can fail open.
func TestScoreDoesNotRetryABadKey(t *testing.T) {
	calls := 0
	c := newTestClient(t, 2, func(w http.ResponseWriter, _ *http.Request, call int) {
		calls = call
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	})
	_, attempts, err := c.Score(context.Background(), "system", "user")
	if err == nil {
		t.Fatal("a 401 did not produce an error")
	}
	if calls != 1 || len(attempts) != 1 {
		t.Fatalf("a 401 was retried: %d calls, %d attempts", calls, len(attempts))
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error does not name the status: %v", err)
	}
}

// The retry carries a sharper instruction, and a model that will not justify
// its top marks twice loses them rather than losing the issue.
func TestScoreRetriesAnUnjustifiedFiveThenDowngrades(t *testing.T) {
	var secondPrompt string
	bad := dims(5)
	bad.Dimensions.BlastRadius = Dimension{Score: 5, Why: "small"}

	c := newTestClientCapturing(t, 1, func(w http.ResponseWriter, body map[string]any, call int) {
		if call == 2 {
			messages := body["messages"].([]any)
			secondPrompt = messages[0].(map[string]any)["content"].(string)
		}
		modelReply(t, w, bad, 100, 20)
	})

	result, attempts, err := c.Score(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("the fallback did not produce a usable response: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("got %d attempts, want 2", len(attempts))
	}
	if !strings.Contains(secondPrompt, "quote the specific text") {
		t.Errorf("the retry did not sharpen the instruction: %q", secondPrompt)
	}
	if got := result.Response.Dimensions.BlastRadius.Score; got != 4 {
		t.Errorf("the unjustified 5 survived as %d, want 4", got)
	}
	if err := result.Response.Validate(); err != nil {
		t.Errorf("the returned response is still invalid: %v", err)
	}
}

func TestScoreRejectsATruncatedResponse(t *testing.T) {
	c := newTestClient(t, 0, func(w http.ResponseWriter, _ *http.Request, _ int) {
		json.NewEncoder(w).Encode(map[string]any{
			"stop_reason": "max_tokens",
			"content":     []any{map[string]string{"type": "text", "text": "{"}},
			"usage":       map[string]int{"input_tokens": 3300, "output_tokens": 1200},
		})
	})
	if _, _, err := c.Score(context.Background(), "system", "user"); err == nil {
		t.Fatal("a truncated response was accepted")
	}
}

// The request must never carry a sampling parameter: every current model
// generation rejects them with a 400.
func TestRequestShape(t *testing.T) {
	var body map[string]any
	c := newTestClientCapturing(t, 0, func(w http.ResponseWriter, b map[string]any, _ int) {
		body = b
		modelReply(t, w, dims(3), 10, 10)
	})
	if _, _, err := c.Score(context.Background(), "system prompt", "user message"); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := body[banned]; ok {
			t.Errorf("request carried %q", banned)
		}
	}
	if body["system"] != "system prompt" {
		t.Errorf("system prompt not sent: %v", body["system"])
	}
	format := body["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("response not constrained by a schema: %v", format)
	}
	if body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Errorf("thinking config not sent: %v", body["thinking"])
	}
}

// --- helpers ---

func newTestClient(t *testing.T, retries int, h func(http.ResponseWriter, *http.Request, int)) *Client {
	t.Helper()
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		h(w, r, call)
	}))
	t.Cleanup(srv.Close)
	return NewClient("key", testTriageConfig(retries), WithAPIBase(srv.URL), WithSleep(noSleep))
}

func newTestClientCapturing(t *testing.T, retries int, h func(http.ResponseWriter, map[string]any, int)) *Client {
	t.Helper()
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		h(w, body, call)
	}))
	t.Cleanup(srv.Close)
	return NewClient("key", testTriageConfig(retries), WithAPIBase(srv.URL), WithSleep(noSleep))
}
