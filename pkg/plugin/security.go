package plugin

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

const (
	maxPromptLength = 10000
	maxContextBytes = 512 * 1024 // 512 KB

	// maxMessagesCount/maxMessagesBytes bound the conversation history a
	// caller can attach to a single chat request. The real chat UI trims
	// history client-side (services/truncation.ts) well under either limit
	// long before this is reached -- this exists for a caller hitting the
	// resource endpoint directly (bypassing that UI), so an unbounded
	// messages array can't force this backend into excessive memory/CPU use
	// or an oversized LLM request on behalf of one user, which would slow
	// down or fail everyone else's requests too.
	maxMessagesCount = 200
	maxMessagesBytes = 1024 * 1024 // 1 MB combined content

	// maxChatBodyBytes bounds the raw request body handleChat/
	// handleStreamResource will read before even attempting to decode it,
	// so an oversized/malicious body can't be fully buffered into memory
	// before any of the checks above ever run (security-audit finding
	// M-04). Must stay comfortably under 16 MiB (16,777,216 bytes): every
	// resource route, streaming included, is delivered to this backend
	// over Grafana's own CallResource gRPC transport, which hard-caps a
	// single message at exactly that (grpc's default max receive size) --
	// live-confirmed a body between this constant's old value (25 MB) and
	// that cap never reached this check at all, failing instead with a
	// generic "ResourceExhausted"/500 from the transport itself, not the
	// friendly 413 this is meant to produce. This is also why
	// maxAttachmentsPerMessage*maxAttachmentMaxBytes (see attachments.go)
	// is no longer sized to fit under this limit with base64 overhead --
	// that combination was already unreachable via gRPC regardless of this
	// constant, a separate, pre-existing sizing mismatch worth revisiting
	// on its own.
	maxChatBodyBytes = 12 * 1024 * 1024 // 12 MB
)

// sanitizePrompt removes control characters and truncates to the maximum length.
func sanitizePrompt(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}

	// Trim first so a whitespace-only prompt (e.g. "   ") reduces to "" and
	// is correctly rejected by handleChat's "prompt is required" check --
	// live-found bug: without this, whitespace-only input silently reached
	// the LLM as a real (wasted) request instead of a 400.
	result := strings.TrimSpace(b.String())
	// Truncate at a rune boundary to avoid splitting multi-byte UTF-8
	runes := []rune(result)
	if len(runes) > maxPromptLength {
		result = string(runes[:maxPromptLength])
	}

	return result
}

// sanitizeContextSize rejects context payloads that exceed the maximum size.
func sanitizeContextSize(data []byte, maxBytes int) error {
	if len(data) > maxBytes {
		return fmt.Errorf("context too large: %d bytes exceeds maximum %d bytes", len(data), maxBytes)
	}
	return nil
}

// validateMessages rejects a conversation history that's too long (count) or
// too large (combined content bytes) for one request to reasonably carry.
func validateMessages(messages []ChatMessage) error {
	if len(messages) > maxMessagesCount {
		return fmt.Errorf("too many messages: %d exceeds maximum %d", len(messages), maxMessagesCount)
	}
	total := 0
	for _, m := range messages {
		total += len(m.Content)
		if total > maxMessagesBytes {
			return fmt.Errorf("messages too large: combined content exceeds maximum %d bytes", maxMessagesBytes)
		}
	}
	return nil
}

// validateURL checks that a URL uses an allowed scheme (http/https) and has a host.
func validateURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("URL is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("URL scheme %q not allowed, must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL has no host")
	}
	return nil
}
