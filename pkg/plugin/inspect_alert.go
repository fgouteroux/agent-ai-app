package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// InspectAlertArgs holds parsed arguments for inspect_alert.
type InspectAlertArgs struct {
	// RuleUID, when known (e.g. from a prior list_alert_rules call), is
	// matched exactly. AlertName is a fallback/alternative -- matched
	// against the rule's alert name/title.
	RuleUID   string `json:"rule_uid,omitempty"`
	AlertName string `json:"alertname,omitempty"`
}

type inspectedRule struct {
	ToolResult
	Folder            string `json:"folder"`
	Group             string `json:"group"`
	UID               string `json:"uid,omitempty"`
	Name              string `json:"name"`
	Expression        string `json:"expression,omitempty"`
	For               string `json:"for,omitempty"`
	Labels            any    `json:"labels,omitempty"`
	Annotations       any    `json:"annotations,omitempty"`
	HasRunbookLink    bool   `json:"hasRunbookLink"`
	RunbookURL        string `json:"runbookUrl,omitempty"`
	HasForDuration    bool   `json:"hasForDuration"`
	NoQuietPeriodNote string `json:"note,omitempty"`
}

// walkRulerRules recursively searches Grafana's own Ruler API response
// (/api/ruler/grafana/api/v1/rules -- a folder-name-keyed map of rule
// groups, each containing rules whose own shape varies across Grafana
// versions between plain Prometheus-style fields and a nested
// "grafana_alert" object) for a rule matching ruleUID or alertName. Walked
// as generic any/map/slice rather than a rigid struct -- deliberately
// defensive against exact-schema assumptions this plugin has no live
// Grafana instance available to verify against right now.
func walkRulerRules(raw map[string]any, ruleUID, alertName string) *inspectedRule {
	lowerAlertName := strings.ToLower(alertName)
	for folder, groupsRaw := range raw {
		groups, ok := groupsRaw.([]any)
		if !ok {
			continue
		}
		for _, groupRaw := range groups {
			group, ok := groupRaw.(map[string]any)
			if !ok {
				continue
			}
			groupName, _ := group["name"].(string)
			rulesRaw, ok := group["rules"].([]any)
			if !ok {
				continue
			}
			for _, ruleRaw := range rulesRaw {
				rule, ok := ruleRaw.(map[string]any)
				if !ok {
					continue
				}
				uid, name := extractRuleIdentity(rule)
				if ruleUID != "" && uid != ruleUID {
					continue
				}
				if ruleUID == "" && !strings.EqualFold(name, alertName) && !strings.Contains(strings.ToLower(name), lowerAlertName) {
					continue
				}
				return buildInspectedRule(folder, groupName, uid, name, rule)
			}
		}
	}
	return nil
}

// extractRuleIdentity pulls a rule's UID and display name from either shape
// Grafana's Ruler API has used: a top-level "alert" (Prometheus-style) or a
// nested "grafana_alert" object with its own "uid"/"title".
func extractRuleIdentity(rule map[string]any) (uid, name string) {
	if ga, ok := rule["grafana_alert"].(map[string]any); ok {
		if u, ok := ga["uid"].(string); ok {
			uid = u
		}
		if t, ok := ga["title"].(string); ok {
			name = t
		}
	}
	if name == "" {
		if a, ok := rule["alert"].(string); ok {
			name = a
		}
	}
	if uid == "" {
		if u, ok := rule["uid"].(string); ok {
			uid = u
		}
	}
	return uid, name
}

func buildInspectedRule(folder, group, uid, name string, rule map[string]any) *inspectedRule {
	ir := &inspectedRule{Folder: folder, Group: group, UID: uid, Name: name}

	if expr, ok := rule["expr"].(string); ok {
		ir.Expression = expr
	}
	if forDur, ok := rule["for"].(string); ok && forDur != "" {
		ir.For = forDur
		ir.HasForDuration = true
	}
	ir.Labels = rule["labels"]
	ir.Annotations = rule["annotations"]

	annotationsJSON, _ := json.Marshal(rule["annotations"])
	lowerAnnotations := strings.ToLower(string(annotationsJSON))
	if strings.Contains(lowerAnnotations, "runbook") || strings.Contains(lowerAnnotations, "http://") || strings.Contains(lowerAnnotations, "https://") {
		ir.HasRunbookLink = true
		if annotations, ok := rule["annotations"].(map[string]any); ok {
			for k, v := range annotations {
				if strings.Contains(strings.ToLower(k), "runbook") {
					if s, ok := v.(string); ok {
						ir.RunbookURL = s
					}
				}
			}
		}
	}

	if !ir.HasForDuration {
		ir.NoQuietPeriodNote = `no "for" duration configured -- this rule fires the instant its condition is true even once, which can make it noisy/flappy on a metric that briefly crosses the threshold`
		ir.Warnings = append(ir.Warnings, ir.NoQuietPeriodNote)
	}
	if !ir.HasRunbookLink {
		ir.Warnings = append(ir.Warnings, "no runbook link found in this rule's annotations")
	}
	ir.Summary = fmt.Sprintf("%s (folder=%s, group=%s): runbook=%v, for-duration=%v", ir.Name, ir.Folder, ir.Group, ir.HasRunbookLink, ir.HasForDuration)
	ir.Sources = []string{"grafana ruler API (/api/ruler/grafana/api/v1/rules)"}
	return ir
}

// inspectAlert fetches Grafana's own Ruler API and returns ONE rule's full
// definition (expression, labels, annotations, for-duration) plus a couple
// of cheap heuristic checks (a runbook link present in its annotations, a
// "for" duration configured) -- instead of list_alert_rules' unfiltered
// dump of every rule, which is unusable once an instance has more than a
// handful. Does not itself evaluate the expression against Prometheus --
// once the model has the real expression here, query_prometheus/
// analyze_metric_anomaly are the tools to check it live.
func (te *ToolExecutor) inspectAlert(ctx context.Context, arguments string) (string, error) {
	var args InspectAlertArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse inspect_alert args: %w", err)
	}
	if args.RuleUID == "" && args.AlertName == "" {
		return "", fmt.Errorf("rule_uid or alertname is required")
	}

	body, err := te.doGrafanaRequest(ctx, http.MethodGet, "/api/ruler/grafana/api/v1/rules", nil)
	if err != nil {
		return "", err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return "", fmt.Errorf("parse ruler rules: %w", err)
	}

	found := walkRulerRules(raw, args.RuleUID, args.AlertName)
	if found == nil {
		want := args.RuleUID
		if want == "" {
			want = args.AlertName
		}
		return fmt.Sprintf(`{"message": %q}`, fmt.Sprintf(
			"No alert rule matching %q found -- call list_alert_rules first to see the exact names/UIDs configured.", want)), nil
	}

	out, err := json.Marshal(found)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
