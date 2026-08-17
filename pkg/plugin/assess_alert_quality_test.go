package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func assessAlertQualityMock(t *testing.T, rulesJSON, alertsJSON string) *httptest.Server {
	t.Helper()
	var mux http.ServeMux
	mux.HandleFunc("/api/ruler/grafana/api/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rulesJSON))
	})
	mux.HandleFunc("/api/alertmanager/grafana/api/v2/alerts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(alertsJSON))
	})
	return httptest.NewServer(&mux)
}

func TestAssessAlertQuality_FlagsNoRunbookAndNoForDuration(t *testing.T) {
	t.Parallel()

	rulesJSON := `{"Demo App": [{"name": "g", "rules": [{"alert": "DiskFull", "expr": "disk < 10", "annotations": {}}]}]}`
	alertsJSON := `[{"labels": {"alertname": "DiskFull"}, "state": "active"}]`
	server := assessAlertQualityMock(t, rulesJSON, alertsJSON)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.assessAlertQuality(context.Background(), `{"alertname":"DiskFull"}`)
	if err != nil {
		t.Fatalf("assessAlertQuality returned error: %v", err)
	}

	var parsed alertQualityAssessment
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.HasRunbookLink {
		t.Error("expected HasRunbookLink = false")
	}
	if parsed.HasForDuration {
		t.Error("expected HasForDuration = false")
	}
	if parsed.CurrentlyFiring != 1 {
		t.Errorf("currentlyFiringInstances = %d, want 1", parsed.CurrentlyFiring)
	}
	if len(parsed.Concerns) < 2 {
		t.Errorf("expected at least 2 concerns (no runbook, no for-duration), got %+v", parsed.Concerns)
	}
}

func TestAssessAlertQuality_WellConfiguredRuleHasNoConcerns(t *testing.T) {
	t.Parallel()

	rulesJSON := `{"Demo App": [{"name": "g", "rules": [{"alert": "DiskFull", "expr": "disk < 10", "for": "5m", "annotations": {"runbook_url": "https://wiki.example.com/r"}}]}]}`
	server := assessAlertQualityMock(t, rulesJSON, `[]`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.assessAlertQuality(context.Background(), `{"alertname":"DiskFull"}`)
	if err != nil {
		t.Fatalf("assessAlertQuality returned error: %v", err)
	}

	var parsed alertQualityAssessment
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if !parsed.HasRunbookLink || !parsed.HasForDuration {
		t.Errorf("expected a well-configured rule to have both, got %+v", parsed)
	}
	if len(parsed.Concerns) != 1 || parsed.Concerns[0] != "no configuration-level concerns found" {
		t.Errorf("expected no real concerns, got %+v", parsed.Concerns)
	}
}

func TestAssessAlertQuality_RequiresRuleUIDOrAlertName(t *testing.T) {
	t.Parallel()

	te := NewToolExecutor("http://unused", log.DefaultLogger)
	if _, err := te.assessAlertQuality(context.Background(), `{}`); err == nil {
		t.Error("expected an error when neither rule_uid nor alertname is given")
	}
}

func TestAssessAlertQuality_NoMatchingRuleReturnsGracefulMessage(t *testing.T) {
	t.Parallel()

	server := assessAlertQualityMock(t, `{"Demo App": [{"name": "g", "rules": [{"alert": "Other"}]}]}`, `[]`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.assessAlertQuality(context.Background(), `{"alertname":"DoesNotExist"}`)
	if err != nil {
		t.Fatalf("assessAlertQuality returned error: %v", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed["message"] == "" {
		t.Errorf("expected a graceful no-match message, got %q", result)
	}
}
