package plugin

import (
	"encoding/json"
	"testing"
)

func TestFilterEnabledTools_EmptyAllowlistReturnsAllUnchanged(t *testing.T) {
	t.Parallel()

	tools := llmTools("generic")
	filtered := filterEnabledTools(tools, nil)
	if len(filtered) != len(tools) {
		t.Fatalf("empty allowlist should return all %d tools unchanged, got %d", len(tools), len(filtered))
	}
}

func TestFilterEnabledTools_RestrictsToNamedTools(t *testing.T) {
	t.Parallel()

	tools := llmTools("generic")
	filtered := filterEnabledTools(tools, []string{"list_dashboards", "query_prometheus"})
	if len(filtered) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(filtered))
	}
	names := map[string]bool{}
	for _, t := range filtered {
		names[t.Function.Name] = true
	}
	if !names["list_dashboards"] || !names["query_prometheus"] {
		t.Errorf("expected list_dashboards and query_prometheus, got %v", names)
	}
}

func TestFilterEnabledTools_UnknownNameHasNoEffect(t *testing.T) {
	t.Parallel()

	tools := llmTools("generic")
	filtered := filterEnabledTools(tools, []string{"list_dashboards", "not_a_real_tool"})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 tool (unknown name ignored, not an error), got %d", len(filtered))
	}
}

func TestLLMTools_ReturnsExpectedTools(t *testing.T) {
	t.Parallel()

	tools := llmTools("generic")
	if len(tools) != 32 {
		t.Fatalf("expected 32 tools for the generic agent, got %d", len(tools))
	}

	expected := map[string]bool{
		"query_prometheus":             false,
		"query_loki":                   false,
		"list_loki_labels":             false,
		"analyze_metric_anomaly":       false,
		"forecast_capacity":            false,
		"diagnose_kubernetes_workload": false,
		"analyze_container_lifecycle":  false,
		"analyze_node_health":          false,
		"inspect_kubernetes_events":    false,
		"analyze_slo_burn_rate":        false,
		"analyze_log_patterns":         false,
		"query_tempo":                  false,
		"list_datasources":             false,
		"list_correlations":            false,
		"follow_correlation":           false,
		"build_change_timeline":        false,
		"list_folders":                 false,
		"list_dashboards":              false,
		"find_dashboards":              false,
		"get_dashboard":                false,
		"list_alerts":                  false,
		"list_alert_rules":             false,
		"inspect_alert":                false,
		"assess_alert_quality":         false,
		"check_observability_coverage": false,
		"analyze_active_alerts":        false,
		"investigate_alert":            false,
		"investigate_incident":         false,
		"analyze_trace_bottlenecks":    false,
		"build_service_topology":       false,
		"analyze_cloud_resource":       false,
		"query_datasource":             false,
	}

	for _, tool := range tools {
		if tool.Function == nil {
			t.Error("tool has nil function definition")
			continue
		}
		name := tool.Function.Name
		if _, ok := expected[name]; !ok {
			t.Errorf("unexpected tool: %s", name)
		}
		expected[name] = true

		if tool.Function.Description == "" {
			t.Errorf("tool %s has empty description", name)
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("expected tool %s not found", name)
		}
	}
}

func TestLLMTools_GenericAgentHasNoDispatchWorker(t *testing.T) {
	t.Parallel()

	for _, tool := range llmTools("generic") {
		if tool.Function != nil && tool.Function.Name == "dispatch_worker" {
			t.Error("generic agent must not get dispatch_worker -- subagents are specialist-only")
		}
	}
}

func TestLLMTools_SpecialistAgentGetsDispatchWorker(t *testing.T) {
	t.Parallel()

	found := false
	for _, tool := range llmTools("agent-1") {
		if tool.Function != nil && tool.Function.Name == "dispatch_worker" {
			found = true
		}
	}
	if !found {
		t.Error("expected dispatch_worker tool for a specialist agent")
	}
}

func TestLLMTools_PrometheusSchemaValid(t *testing.T) {
	t.Parallel()

	tools := llmTools("generic")
	var promTool *json.RawMessage
	for _, tool := range tools {
		if tool.Function != nil && tool.Function.Name == "query_prometheus" {
			raw, ok := tool.Function.Parameters.(json.RawMessage)
			if !ok {
				t.Fatal("expected json.RawMessage parameters")
			}
			promTool = &raw
			break
		}
	}

	if promTool == nil {
		t.Fatal("query_prometheus tool not found")
	}

	var schema map[string]any
	if err := json.Unmarshal(*promTool, &schema); err != nil {
		t.Fatalf("invalid JSON schema: %v", err)
	}

	if schema["type"] != "object" {
		t.Error("expected type=object")
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties object")
	}

	if _, ok := props["query"]; !ok {
		t.Error("expected 'query' property")
	}

	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("expected required array")
	}

	if len(required) != 1 || required[0] != "query" {
		t.Errorf("expected required=[query], got %v", required)
	}
}

func TestPrometheusQueryArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"query":"rate(http_requests_total[5m])","start":"now-1h","end":"now","step":"30s"}`
	var args PrometheusQueryArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.Query != "rate(http_requests_total[5m])" {
		t.Errorf("query = %q", args.Query)
	}
	if args.Start != "now-1h" {
		t.Errorf("start = %q", args.Start)
	}
	if args.Step != "30s" {
		t.Errorf("step = %q", args.Step)
	}
}

func TestLokiQueryArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"query":"{app=\"nginx\"} |= \"error\"","limit":50}`
	var args LokiQueryArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.Query != `{app="nginx"} |= "error"` {
		t.Errorf("query = %q", args.Query)
	}
	if args.Limit != 50 {
		t.Errorf("limit = %d", args.Limit)
	}
}

func TestListDashboardsArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"query":"kubernetes"}`
	var args ListDashboardsArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.Query != "kubernetes" {
		t.Errorf("query = %q", args.Query)
	}
}

func TestGetDashboardArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"uid":"abc-123"}`
	var args GetDashboardArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.UID != "abc-123" {
		t.Errorf("uid = %q", args.UID)
	}
}

func TestListAlertsArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"filter":"severity=critical","state":"firing"}`
	var args ListAlertsArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.Filter != "severity=critical" {
		t.Errorf("filter = %q", args.Filter)
	}
	if args.State != "firing" {
		t.Errorf("state = %q", args.State)
	}
}

func TestAnalyzeActiveAlertsArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"project":"platform-ai"}`
	var args AnalyzeActiveAlertsArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.Project != "platform-ai" {
		t.Errorf("project = %q", args.Project)
	}
}

func TestInvestigateAlertArgs_Unmarshal(t *testing.T) {
	t.Parallel()

	input := `{"alertname":"HighCPU","namespace":"prod","project":"checkout-team"}`
	var args InvestigateAlertArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.AlertName != "HighCPU" {
		t.Errorf("alertname = %q", args.AlertName)
	}
	if args.Namespace != "prod" {
		t.Errorf("namespace = %q", args.Namespace)
	}
	if args.Project != "checkout-team" {
		t.Errorf("project = %q", args.Project)
	}
}

func TestListAlertsArgs_Unmarshal_Empty(t *testing.T) {
	t.Parallel()

	input := `{}`
	var args ListAlertsArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if args.Filter != "" {
		t.Errorf("expected empty filter, got %q", args.Filter)
	}
	if args.State != "" {
		t.Errorf("expected empty state, got %q", args.State)
	}
}
