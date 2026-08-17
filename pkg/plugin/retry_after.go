package plugin

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxRespectedRetryAfter caps how long we'll honor a provider's Retry-After
// header for -- a misbehaving or unusually strict provider could otherwise
// stall a chat reply far longer than any user would wait for one. Beyond
// this, we still retry, just capped at this wait instead of whatever the
// header said. A var (not const), like rateLimitBaseBackoff, so tests can
// shrink it instead of a real test run actually sleeping a full minute.
var maxRespectedRetryAfter = 60 * time.Second

type retryAfterCaptureKey struct{}

// retryAfterCapture is a per-request side channel for handing a 429
// response's Retry-After header back to the retry loop in
// createChatCompletionWithRetry. go-openai decodes the response and
// discards the raw *http.Response (and its headers) before returning its
// own APIError, which has no way to carry a header value -- this captures
// it earlier, in the RoundTripper, while the real response still exists.
//
// Scoped to a single request via context (see withRetryAfterCapture)
// rather than a field on the transport itself, since a provider's
// *http.Client (and therefore its Transport) is shared across concurrent
// requests from different users -- a shared mutable field would let one
// request's captured value leak into another's.
type retryAfterCapture struct {
	mu    sync.Mutex
	value time.Duration
	found bool
}

func withRetryAfterCapture(ctx context.Context) (context.Context, *retryAfterCapture) {
	c := &retryAfterCapture{}
	return context.WithValue(ctx, retryAfterCaptureKey{}, c), c
}

func (c *retryAfterCapture) get() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.found
}

func (c *retryAfterCapture) set(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = d
	c.found = true
}

// retryAfterTransport wraps an http.RoundTripper to capture a 429 response's
// Retry-After header (if present) into whatever *retryAfterCapture is
// attached to the request's context. It never alters the request or
// response -- go-openai's own error handling runs completely unaffected.
type retryAfterTransport struct {
	base http.RoundTripper
}

func (t *retryAfterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	if capture, ok := req.Context().Value(retryAfterCaptureKey{}).(*retryAfterCapture); ok {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			capture.set(d)
		}
	}
	return resp, err
}

// parseRetryAfter parses an HTTP Retry-After header value, which per RFC
// 9110 is either a whole number of seconds or an HTTP-date. Returns false
// if the header is absent, malformed, or resolves to a non-positive wait.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
	}
	return 0, false
}
