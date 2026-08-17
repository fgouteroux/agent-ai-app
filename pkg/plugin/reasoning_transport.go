package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// reasoningKeyRewriteTransport rewrites a chat-completions response's
// "reasoning" field into "reasoning_content" -- the key go-openai's
// ChatCompletionMessage/ChatCompletionStreamChoiceDelta structs actually
// know how to decode (added there for DeepSeek's own hosted API, which
// happens to use that name). Confirmed live against Ollama 0.32.4 serving
// deepseek-r1:14b: its OpenAI-compatible /v1/chat/completions response
// carries message.reasoning (non-streaming) / delta.reasoning (streaming),
// never *_content -- without this rewrite, go-openai silently drops the
// field entirely (unknown JSON key), so a real "thinking" model's reasoning
// never reaches this plugin at all, and the frontend's ThinkingBlock never
// has anything to render.
//
// A no-op for every other provider (OpenAI, Groq, grafana-llm-app, and
// DeepSeek's own hosted API, which already uses reasoning_content): the
// "reasoning" key just isn't present in their responses, so nothing is
// rewritten.
type reasoningKeyRewriteTransport struct {
	base http.RoundTripper
}

func (t *reasoningKeyRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = &sseReasoningRewriteReader{scanner: bufio.NewScanner(resp.Body), closer: resp.Body}
		return resp, nil
	}

	// Same reasoning as tool_executor.go's doGrafanaRequest and mcp.go's MCP
	// response read (security-audit finding M-04): bound how much of a
	// response this reads into memory, even from the admin-configured LLM
	// endpoint itself.
	const maxLLMResponseBytes = 10 * 1024 * 1024
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return resp, readErr
	}
	rewritten := rewriteReasoningKeyInChoices(body, "message")
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

// rewriteReasoningKeyInChoices renames "reasoning" to "reasoning_content"
// inside every choice's message/delta object of a chat-completions JSON
// body. Everything else round-trips as untouched json.RawMessage, so no
// other field (including numbers, which a map[string]any round-trip could
// otherwise reformat) is at risk of being altered. Falls back to the
// original bytes unchanged if the body isn't shaped the way we expect --
// never worth failing an otherwise-good response over.
func rewriteReasoningKeyInChoices(body []byte, messageField string) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	rawChoices, ok := top["choices"]
	if !ok {
		return body
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return body
	}

	changed := false
	for i, choice := range choices {
		rawMsg, ok := choice[messageField]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}
		reasoning, ok := msg["reasoning"]
		if !ok {
			continue
		}
		delete(msg, "reasoning")
		msg["reasoning_content"] = reasoning
		remarshaled, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		choice[messageField] = remarshaled
		choices[i] = choice
		changed = true
	}
	if !changed {
		return body
	}

	remarshaledChoices, err := json.Marshal(choices)
	if err != nil {
		return body
	}
	top["choices"] = remarshaledChoices
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// sseReasoningRewriteReader wraps a text/event-stream response body,
// rewriting each "data: {...}" line's delta.reasoning into
// delta.reasoning_content before go-openai's own SSE scanner ever sees it.
type sseReasoningRewriteReader struct {
	scanner *bufio.Scanner
	closer  io.Closer
	buf     bytes.Buffer
}

func (r *sseReasoningRewriteReader) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		r.buf.Write(rewriteSSELine(r.scanner.Bytes()))
		r.buf.WriteByte('\n')
	}
	return r.buf.Read(p)
}

func (r *sseReasoningRewriteReader) Close() error {
	return r.closer.Close()
}

// withThinkingPrefix reconstructs the "<think>...</think>" format the
// frontend's ThinkingBlock parser (ChatInterface.tsx) already knows how to
// render, from the separate reasoning field go-openai's ChatCompletionMessage
// exposes after reasoningKeyRewriteTransport's rewrite. A no-op when there's
// no reasoning (every non-"thinking" model, and any response where the
// rewrite above found nothing to rewrite).
func withThinkingPrefix(content, reasoning string) string {
	if reasoning == "" {
		return content
	}
	return "<think>" + reasoning + "</think>" + content
}

func rewriteSSELine(line []byte) []byte {
	const prefix = "data: "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return line
	}
	payload := line[len(prefix):]
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return line
	}
	rewritten := rewriteReasoningKeyInChoices(payload, "delta")
	out := make([]byte, 0, len(prefix)+len(rewritten))
	out = append(out, prefix...)
	out = append(out, rewritten...)
	return out
}
