package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func inspectAlertMock(t *testing.T, rulesJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rulesJSON))
	}))
}

func TestInspectAlert_MatchesByRuleUID_GrafanaAlertShape(t *testing.T) {
	t.Parallel()

	rulesJSON := `{
		"Demo App": [
			{
				"name": "checkout-rules",
				"rules": [
					{
						"grafana_alert": {"uid": "abc123", "title": "HighErrorRate"},
						"for": "5m",
						"labels": {"severity": "critical"},
						"annotations": {"runbook_url": "https://wiki.example.com/runbooks/high-error-rate"}
					}
				]
			}
		]
	}`
	server := inspectAlertMock(t, rulesJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.inspectAlert(context.Background(), `{"rule_uid":"abc123"}`)
	if err != nil {
		t.Fatalf("inspectAlert returned error: %v", err)
	}

	var parsed inspectedRule
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.Name != "HighErrorRate" {
		t.Errorf("name = %q, want HighErrorRate", parsed.Name)
	}
	if parsed.Folder != "Demo App" || parsed.Group != "checkout-rules" {
		t.Errorf("folder/group = %q/%q, want Demo App/checkout-rules", parsed.Folder, parsed.Group)
	}
	if !parsed.HasRunbookLink {
		t.Error("expected HasRunbookLink = true")
	}
	if parsed.RunbookURL != "https://wiki.example.com/runbooks/high-error-rate" {
		t.Errorf("runbookUrl = %q, want the real URL", parsed.RunbookURL)
	}
	if !parsed.HasForDuration {
		t.Error("expected HasForDuration = true")
	}
}

func TestInspectAlert_MatchesByAlertName_PrometheusStyleShape(t *testing.T) {
	t.Parallel()

	rulesJSON := `{
		"Demo App": [
			{
				"name": "checkout-rules",
				"rules": [
					{
						"alert": "DiskFull",
						"expr": "disk_free_percent < 10",
						"labels": {"severity": "warning"},
						"annotations": {}
					}
				]
			}
		]
	}`
	server := inspectAlertMock(t, rulesJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.inspectAlert(context.Background(), `{"alertname":"DiskFull"}`)
	if err != nil {
		t.Fatalf("inspectAlert returned error: %v", err)
	}

	var parsed inspectedRule
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.Name != "DiskFull" {
		t.Errorf("name = %q, want DiskFull", parsed.Name)
	}
	if parsed.Expression != "disk_free_percent < 10" {
		t.Errorf("expression = %q, want the real PromQL", parsed.Expression)
	}
	if parsed.HasRunbookLink {
		t.Error("expected HasRunbookLink = false (no runbook annotation)")
	}
	if parsed.HasForDuration {
		t.Error("expected HasForDuration = false (no \"for\" field)")
	}
	if parsed.NoQuietPeriodNote == "" {
		t.Error("expected a note about the missing \"for\" duration")
	}
}

func TestInspectAlert_NoMatchReturnsGracefulMessage(t *testing.T) {
	t.Parallel()

	server := inspectAlertMock(t, `{"Demo App": [{"name": "g", "rules": [{"alert": "OtherAlert"}]}]}`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.inspectAlert(context.Background(), `{"alertname":"DoesNotExist"}`)
	if err != nil {
		t.Fatalf("inspectAlert returned error: %v", err)
	}
	if !strings.Contains(result, "No alert rule matching") {
		t.Errorf("result = %q, want a graceful no-match message", result)
	}
}

func TestInspectAlert_RequiresRuleUIDOrAlertName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.inspectAlert(context.Background(), `{}`); err == nil {
		t.Error("expected an error when neither rule_uid nor alertname is given")
	}
}
