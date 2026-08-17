package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// AssessAlertQualityArgs holds parsed arguments for assess_alert_quality.
type AssessAlertQualityArgs struct {
	RuleUID   string `json:"rule_uid,omitempty"`
	AlertName string `json:"alertname,omitempty"`
}

type alertQualityAssessment struct {
	ToolResult
	Name            string   `json:"name"`
	HasRunbookLink  bool     `json:"hasRunbookLink"`
	RunbookURL      string   `json:"runbookUrl,omitempty"`
	HasForDuration  bool     `json:"hasForDuration"`
	CurrentlyFiring int      `json:"currentlyFiringInstances"`
	Concerns        []string `json:"concerns"`
	Scope           string   `json:"scope"`
}

// assessAlertQuality reports quality signals for ONE alert rule that are
// actually verifiable from this plugin's real API access: whether it has a
// runbook link, whether it has a "for" duration configured (a rule with
// none fires the instant its condition is true even once), and how many
// instances are firing right now. Deliberately does NOT claim to detect
// flapping/noise frequency over time -- that needs an alert state-history
// API this plugin has no verified access to, and guessing at an unverified
// endpoint risks silently returning wrong data dressed up as a real
// assessment.
func (te *ToolExecutor) assessAlertQuality(ctx context.Context, arguments string) (string, error) {
	var args AssessAlertQualityArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse assess_alert_quality args: %w", err)
	}
	if args.RuleUID == "" && args.AlertName == "" {
		return "", fmt.Errorf("rule_uid or alertname is required")
	}

	rulesBody, err := te.doGrafanaRequest(ctx, http.MethodGet, "/api/ruler/grafana/api/v1/rules", nil)
	if err != nil {
		return "", err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(rulesBody), &raw); err != nil {
		return "", fmt.Errorf("parse ruler rules: %w", err)
	}
	rule := walkRulerRules(raw, args.RuleUID, args.AlertName)
	if rule == nil {
		want := args.RuleUID
		if want == "" {
			want = args.AlertName
		}
		return fmt.Sprintf(`{"message": %q}`, fmt.Sprintf(
			"No alert rule matching %q found -- call list_alert_rules first to see the exact names/UIDs configured.", want)), nil
	}

	alertsBody, err := te.doGrafanaRequest(ctx, http.MethodGet, "/api/alertmanager/grafana/api/v2/alerts", nil)
	firingCount := 0
	if err == nil {
		var alerts []map[string]any
		if json.Unmarshal([]byte(alertsBody), &alerts) == nil {
			for _, alert := range alerts {
				name, _, _ := extractAlertIdentity(alert)
				if !strings.EqualFold(name, rule.Name) {
					continue
				}
				state, _ := alert["state"].(string)
				if state == "" {
					if status, ok := alert["status"].(map[string]any); ok {
						state, _ = status["state"].(string)
					}
				}
				if state == "active" {
					firingCount++
				}
			}
		}
	}

	assessment := alertQualityAssessment{
		Name:            rule.Name,
		HasRunbookLink:  rule.HasRunbookLink,
		RunbookURL:      rule.RunbookURL,
		HasForDuration:  rule.HasForDuration,
		CurrentlyFiring: firingCount,
		Scope:           "Based on configuration (runbook link, for-duration) and current firing state only -- does NOT measure historical flapping/resolve frequency, which needs an alert state-history source this plugin doesn't have verified access to.",
	}

	if !rule.HasRunbookLink {
		assessment.Concerns = append(assessment.Concerns, "no runbook link in this rule's annotations -- whoever responds has no documented procedure to follow")
	}
	if !rule.HasForDuration {
		assessment.Concerns = append(assessment.Concerns, `no "for" duration configured -- fires the instant the condition is true even once, which can make it noisy on a metric that briefly crosses the threshold`)
	}
	if firingCount > 10 {
		assessment.Concerns = append(assessment.Concerns, fmt.Sprintf("%d instances currently firing simultaneously -- worth confirming this is one broad real incident and not an overly-broad rule matching too much", firingCount))
	}
	if len(assessment.Concerns) == 0 {
		assessment.Concerns = []string{"no configuration-level concerns found"}
	} else {
		assessment.Warnings = assessment.Concerns
	}
	assessment.Summary = fmt.Sprintf("%s: %d concern(s), %d currently firing", assessment.Name, len(assessment.Concerns), firingCount)
	assessment.Sources = []string{"grafana ruler API (/api/ruler/grafana/api/v1/rules)", "grafana alertmanager API (/api/alertmanager/grafana/api/v2/alerts)"}

	out, err := json.Marshal(assessment)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
