package plugin

import (
	"context"
	"errors"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// rateLimitBaseBackoff is how long to wait before the 1st/2nd/3rd retry
// after a 429. Free LLM tiers (e.g. Groq) rate-limit per-minute, so a short
// wait often clears the window instead of failing the whole request the
// moment a tool-calling loop happens to send one request too many. Beyond
// the 3rd retry (configurable via Settings.RateLimitMaxRetries), the last
// value repeats for any further attempt.
var rateLimitBaseBackoff = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}

// rateLimitMaxRetries safely reads Settings.RateLimitMaxRetries, falling
// back to the default when unset (should only happen in tests that build a
// Settings literal directly instead of going through NewApp).
func rateLimitMaxRetries(s Settings) int {
	if s.RateLimitMaxRetries == nil {
		return defaultRateLimitMaxRetries
	}
	return *s.RateLimitMaxRetries
}

// rateLimitWait returns the wait duration before the given retry attempt
// (0-indexed), extending the last entry of rateLimitBaseBackoff for
// attempts beyond it.
func rateLimitWait(attempt int) time.Duration {
	if attempt < len(rateLimitBaseBackoff) {
		return rateLimitBaseBackoff[attempt]
	}
	return rateLimitBaseBackoff[len(rateLimitBaseBackoff)-1]
}

// createChatCompletionWithRetry retries on a 429 response from the LLM
// endpoint with a short backoff, up to maxRetries times (0 disables
// retrying). Any other error (or a non-429 API error) returns immediately.
// onRetry, if non-nil, is called before each wait so callers with a live
// stream (e.g. the chat UI) can surface a status update instead of leaving
// the user looking at a silent "thinking" spinner for up to tens of
// seconds.
func createChatCompletionWithRetry(ctx context.Context, client *openai.Client, req openai.ChatCompletionRequest, maxRetries int, onRetry func(wait time.Duration)) (openai.ChatCompletionResponse, error) {
	for attempt := 0; ; attempt++ {
		reqCtx, capture := withRetryAfterCapture(ctx)
		resp, err := client.CreateChatCompletion(reqCtx, req)
		if err == nil {
			return resp, nil
		}

		var apiErr *openai.APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatusCode != 429 || attempt >= maxRetries {
			return resp, err
		}

		// Prefer the provider's own Retry-After when it sent one -- it
		// knows its actual rate-limit window better than our guessed
		// backoff schedule does -- but cap it so a provider returning an
		// unreasonable value can't stall the request far longer than any
		// user would wait for a reply.
		wait := rateLimitWait(attempt)
		if d, ok := capture.get(); ok {
			wait = d
			if wait > maxRespectedRetryAfter {
				wait = maxRespectedRetryAfter
			}
		}
		if onRetry != nil {
			onRetry(wait)
		}

		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(wait):
		}
	}
}
