package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func TestResourceHealth_Success(t *testing.T) {
	t.Parallel()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	req := &backend.CallResourceRequest{
		Path:   "health",
		Method: http.MethodGet,
	}

	var statusCode int
	var body []byte

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		body = res.Body
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", statusCode, http.StatusOK)
	}

	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}

	// Verify provider URL is NOT leaked
	if _, exists := resp["provider"]; exists {
		t.Error("health response should not include provider URL")
	}
}

func TestResourceChat_MissingPrompt(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	req := &backend.CallResourceRequest{
		Path:   "chat",
		Method: http.MethodPost,
		Body:   []byte(`{"mode":"explain_panel","context":{}}`),
	}

	var statusCode int

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", statusCode, http.StatusBadRequest)
	}
}

// Security-audit finding M-04: handleChat used to decode the request body
// with no size cap at all -- a caller could send an arbitrarily large body
// and it would be fully buffered into memory before any validation ran.
// http.MaxBytesReader now rejects anything over maxChatBodyBytes before
// decode gets anywhere near it.
func TestResourceChat_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	oversized := strings.Repeat("a", maxChatBodyBytes+1024)
	body, err := json.Marshal(map[string]string{
		"mode":    "chat",
		"prompt":  "test",
		"context": "{}",
		"padding": oversized,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := &backend.CallResourceRequest{
		Path:   "chat",
		Method: http.MethodPost,
		Body:   body,
	}

	var statusCode int
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode < 400 || statusCode >= 500 {
		t.Errorf("status = %d, want a 4xx rejection of the oversized body", statusCode)
	}
}

// The /chat/stream path (handleStreamResource, routed via CallResource) had
// no equivalent size cap at all -- unlike handleChat, it can't wrap the body
// in http.MaxBytesReader (CallResourceRequest.Body already arrives as a
// fully-read []byte over the plugin protocol), but nothing else stood in
// for that check either, so an oversized body reached json.Unmarshal
// unchecked.
func TestResourceChatStream_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	oversized := strings.Repeat("a", maxChatBodyBytes+1024)
	body, err := json.Marshal(map[string]string{
		"mode":    "chat",
		"prompt":  "test",
		"context": "{}",
		"padding": oversized,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	req := &backend.CallResourceRequest{
		Path:   "chat/stream",
		Method: http.MethodPost,
		Body:   body,
	}

	var statusCode int
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d for an oversized /chat/stream body", statusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestResourceChat_InvalidMode(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	req := &backend.CallResourceRequest{
		Path:   "chat",
		Method: http.MethodPost,
		Body:   []byte(`{"mode":"invalid_mode","prompt":"test","context":{}}`),
	}

	var statusCode int

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", statusCode, http.StatusBadRequest)
	}
}

func TestResourceChat_Success(t *testing.T) {
	t.Parallel()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		bodyBytes, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		_ = json.Unmarshal(bodyBytes, &reqBody)

		if reqBody["model"] != "test-model" {
			t.Errorf("model = %v, want test-model", reqBody["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"content": "This panel shows test data."}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5}
		}`))
	}))
	defer llmServer.Close()

	app := newTestApp(t, llmServer.URL+"/v1", "key")

	chatReq := `{
		"mode": "explain_panel",
		"prompt": "What does this show?",
		"context": {"panel": {"title": "Test"}}
	}`

	req := &backend.CallResourceRequest{
		Path:   "chat",
		Method: http.MethodPost,
		Body:   []byte(chatReq),
	}

	var statusCode int
	var body []byte

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		body = res.Body
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", statusCode, http.StatusOK, string(body))
	}

	var resp ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if !strings.Contains(resp.Content, "test data") {
		t.Errorf("content = %q, expected to contain 'test data'", resp.Content)
	}
}

func TestResourceUnknownPath(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	req := &backend.CallResourceRequest{
		Path:   "unknown",
		Method: http.MethodGet,
	}

	var statusCode int

	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", statusCode, http.StatusNotFound)
	}
}

func TestStreamResource_RateLimitExceeded(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	// Exhaust the burst of 10
	for range 10 {
		req := &backend.CallResourceRequest{
			Path:    "chat/stream",
			Method:  http.MethodPost,
			Body:    []byte(`{"mode":"chat","prompt":"test","context":{}}`),
			Headers: map[string][]string{"X-Grafana-User": {"testuser"}},
		}
		sender := backend.CallResourceResponseSenderFunc(func(_ *backend.CallResourceResponse) error { return nil })
		_ = app.CallResource(context.Background(), req, sender)
	}

	// 11th request should be rate limited
	req := &backend.CallResourceRequest{
		Path:    "chat/stream",
		Method:  http.MethodPost,
		Body:    []byte(`{"mode":"chat","prompt":"test","context":{}}`),
		Headers: map[string][]string{"X-Grafana-User": {"testuser"}},
	}

	var statusCode int
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		return nil
	})

	err := app.CallResource(context.Background(), req, sender)
	if err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	if statusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", statusCode, http.StatusTooManyRequests)
	}
}

func TestRequestUser_FromPluginContext(t *testing.T) {
	t.Parallel()

	ctx := backend.WithPluginContext(context.Background(), backend.PluginContext{User: &backend.User{Login: "admin"}})
	if got := requestUser(ctx); got != "admin" {
		t.Errorf("requestUser() = %q, want %q", got, "admin")
	}
}

// Security-audit regression: a client-supplied X-Grafana-User header must
// NOT be able to spoof the identity used for rate-limiting/audit logging --
// only the authenticated plugin context counts.
func TestRequestUser_IgnoresSpoofedHeader(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-Grafana-User", "forged-admin-identity")
	ctx := backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Login: "real-user"}})
	if got := requestUser(ctx); got != "real-user" {
		t.Errorf("requestUser() = %q, want %q (the real authenticated login, not the spoofed header)", got, "real-user")
	}
}

func TestRequestUser_DefaultsToAnonymous(t *testing.T) {
	t.Parallel()

	if got := requestUser(context.Background()); got != "anonymous" {
		t.Errorf("requestUser(no plugin context) = %q, want %q", got, "anonymous")
	}
}

func TestHandleLimits_DefaultsToFeaturesEnabled(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://localhost:1/v1", "key")

	req := &backend.CallResourceRequest{Path: "limits", Method: http.MethodGet}
	var body []byte
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		body = res.Body
		return nil
	})
	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["enableStandaloneChat"] != true {
		t.Errorf("enableStandaloneChat = %v, want true (default)", resp["enableStandaloneChat"])
	}
	if resp["enableDashboardIntegration"] != true {
		t.Errorf("enableDashboardIntegration = %v, want true (default)", resp["enableDashboardIntegration"])
	}

	// Regression: the UI used to have no way to know the max attachment
	// COUNT or the total payload budget across all attachments combined --
	// only the per-attachment byte cap -- so it could let a user build a
	// request that passed every individual check yet still blew past
	// Grafana's own request-payload cap.
	maxAttachments, ok := resp["maxAttachments"].(float64)
	if !ok || maxAttachments != maxAttachmentsPerMessage {
		t.Errorf("maxAttachments = %v, want %d", resp["maxAttachments"], maxAttachmentsPerMessage)
	}
	maxTotal, ok := resp["maxAttachmentsTotalBytes"].(float64)
	if !ok || maxTotal <= 0 {
		t.Fatalf("maxAttachmentsTotalBytes = %v, want a positive number", resp["maxAttachmentsTotalBytes"])
	}
	if maxTotal >= maxChatBodyBytes {
		t.Errorf("maxAttachmentsTotalBytes = %v, want it to leave headroom under maxChatBodyBytes (%d) for the rest of the request", maxTotal, maxChatBodyBytes)
	}
}

func TestHandleLimits_RespectsDisabledToggles(t *testing.T) {
	t.Parallel()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":                "http://localhost:1/v1",
		"model":                      "test-model",
		"enableStandaloneChat":       false,
		"enableDashboardIntegration": false,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "key"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	req := &backend.CallResourceRequest{Path: "limits", Method: http.MethodGet}
	var body []byte
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		body = res.Body
		return nil
	})
	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["enableStandaloneChat"] != false {
		t.Errorf("enableStandaloneChat = %v, want false", resp["enableStandaloneChat"])
	}
	if resp["enableDashboardIntegration"] != false {
		t.Errorf("enableDashboardIntegration = %v, want false", resp["enableDashboardIntegration"])
	}
}

func TestResourceMetrics_ExposesPrometheusFormat(t *testing.T) {
	t.Parallel()

	app := newTestApp(t, "http://unused/v1", "key")
	app.metrics.recordRequest("test-model", "ok", 1.5, 10, 20)

	req := &backend.CallResourceRequest{
		Path:   "metrics",
		Method: http.MethodGet,
	}

	var statusCode int
	var body []byte
	sender := backend.CallResourceResponseSenderFunc(func(res *backend.CallResourceResponse) error {
		statusCode = res.Status
		body = res.Body
		return nil
	})
	if err := app.CallResource(context.Background(), req, sender); err != nil {
		t.Fatalf("CallResource returned error: %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", statusCode, http.StatusOK)
	}
	if !strings.Contains(string(body), "grafana_llm_requests_total") {
		t.Errorf("body missing expected metric name: %s", body)
	}
	if !strings.Contains(string(body), `model="test-model"`) {
		t.Errorf("body missing recorded label value: %s", body)
	}
}

func TestTryAcquireChatSlot_ExhaustsAtConfiguredCapacity(t *testing.T) {
	t.Parallel()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":        "http://127.0.0.1:1/v1",
		"model":              "test-model",
		"maxConcurrentChats": 2,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	release1, ok := app.tryAcquireChatSlot()
	if !ok {
		t.Fatal("1st acquire should succeed (capacity 2)")
	}
	_, ok = app.tryAcquireChatSlot()
	if !ok {
		t.Fatal("2nd acquire should succeed (capacity 2)")
	}
	if _, ok := app.tryAcquireChatSlot(); ok {
		t.Error("3rd acquire should fail -- capacity is 2 and both slots are held")
	}

	release1()
	if _, ok := app.tryAcquireChatSlot(); !ok {
		t.Error("acquire should succeed again after a release freed a slot")
	}
}

func newQueuedTestApp(t *testing.T, maxConcurrent, queueWaitSeconds, queueDepth int) *App {
	t.Helper()
	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":          "http://127.0.0.1:1/v1",
		"model":                "test-model",
		"maxConcurrentChats":   maxConcurrent,
		"chatQueueWaitSeconds": queueWaitSeconds,
		"chatQueueDepth":       queueDepth,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{JSONData: jsonData})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return inst.(*App)
}

// Fast path: a slot is already free -- must return immediately (~0s), never
// touching the queue, regardless of how queueing is configured. This is the
// "purely additive, never a new bottleneck" guarantee -- an install with
// real spare capacity must see zero added latency.
func TestTryAcquireChatSlotQueued_FastPathWhenSlotFree(t *testing.T) {
	t.Parallel()

	app := newQueuedTestApp(t, 2, 30, 50)
	t0 := time.Now()
	release, ok := app.tryAcquireChatSlotQueued(context.Background())
	elapsed := time.Since(t0)
	if !ok {
		t.Fatal("expected immediate success -- a slot was free")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("fast path took %v, want near-instant", elapsed)
	}
	release()
}

// Queued path: no slot free at first, but one opens up before the wait
// timeout -- the waiting caller must pick it up rather than time out.
func TestTryAcquireChatSlotQueued_SucceedsWhenSlotFreesUpInTime(t *testing.T) {
	t.Parallel()

	app := newQueuedTestApp(t, 1, 5, 10)
	release1, ok := app.tryAcquireChatSlotQueued(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed (capacity 1, nothing else running)")
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		release1()
	}()

	t0 := time.Now()
	release2, ok := app.tryAcquireChatSlotQueued(context.Background())
	elapsed := time.Since(t0)
	if !ok {
		t.Fatal("2nd acquire should have waited for the slot to free up and then succeeded")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed = %v, expected to have actually waited for the release (~200ms)", elapsed)
	}
	release2()
}

// Timeout path: no slot ever frees up -- the waiting caller must give up
// after ChatQueueWaitSeconds, not wait forever.
func TestTryAcquireChatSlotQueued_TimesOutWhenNoSlotFreesUp(t *testing.T) {
	t.Parallel()

	app := newQueuedTestApp(t, 1, 1, 10) // 1s wait
	release1, ok := app.tryAcquireChatSlotQueued(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed")
	}
	defer release1()

	t0 := time.Now()
	_, ok = app.tryAcquireChatSlotQueued(context.Background())
	elapsed := time.Since(t0)
	if ok {
		t.Error("2nd acquire should have timed out -- the 1st holder never released")
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, expected to have waited close to the configured 1s timeout", elapsed)
	}
}

// ChatQueueWaitSeconds=0 must restore the EXACT old fail-fast behavior --
// instant rejection, no waiting at all, for anyone who prefers that.
func TestTryAcquireChatSlotQueued_ZeroWaitSecondsFailsInstantly(t *testing.T) {
	t.Parallel()

	app := newQueuedTestApp(t, 1, 0, 50)
	release1, ok := app.tryAcquireChatSlotQueued(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed")
	}
	defer release1()

	t0 := time.Now()
	_, ok = app.tryAcquireChatSlotQueued(context.Background())
	elapsed := time.Since(t0)
	if ok {
		t.Error("2nd acquire should fail instantly -- queueing is disabled")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %v, want near-instant rejection with queueing disabled", elapsed)
	}
}

// ChatQueueDepth bounds how many callers may wait at once, independent of
// the wait timeout -- a caller beyond that depth must fail immediately
// instead of joining an unbounded backlog.
func TestTryAcquireChatSlotQueued_RejectsImmediatelyPastQueueDepth(t *testing.T) {
	t.Parallel()

	app := newQueuedTestApp(t, 1, 5, 2) // long wait, but only 2 may queue
	release1, ok := app.tryAcquireChatSlotQueued(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed")
	}
	defer release1()

	// Fill the queue depth (2 waiters) with goroutines that will block for
	// the full wait duration (nothing ever releases the running slot).
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.tryAcquireChatSlotQueued(context.Background())
		}()
	}
	// Give the two goroutines above time to actually register as waiting
	// before this goroutine's own attempt -- avoids a race where this call
	// checks the counter before the others increment it.
	time.Sleep(100 * time.Millisecond)

	t0 := time.Now()
	_, ok = app.tryAcquireChatSlotQueued(context.Background())
	elapsed := time.Since(t0)
	if ok {
		t.Error("3rd waiter should be rejected immediately -- queue depth is already 2")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %v, want near-instant rejection when queue depth is exceeded", elapsed)
	}
	wg.Wait()
}

// A caller's own context cancelling while queued must stop the wait
// promptly instead of blocking until the full timeout -- e.g. the original
// HTTP client disconnected/gave up.
func TestTryAcquireChatSlotQueued_StopsWaitingWhenContextCancelled(t *testing.T) {
	t.Parallel()

	app := newQueuedTestApp(t, 1, 30, 10) // long wait
	release1, ok := app.tryAcquireChatSlotQueued(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed")
	}
	defer release1()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	t0 := time.Now()
	_, ok = app.tryAcquireChatSlotQueued(ctx)
	elapsed := time.Since(t0)
	if ok {
		t.Error("acquire should fail -- context was cancelled before any slot freed up")
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, expected to stop waiting promptly after context cancellation (~150ms), not the full 30s configured timeout", elapsed)
	}
}

// Verifies the actual HTTP wiring (handleChat), not just the semaphore
// primitive: with MaxConcurrentChats=1, a request already in flight (the
// mock LLM endpoint blocks until released below) must make a second,
// concurrent request fail with 429 -- exactly the scenario getLimiter's
// per-user cap alone cannot catch (two DIFFERENT users would each pass
// their own limiter).
func TestHandleChat_GlobalCapacityReached(t *testing.T) {
	t.Parallel()

	requestArrived := make(chan struct{})
	releaseRequest := make(chan struct{})
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			close(requestArrived)
			<-releaseRequest
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer llmServer.Close()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":        llmServer.URL + "/v1",
		"model":              "test-model",
		"maxConcurrentChats": 1,
		// Disables queueing (see tryAcquireChatSlotQueued) so the 2nd
		// request rejects instantly, matching what this test actually
		// exercises (immediate capacity-reached rejection) -- otherwise
		// it would wait defaultChatQueueWaitSeconds before failing.
		"chatQueueWaitSeconds": 0,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "key"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"chat","prompt":"hello","context":{}}`))
		req = req.WithContext(backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Login: "user-a"}}))
		w := httptest.NewRecorder()
		app.handleChat(w, req)
		done <- w.Code
	}()

	select {
	case <-requestArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached the LLM mock")
	}

	// Second request, different user -- getLimiter's per-user cap would let
	// this straight through; only the global slot cap can reject it while
	// the first request is still in flight.
	req2 := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"chat","prompt":"hello","context":{}}`))
	req2 = req2.WithContext(backend.WithPluginContext(req2.Context(), backend.PluginContext{User: &backend.User{Login: "user-b"}}))
	w2 := httptest.NewRecorder()
	app.handleChat(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("2nd request status = %d, want %d (global capacity reached)", w2.Code, http.StatusTooManyRequests)
	}

	close(releaseRequest)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("first request status = %d, want %d", code, http.StatusOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request never completed after being released")
	}
}

// Resilience test requested explicitly: the chat-slot QUEUE must behave
// correctly no matter what the currently-running request is doing
// internally -- including a slow brain-agent-involving turn (memory
// prefetch/tool calls) -- since tryAcquireChatSlotQueued is acquired before
// any of that logic runs and released via defer after it, regardless of
// success/failure/slowness inside. Simulates: one request holding the only
// slot while it does brain-agent-style slow work, two more real handleChat
// calls queued behind it (maxConcurrentChats=1), one of which has its own
// request context cancelled mid-wait (a client giving up) -- the survivor
// must still pick up the slot once it frees, and the cancelled one must
// never block or corrupt the queue for anyone else.
func TestHandleChat_QueueResilientAroundSlowBrainAgentInvolvingRequest(t *testing.T) {
	t.Parallel()

	holderArrived := make(chan struct{})
	releaseHolder := make(chan struct{})
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			// The first caller to reach the LLM mock is the "holder" --
			// block it here to simulate a slow brain-agent memory
			// prefetch/tool round-trip happening inside this same held
			// chat slot, exactly like the real flow (chatCompletion calls
			// prefetchMemoryContext/executeToolCalls before ever returning,
			// all still holding the one slot acquired at the top of
			// handleChat).
			select {
			case <-holderArrived:
				// Not the first caller (a queued one that got through) --
				// answer immediately.
			default:
				close(holderArrived)
				<-releaseHolder
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer llmServer.Close()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":          llmServer.URL + "/v1",
		"model":                "test-model",
		"maxConcurrentChats":   1,
		"chatQueueWaitSeconds": 10,
		"chatQueueDepth":       10,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "key"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	holderDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"chat","prompt":"hello","context":{}}`))
		req = req.WithContext(backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Login: "user-holder"}}))
		w := httptest.NewRecorder()
		app.handleChat(w, req)
		holderDone <- w.Code
	}()

	select {
	case <-holderArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("holder request never reached the LLM mock")
	}

	// Queued caller #1: its own request context gets cancelled shortly
	// after joining the queue -- must give up promptly, never taking the
	// slot, never blocking caller #2 behind it.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancelledDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"chat","prompt":"hello","context":{}}`))
		req = req.WithContext(backend.WithPluginContext(cancelledCtx, backend.PluginContext{User: &backend.User{Login: "user-cancelled"}}))
		w := httptest.NewRecorder()
		app.handleChat(w, req)
		cancelledDone <- w.Code
	}()
	time.Sleep(100 * time.Millisecond) // let it actually join the queue
	cancel()

	select {
	case code := <-cancelledDone:
		if code != http.StatusTooManyRequests {
			t.Errorf("cancelled queued request status = %d, want %d", code, http.StatusTooManyRequests)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled queued request never returned -- queue must not block on a dead waiter")
	}

	// Queued caller #2 (the survivor): joins the queue after #1 was already
	// cancelled -- must still successfully wait for and receive the slot
	// once the holder (simulating the finished brain-agent-involving turn)
	// releases it.
	survivorDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"chat","prompt":"hello","context":{}}`))
		req = req.WithContext(backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Login: "user-survivor"}}))
		w := httptest.NewRecorder()
		app.handleChat(w, req)
		survivorDone <- w.Code
	}()
	time.Sleep(100 * time.Millisecond) // let it actually join the queue

	close(releaseHolder)

	select {
	case code := <-holderDone:
		if code != http.StatusOK {
			t.Errorf("holder status = %d, want %d", code, http.StatusOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("holder request never completed after being released")
	}

	select {
	case code := <-survivorDone:
		if code != http.StatusOK {
			t.Errorf("survivor (queued) status = %d, want %d -- it should have waited for the slot and then succeeded", code, http.StatusOK)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("survivor queued request never completed -- the queue must hand off the freed slot correctly")
	}
}

// Security regression test: a malformed/oversized request body must be
// rejected INSTANTLY, never waiting in the chat-slot queue first. Live-found
// during a security review of the queue feature: request validation ran
// AFTER tryAcquireChatSlotQueued, so a garbage body could occupy a shared,
// limited resource (chatQueueDepth) for up to chatQueueWaitSeconds before
// ever being rejected -- letting an attacker starve out legitimate queued
// callers with cheap, invalid requests. Fixed by validating before ever
// attempting to acquire/queue for a slot; this test proves the fix by
// holding the only slot (so the queue path would definitely trigger if
// reached) and sending an invalid body as the 2nd request -- it must fail
// near-instantly, not after waiting.
func TestHandleChat_InvalidBodyRejectedInstantlyEvenWhenSlotSaturated(t *testing.T) {
	t.Parallel()

	requestArrived := make(chan struct{})
	releaseRequest := make(chan struct{})
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			close(requestArrived)
			<-releaseRequest
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer llmServer.Close()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":          llmServer.URL + "/v1",
		"model":                "test-model",
		"maxConcurrentChats":   1,
		"chatQueueWaitSeconds": 10, // long enough that a bug (validating after queueing) would clearly show up as a slow response
		"chatQueueDepth":       10,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "key"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"chat","prompt":"hello","context":{}}`))
		req = req.WithContext(backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Login: "user-holder"}}))
		w := httptest.NewRecorder()
		app.handleChat(w, req)
	}()

	select {
	case <-requestArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("holder request never reached the LLM mock")
	}
	defer close(releaseRequest)

	// Invalid mode -- a cheap, obviously-rejectable request. Must fail
	// instantly (well under the 10s queue timeout), proving it was
	// validated BEFORE ever attempting to queue for the saturated slot.
	req2 := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"mode":"not_a_real_mode","prompt":"hello","context":{}}`))
	req2 = req2.WithContext(backend.WithPluginContext(req2.Context(), backend.PluginContext{User: &backend.User{Login: "user-attacker"}}))
	w2 := httptest.NewRecorder()

	t0 := time.Now()
	app.handleChat(w2, req2)
	elapsed := time.Since(t0)

	if w2.Code != http.StatusBadRequest {
		t.Errorf("invalid-mode request status = %d, want %d", w2.Code, http.StatusBadRequest)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("invalid-mode request took %v -- it must be rejected instantly, before ever touching the chat-slot queue (which is saturated and would make it wait up to the configured 10s)", elapsed)
	}
}

// Security test: with several DIFFERENT users' requests queued/running
// concurrently through the queue, each caller must receive ONLY their own
// response -- never another caller's. Uses distinct, unmistakable markers
// per user threaded through a real mock LLM round-trip so any cross-wiring
// in the queue hand-off would be immediately visible as a mismatched
// marker, not just a generic "it worked" pass.
func TestHandleChat_QueuedRequestsNeverCrossUserResponses(t *testing.T) {
	t.Parallel()

	const numUsers = 6
	type callInfo struct {
		marker string
	}
	var mu sync.Mutex
	var calls []callInfo

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var lastUserMsg string
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role == "user" {
				lastUserMsg = body.Messages[i].Content
				break
			}
		}
		mu.Lock()
		calls = append(calls, callInfo{marker: lastUserMsg})
		mu.Unlock()

		// Deliberately stagger responses so requests genuinely interleave
		// through the queue (some finish while others are still waiting),
		// maximizing the chance any cross-wiring bug would surface.
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id": "1", "choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "echo: " + lastUserMsg}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	jsonData, err := json.Marshal(map[string]any{
		"endpointURL":          llmServer.URL + "/v1",
		"model":                "test-model",
		"maxConcurrentChats":   2,
		"chatQueueWaitSeconds": 10,
		"chatQueueDepth":       50,
	})
	if err != nil {
		t.Fatalf("marshal jsonData: %v", err)
	}
	inst, err := NewApp(context.Background(), backend.AppInstanceSettings{
		JSONData:                jsonData,
		DecryptedSecureJSONData: map[string]string{"apiKey": "key"},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app := inst.(*App)

	var wg sync.WaitGroup
	responses := make([]string, numUsers)
	statuses := make([]int, numUsers)
	for i := 0; i < numUsers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("UNIQUE-MARKER-USER-%d", i)
			body := fmt.Sprintf(`{"mode":"chat","prompt":%q,"context":{}}`, marker)
			req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
			req = req.WithContext(backend.WithPluginContext(req.Context(), backend.PluginContext{User: &backend.User{Login: fmt.Sprintf("user-%d", i)}}))
			w := httptest.NewRecorder()
			app.handleChat(w, req)
			statuses[i] = w.Code
			var parsed struct {
				Content string `json:"content"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &parsed)
			responses[i] = parsed.Content
		}(i)
	}
	wg.Wait()

	for i := 0; i < numUsers; i++ {
		ownMarker := fmt.Sprintf("UNIQUE-MARKER-USER-%d", i)
		if statuses[i] != http.StatusOK {
			t.Errorf("user %d: status = %d, want %d", i, statuses[i], http.StatusOK)
			continue
		}
		if !strings.Contains(responses[i], ownMarker) {
			t.Errorf("user %d: response = %q, want it to contain its own marker %q -- possible cross-user leak", i, responses[i], ownMarker)
		}
		for j := 0; j < numUsers; j++ {
			if j == i {
				continue
			}
			foreignMarker := fmt.Sprintf("UNIQUE-MARKER-USER-%d", j)
			if strings.Contains(responses[i], foreignMarker) {
				t.Errorf("user %d: response = %q contains user %d's marker -- CROSS-USER LEAK", i, responses[i], j)
			}
		}
	}
}
