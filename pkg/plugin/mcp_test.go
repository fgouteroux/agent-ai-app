package plugin

import (
	"context"
	"encoding/json"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func mcpRPCHandler(t *testing.T, handleMethod func(method string, body []byte) (int, string)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// doRPC checks brain-agent's in-transit-encoding status before every
		// call (cached, but the cache starts cold in each test) -- respond
		// with "disabled" so tests exercise the plain, unencoded path unless
		// they deliberately want otherwise.
		if r.URL.Path == inTransitStatusPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"enabled": false}`))
			return
		}
		if r.URL.Path != mcpToolsPath {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req struct {
			Method string `json:"method"`
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		_ = json.Unmarshal(body, &req)

		status, respBody := handleMethod(req.Method, body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}
}

func TestMCPClient_Tools_FiltersReadOnlyAndCollisions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		if method != "tools/list" {
			t.Errorf("unexpected method: %s", method)
		}
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"tools":[
			{"name":"list_datasources","description":"dup","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},
			{"name":"query_prometheus","description":"dup","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},
			{"name":"get_current_oncall_users","description":"who is oncall","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},
			{"name":"create_annotation","description":"writes data","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false}},
			{"name":"alerting_manage_rules","description":"destructive","inputSchema":{"type":"object"},"annotations":{"destructiveHint":true}}
		]}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	tools := c.Tools(context.Background())

	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1 (only get_current_oncall_users should survive), got %+v", len(tools), tools)
	}
	if tools[0].Function.Name != "get_current_oncall_users" {
		t.Errorf("tools[0].Function.Name = %q, want get_current_oncall_users", tools[0].Function.Name)
	}
	if !c.HasTool("get_current_oncall_users") {
		t.Error("HasTool(get_current_oncall_users) = false, want true")
	}
	if c.HasTool("list_datasources") {
		t.Error("HasTool(list_datasources) = true, want false (collides with our own tool)")
	}
	if c.HasTool("create_annotation") {
		t.Error("HasTool(create_annotation) = true, want false (not read-only)")
	}
}

func TestMCPClient_Tools_AllowsStoreUpsertAndSuggestMemoryButNotDeleteOrCondense(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"tools":[
			{"name":"store_memory","description":"write","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false}},
			{"name":"upsert_memory","description":"write, deduped","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false}},
			{"name":"suggest_memory","description":"write, queued for approval","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false}},
			{"name":"search_memory","description":"read","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},
			{"name":"delete_memory","description":"destructive write","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false}},
			{"name":"condense_memory","description":"destructive write","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":false}}
		]}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	c.Tools(context.Background())

	for _, allowed := range []string{"store_memory", "upsert_memory", "suggest_memory", "search_memory"} {
		if !c.HasTool(allowed) {
			t.Errorf("HasTool(%q) = false, want true", allowed)
		}
	}
	for _, blocked := range []string{"delete_memory", "condense_memory"} {
		if c.HasTool(blocked) {
			t.Errorf("HasTool(%q) = true, want false (destructive memory ops must never reach the LLM directly)", blocked)
		}
	}
}

func TestTrimSchemaDescriptions(t *testing.T) {
	t.Parallel()

	longDesc := ""
	for len(longDesc) < 300 {
		longDesc += "this is a very long description that goes on and on. "
	}
	schema := json.RawMessage(`{"type":"object","required":["operation"],"properties":{
		"operation":{"type":"string","enum":["a","b"],"description":"` + longDesc + `"},
		"limit":{"type":"integer","description":"short"}
	}}`)

	trimmed := trimSchemaDescriptions(schema)

	var parsed map[string]any
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		t.Fatalf("trimmed schema is not valid JSON: %v", err)
	}
	props := parsed["properties"].(map[string]any)
	opDesc := props["operation"].(map[string]any)["description"].(string)
	if len(opDesc) > mcpMaxPropertyDescriptionLen+len("... [truncated]") {
		t.Errorf("operation description not trimmed: len=%d", len(opDesc))
	}
	limitDesc := props["limit"].(map[string]any)["description"].(string)
	if limitDesc != "short" {
		t.Errorf("short description should be left untouched, got %q", limitDesc)
	}
	// type/enum/required must survive untouched -- they're what makes the
	// tool actually callable correctly.
	if parsed["required"].([]any)[0] != "operation" {
		t.Error("required field was lost during trimming")
	}
	opEnum := props["operation"].(map[string]any)["enum"].([]any)
	if len(opEnum) != 2 {
		t.Error("enum field was lost during trimming")
	}
}

func TestTrimSchemaDescriptions_NoPropertiesReturnsUnchanged(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object"}`)
	trimmed := trimSchemaDescriptions(schema)
	if string(trimmed) != string(schema) {
		t.Errorf("schema without properties should be returned as-is (semantically); got %s", trimmed)
	}
}

func TestMCPClient_Tools_EmptySchemaDefaultsToEmptyObject(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"tools":[
			{"name":"list_teams","description":"list teams","annotations":{"readOnlyHint":true}}
		]}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	tools := c.Tools(context.Background())
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].Function.Parameters.(json.RawMessage), &schema); err != nil {
		t.Fatalf("parameters should be valid JSON: %v", err)
	}
}

func TestMCPClient_Tools_CachesWithinTTL(t *testing.T) {
	t.Parallel()

	var calls int32
	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		atomic.AddInt32(&calls, 1)
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	c.Tools(context.Background())
	c.Tools(context.Background())
	c.Tools(context.Background())

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server called %d times, want 1 (should be cached within TTL)", got)
	}
}

func TestMCPClient_Tools_FailureStillMarksCacheFresh(t *testing.T) {
	t.Parallel()

	var calls int32
	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		atomic.AddInt32(&calls, 1)
		return http.StatusInternalServerError, `{}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	tools1 := c.Tools(context.Background())
	tools2 := c.Tools(context.Background())

	if tools1 != nil || tools2 != nil {
		t.Errorf("expected nil tools on repeated failure, got %+v / %+v", tools1, tools2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server called %d times, want 1 (failure should still bound retries to once per TTL)", got)
	}
}

func TestMCPClient_Tools_NoTokenNeverMakesARequest(t *testing.T) {
	t.Parallel()

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "" }, log.DefaultLogger)
	tools := c.Tools(context.Background())

	if tools != nil {
		t.Errorf("tools = %+v, want nil", tools)
	}
	if called {
		t.Error("server was called despite no token being configured")
	}
}

func TestMCPClient_Call_ReturnsJoinedText(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, body []byte) (int, string) {
		if method != "tools/call" {
			t.Errorf("unexpected method: %s", method)
		}
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Params.Name != "list_teams" {
			t.Errorf("params.name = %q, want list_teams", req.Params.Name)
		}
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{\"name\":\"SRE\"}]"}]}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	result, err := c.Call(context.Background(), "list_teams", "{}")
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if result != `[{"name":"SRE"}]` {
		t.Errorf("result = %q, want the joined content text", result)
	}
}

func TestMCPClient_Call_IsErrorBecomesGoError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"list datasources: [GET /datasources][401] Unauthorized"}],"isError":true}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	_, err := c.Call(context.Background(), "list_datasources", "{}")
	if err == nil {
		t.Fatal("expected an error when the MCP result has isError:true")
	}
}

func TestMCPClient_Call_InvalidArgumentsJSON(t *testing.T) {
	t.Parallel()

	c := newMCPClient("http://localhost:1", func() string { return "token" }, log.DefaultLogger)
	_, err := c.Call(context.Background(), "list_teams", "{not valid json")
	if err == nil {
		t.Fatal("expected an error for invalid arguments JSON")
	}
}

func TestMCPClient_Call_ServerErrorStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		return http.StatusUnauthorized, `{}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	_, err := c.Call(context.Background(), "list_teams", "{}")
	if err == nil {
		t.Fatal("expected an error for a non-200 mcp server response")
	}
}

func TestMCPClient_Call_RPCLevelError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	_, err := c.Call(context.Background(), "list_teams", "{}")
	if err == nil {
		t.Fatal("expected an error for a JSON-RPC level error response")
	}
}

func TestToolExecutor_RoutesUnknownToolToMCP(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		switch method {
		case "tools/list":
			return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"tools":[
				{"name":"get_current_oncall_users","description":"who is oncall","annotations":{"readOnlyHint":true}}
			]}}`
		case "tools/call":
			return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"alice"}]}}`
		default:
			t.Fatalf("unexpected method: %s", method)
			return 0, ""
		}
	}))
	defer server.Close()

	te := NewToolExecutor("http://localhost:1", log.DefaultLogger)
	te.mcp = newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)

	// Warm the cache the same way a real request would (tools sent to the
	// LLM before any tool call comes back).
	te.mcp.Tools(context.Background())

	result, err := te.Execute(context.Background(), "get_current_oncall_users", "{}")
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result != "alice" {
		t.Errorf("result = %q, want %q", result, "alice")
	}
}

func TestToolExecutor_UnknownToolWithoutMCPStillErrors(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://localhost:1", log.DefaultLogger)
	_, err := te.Execute(context.Background(), "totally_made_up_tool", "{}")
	if err == nil {
		t.Fatal("expected an error for an unknown tool with no MCP client configured")
	}
}

func TestToolExecutor_ToolNotExposedByMCPStillErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mcpRPCHandler(t, func(method string, _ []byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
	}))
	defer server.Close()

	te := NewToolExecutor("http://localhost:1", log.DefaultLogger)
	te.mcp = newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	te.mcp.Tools(context.Background())

	_, err := te.Execute(context.Background(), "some_tool_mcp_doesnt_have", "{}")
	if err == nil {
		t.Fatal("expected an error for a tool name MCP doesn't expose")
	}
}

func TestNewMCPClient_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	c := newMCPClient("http://localhost:3000/", func() string { return "token" }, log.DefaultLogger)
	if c.grafanaURL != "http://localhost:3000" {
		t.Errorf("grafanaURL = %q, want trimmed trailing slash", c.grafanaURL)
	}
}

// Security-audit finding M3: brain-agent's "RPC Bus" toggle used to be
// checked via a shared /tmp sentinel file both plugins read directly,
// assuming they share a filesystem -- broken across pods/replicas, and
// doesn't survive a restart. It now lives in brain-agent's own settings,
// checked over HTTP through its /encryption_in_transit/status route.
func TestMCPClient_InTransitEncodingEnabled_ReflectsBrainAgentStatus(t *testing.T) {
	t.Parallel()

	var statusCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != inTransitStatusPath {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&statusCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": true})
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	if !c.inTransitEncodingEnabled(context.Background(), "token") {
		t.Error("expected true, matching brain-agent's reported status")
	}
	// Second call within TTL must not hit the server again.
	c.inTransitEncodingEnabled(context.Background(), "token")
	if got := atomic.LoadInt32(&statusCalls); got != 1 {
		t.Errorf("server called %d times, want 1 (should be cached within TTL)", got)
	}
}

func TestMCPClient_InTransitEncodingEnabled_DefaultsFalseOnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	if c.inTransitEncodingEnabled(context.Background(), "token") {
		t.Error("expected false when brain-agent's status endpoint fails, not a guess of true")
	}
}

func TestMCPClient_DoRPC_TimesOutIndependentlyOfParentContext(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer server.Close()

	c := newMCPClient(server.URL, func() string { return "token" }, log.DefaultLogger)
	_, err := c.doRPC(context.Background(), 10*time.Millisecond, "tools/list", nil)
	if err == nil {
		t.Fatal("expected a timeout error with a 10ms budget against a 200ms-slow server")
	}
}
