package main

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Live throughput (SPEC.md section 20). The proxy observes response
// counters while streaming; the byte stream is never altered. Native
// Ollama responses carry exact eval counters in the final chunk.
// OpenAI-compatible responses report token usage but no durations, so
// their tok/s is estimated from completion tokens over the measured
// generation window and marked approximate wherever it is shown.

// tailBuf keeps the last max bytes written through it.
type tailBuf struct {
	buf []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuf) Bytes() []byte { return t.buf }

type throughput struct {
	evalCount       int
	promptEvalCount int
	tps             float64
	approx          bool
}

// lastJSONLines returns the JSON objects found in tail, newest first.
// Lines may be plain JSON (NDJSON, single responses) or SSE data
// lines; "[DONE]" and non-JSON lines are skipped.
func lastJSONLines(tail []byte) []json.RawMessage {
	lines := bytes.Split(tail, []byte("\n"))
	var out []json.RawMessage
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if json.Valid(line) {
			out = append(out, line)
		}
	}
	return out
}

func observeThroughput(path string, tail []byte, totalMs, firstByteMs int64) throughput {
	objs := lastJSONLines(tail)
	if path == "/v1/chat/completions" || path == "/v1/embeddings" {
		for _, raw := range objs {
			var body struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(raw, &body); err != nil || body.Usage == nil {
				continue
			}
			tp := throughput{evalCount: body.Usage.CompletionTokens, promptEvalCount: body.Usage.PromptTokens, approx: true}
			// The generation window is first byte to last byte. A
			// single-flush (non-streaming) response has no window;
			// its window is the total time.
			windowMs := totalMs - firstByteMs
			if windowMs <= 0 {
				windowMs = totalMs
			}
			if windowMs > 0 && tp.evalCount > 0 {
				tp.tps = float64(tp.evalCount) / (float64(windowMs) / 1000.0)
			}
			return tp
		}
		return throughput{}
	}
	for _, raw := range objs {
		var body struct {
			EvalCount       int   `json:"eval_count"`
			EvalDuration    int64 `json:"eval_duration"` // nanoseconds
			PromptEvalCount int   `json:"prompt_eval_count"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.EvalCount == 0 {
			continue
		}
		tp := throughput{evalCount: body.EvalCount, promptEvalCount: body.PromptEvalCount}
		if body.EvalDuration > 0 {
			tp.tps = float64(body.EvalCount) / (float64(body.EvalDuration) / 1e9)
		}
		return tp
	}
	return throughput{}
}

// throughputSummary averages tok/s per backend and served model over
// the entries that carry it. It lives in the activity ring only: a
// live view, reset on restart like the feed itself.
func throughputSummary(entries []activityEntry) map[string]any {
	type agg struct {
		requests int
		sum      float64
		approx   bool
	}
	groups := map[string]*agg{}
	for _, e := range entries {
		if e.Kind != "route" || e.TPS <= 0 {
			continue
		}
		key := e.Backend + " " + e.Served
		g := groups[key]
		if g == nil {
			g = &agg{approx: e.TPSApprox}
			groups[key] = g
		}
		g.requests++
		g.sum += e.TPS
		g.approx = g.approx || e.TPSApprox
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		backend, served := "", k
		for i := 0; i < len(k); i++ {
			if k[i] == ' ' {
				backend, served = k[:i], k[i+1:]
				break
			}
		}
		out = append(out, map[string]any{
			"backend": backend, "model": served, "requests": g.requests,
			"avg_tok_per_sec": g.sum / float64(g.requests), "approx": g.approx,
		})
	}
	return map[string]any{"by_backend_model": out}
}
