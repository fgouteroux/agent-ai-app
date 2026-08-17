package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseRetryAfter_SecondsForm(t *testing.T) {
	d, ok := parseRetryAfter("5")
	if !ok {
		t.Fatal("expected ok=true for a valid seconds value")
	}
	if d != 5*time.Second {
		t.Errorf("expected 5s, got %v", d)
	}
}

func TestParseRetryAfter_HTTPDateForm(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC()
	d, ok := parseRetryAfter(future.Format(http.TimeFormat))
	if !ok {
		t.Fatal("expected ok=true for a valid future HTTP-date")
	}
	// Formatting/parsing an HTTP-date truncates to whole seconds, and wall
	// clock advances between formatting it above and parsing it inside
	// parseRetryAfter -- allow a couple seconds of slack either way instead
	// of asserting an exact duration.
	if d < 7*time.Second || d > 11*time.Second {
		t.Errorf("expected ~10s, got %v", d)
	}
}

func TestParseRetryAfter_EmptyIsNotOk(t *testing.T) {
	if _, ok := parseRetryAfter(""); ok {
		t.Error("expected ok=false for an empty header")
	}
}

func TestParseRetryAfter_GarbageIsNotOk(t *testing.T) {
	if _, ok := parseRetryAfter("not-a-valid-value"); ok {
		t.Error("expected ok=false for a malformed header")
	}
}

func TestParseRetryAfter_ZeroOrNegativeSecondsIsNotOk(t *testing.T) {
	if _, ok := parseRetryAfter("0"); ok {
		t.Error("expected ok=false for \"0\" seconds")
	}
	if _, ok := parseRetryAfter("-5"); ok {
		t.Error("expected ok=false for a negative seconds value")
	}
}

func TestParseRetryAfter_PastHTTPDateIsNotOk(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC()
	if _, ok := parseRetryAfter(past.Format(http.TimeFormat)); ok {
		t.Error("expected ok=false for an HTTP-date already in the past")
	}
}

func TestRetryAfterTransport_CapturesHeaderOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	transport := &retryAfterTransport{base: http.DefaultTransport}
	ctx, capture := withRetryAfterCapture(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	d, ok := capture.get()
	if !ok {
		t.Fatal("expected the Retry-After header to be captured")
	}
	if d != 3*time.Second {
		t.Errorf("expected 3s captured, got %v", d)
	}
}

func TestRetryAfterTransport_NoCaptureWithoutHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	transport := &retryAfterTransport{base: http.DefaultTransport}
	ctx, capture := withRetryAfterCapture(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if _, ok := capture.get(); ok {
		t.Error("expected no capture when the response has no Retry-After header")
	}
}

func TestRetryAfterTransport_NoCaptureOnNon429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &retryAfterTransport{base: http.DefaultTransport}
	ctx, capture := withRetryAfterCapture(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if _, ok := capture.get(); ok {
		t.Error("expected no capture for a 200 response, even with a Retry-After header present")
	}
}

func TestRetryAfterTransport_NoCaptureWithoutContextValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	transport := &retryAfterTransport{base: http.DefaultTransport}
	// No withRetryAfterCapture -- must not panic just because nothing is
	// listening for a captured value.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
}
