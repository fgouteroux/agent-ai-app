package plugin

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestSanitizeToolCallArguments_ReplacesInvalidJSON(t *testing.T) {
	t.Parallel()

	calls := []openai.ToolCall{
		{ID: "call_1", Function: openai.FunctionCall{Name: "query_prometheus", Arguments: `{"expr": "up"`}}, // truncated, invalid
		{ID: "call_2", Function: openai.FunctionCall{Name: "list_datasources", Arguments: "{}"}},            // already valid
	}

	got := sanitizeToolCallArguments(calls)

	if got[0].Function.Arguments != "{}" {
		t.Errorf("call_1 Arguments = %q, want it replaced with \"{}\" (was invalid JSON)", got[0].Function.Arguments)
	}
	if got[1].Function.Arguments != "{}" {
		t.Errorf("call_2 Arguments = %q, want it left unchanged (was already valid JSON)", got[1].Function.Arguments)
	}
	// The original slice/calls must be untouched -- the caller still needs
	// the ORIGINAL (possibly invalid) arguments to execute the tool and
	// report a faithful error.
	if calls[0].Function.Arguments != `{"expr": "up"` {
		t.Errorf("original calls[0] was mutated: %q", calls[0].Function.Arguments)
	}
}

func TestExtractPseudoToolCalls_NoTag(t *testing.T) {
	clean, calls := extractPseudoToolCalls("Hello, how can I help?")
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %d", len(calls))
	}
	if clean != "Hello, how can I help?" {
		t.Fatalf("unexpected clean text: %q", clean)
	}
}

func TestExtractPseudoToolCalls_NoArgs(t *testing.T) {
	clean, calls := extractPseudoToolCalls("Hi! <function=list_dashboards></function>")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "list_dashboards" {
		t.Fatalf("unexpected tool name: %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != "{}" {
		t.Fatalf("expected empty-args to normalize to {}, got %q", calls[0].Function.Arguments)
	}
	if clean != "Hi!" {
		t.Fatalf("unexpected clean text: %q", clean)
	}
}

func TestExtractPseudoToolCalls_WithArgs(t *testing.T) {
	clean, calls := extractPseudoToolCalls(`<function=list_dashboards>{"query":"kafka"}</function>`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Arguments != `{"query":"kafka"}` {
		t.Fatalf("unexpected arguments: %q", calls[0].Function.Arguments)
	}
	if clean != "" {
		t.Fatalf("expected empty clean text, got %q", clean)
	}
}

func TestExtractPseudoToolCalls_Multiple(t *testing.T) {
	_, calls := extractPseudoToolCalls("<function=list_folders></function> then <function=list_dashboards></function>")
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "list_folders" || calls[1].Function.Name != "list_dashboards" {
		t.Fatalf("unexpected call order/names: %+v", calls)
	}
}

func TestExtractPseudoToolCalls_JSONBlob(t *testing.T) {
	clean, calls := extractPseudoToolCalls(`I'll check that. {"name": "find_dashboards", "arguments": {"topic": "deploy"}} Let me look.`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "find_dashboards" {
		t.Fatalf("unexpected tool name: %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"topic": "deploy"}` {
		t.Fatalf("unexpected arguments: %q", calls[0].Function.Arguments)
	}
	if clean != "I'll check that.  Let me look." {
		t.Fatalf("unexpected clean text: %q", clean)
	}
}

func TestExtractPseudoToolCalls_JSONBlobWithStrayMarkerWords(t *testing.T) {
	// Real observed shape from qwen2.5:14b-instruct: a marker-like word
	// before the JSON and a garbled token after it, neither of which is
	// part of the JSON object itself.
	clean, calls := extractPseudoToolCalls("CallCheck\n{\"name\": \"find_dashboards\", \"arguments\": {\"topic\": \"deploy\"}}\nичество")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "find_dashboards" {
		t.Fatalf("unexpected tool name: %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"topic": "deploy"}` {
		t.Fatalf("unexpected arguments: %q", calls[0].Function.Arguments)
	}
	// The stray marker words around the JSON aren't part of the match, so
	// they remain in the clean text -- this is a cosmetic residue, not a
	// leaked tool-call payload, which is the part that actually matters.
	if strings.Contains(clean, `"name"`) || strings.Contains(clean, `"arguments"`) {
		t.Fatalf("raw JSON tool-call payload leaked into clean text: %q", clean)
	}
}

// Regression test for a real live-validation finding: llama3.2:3b emits its
// own OpenAI-shaped tool-call JSON ({"type":"function","name":...,
// "parameters":{...}}) as plain content -- a third pseudo-tool-call shape
// distinct from the other two (leads with "type", uses "parameters" not
// "arguments"). The exact live example leaked a rejected write attempt
// (DELETE FROM ...) as raw JSON to the user instead of it becoming a real,
// safely-rejected tool call.
func TestExtractPseudoToolCalls_OpenAIShapedJSONBlob(t *testing.T) {
	clean, calls := extractPseudoToolCalls(`Sure, here's the call: {"type": "function", "name": "query_datasource", "parameters": {"datasource_uid": "your-datasource-uid-here", "max_rows": "0", "query": "DELETE FROM deployments WHERE id=1"}}`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "query_datasource" {
		t.Fatalf("unexpected tool name: %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"datasource_uid": "your-datasource-uid-here", "max_rows": "0", "query": "DELETE FROM deployments WHERE id=1"}` {
		t.Fatalf("unexpected arguments: %q", calls[0].Function.Arguments)
	}
	if strings.Contains(clean, `"parameters"`) || strings.Contains(clean, "DELETE FROM") {
		t.Fatalf("raw JSON tool-call payload leaked into clean text: %q", clean)
	}
}

func TestExtractPseudoToolCalls_OpenAIShapedJSONBlob_ArgumentsKeyVariant(t *testing.T) {
	_, calls := extractPseudoToolCalls(`{"type": "function", "name": "list_dashboards", "arguments": {"folder": "SRE"}}`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Arguments != `{"folder": "SRE"}` {
		t.Fatalf("unexpected arguments: %q", calls[0].Function.Arguments)
	}
}

func TestExtractPseudoToolCalls_MixedTagAndJSONBlob(t *testing.T) {
	clean, calls := extractPseudoToolCalls(`<function=list_folders></function> then {"name": "list_dashboards", "arguments": {}}`)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "list_folders" || calls[1].Function.Name != "list_dashboards" {
		t.Fatalf("unexpected call order/names: %+v", calls)
	}
	if strings.Contains(clean, `"name"`) {
		t.Fatalf("raw JSON tool-call payload leaked into clean text: %q", clean)
	}
}

func TestExtractPseudoToolCalls_NestedArgumentsObjectNotMatched(t *testing.T) {
	// A nested arguments object isn't handled by the flat-object pattern --
	// documented limitation, not a crash: it just doesn't become a pseudo
	// tool call (same outcome as any other malformed pseudo-call shape).
	text := `{"name": "weird_tool", "arguments": {"outer": {"inner": 1}}}`
	clean, calls := extractPseudoToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls for a nested-arguments blob, got %d", len(calls))
	}
	if clean != text {
		t.Fatalf("unmatched content should pass through unchanged, got %q", clean)
	}
}

func TestExtractPseudoToolCalls_NoArgsJSONBlob(t *testing.T) {
	clean, calls := extractPseudoToolCalls(`{"name": "list_datasources"}`)
	if clean != "" {
		t.Errorf("expected clean text to be empty, got %q", clean)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "list_datasources" {
		t.Errorf("expected list_datasources, got %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != "{}" {
		t.Errorf("expected {}, got %q", calls[0].Function.Arguments)
	}
}
