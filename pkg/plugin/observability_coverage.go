package plugin

import (
	"context"
	"encoding/json"
	"fmt"
)

// CheckObservabilityCoverageArgs holds parsed arguments for
// check_observability_coverage.
type CheckObservabilityCoverageArgs struct {
	ServiceName string `json:"service_name"`
}

type coverageCheck struct {
	Found  bool   `json:"found"`
	Detail string `json:"detail"`
}

type observabilityCoverageResult struct {
	ToolResult
	ServiceName string        `json:"service_name"`
	Logs        coverageCheck `json:"logs"`
	Metrics     coverageCheck `json:"metrics"`
	Dashboards  coverageCheck `json:"dashboards"`
	Traces      coverageCheck `json:"traces"`
}

// checkObservabilityCoverage checks whether a service has real logs,
// metrics, dashboards, and traces -- cheap, existing queries reused (the
// same Loki label lookup investigate_incident uses, a lightweight
// Prometheus `up` check, find_dashboards, and a Tempo service-name search),
// not new infrastructure. Traces is checked for real once a Tempo
// datasource actually exists; when none is configured, it's reported as
// genuinely unchecked (with an explicit reason) rather than silently
// skipped or -- worse -- falsely claimed as a checked negative result, a
// lie the model couldn't otherwise tell apart from a real "no traces
// found".
func (te *ToolExecutor) checkObservabilityCoverage(ctx context.Context, arguments string) (string, error) {
	var args CheckObservabilityCoverageArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse check_observability_coverage args: %w", err)
	}
	if args.ServiceName == "" {
		return "", fmt.Errorf("service_name is required")
	}

	result := observabilityCoverageResult{ServiceName: args.ServiceName}

	labelKey, labelValue := te.resolveSeedToLokiLabel(ctx, args.ServiceName)
	if labelKey != "" {
		result.Logs = coverageCheck{Found: true, Detail: fmt.Sprintf("matched Loki label %s=%q", labelKey, labelValue)}
	} else {
		result.Logs = coverageCheck{Found: false, Detail: "no Loki label value matched this service name"}
	}

	metricsQuery := fmt.Sprintf(`count(up{job=~"(?i).*%s.*"})`, args.ServiceName)
	if promResult, err := te.queryPrometheus(ctx, mustMarshal(PrometheusQueryArgs{Query: metricsQuery})); err == nil && promResultHasSeries(promResult) {
		result.Metrics = coverageCheck{Found: true, Detail: "found a Prometheus \"up\" series with a matching job label"}
	} else {
		result.Metrics = coverageCheck{Found: false, Detail: "no Prometheus \"up\" series found for this service (checked the job label only -- a different metric naming scheme may still exist)"}
	}

	dashResult, err := te.findDashboards(ctx, mustMarshal(struct {
		Topic string `json:"topic"`
	}{Topic: args.ServiceName}))
	if err == nil {
		var dashboards []map[string]any
		if json.Unmarshal([]byte(dashResult), &dashboards) == nil && len(dashboards) > 0 {
			result.Dashboards = coverageCheck{Found: true, Detail: fmt.Sprintf("%d matching dashboard(s) found", len(dashboards))}
		} else {
			result.Dashboards = coverageCheck{Found: false, Detail: "no dashboards matched this service name"}
		}
	} else {
		result.Dashboards = coverageCheck{Found: false, Detail: "dashboard search failed: " + err.Error()}
	}

	tracesChecked := true
	if _, dsErr := te.resolveDatasourceUID(ctx, "tempo", ""); dsErr != nil {
		tracesChecked = false
		result.Traces = coverageCheck{Found: false, Detail: "NOT CHECKED -- no Tempo datasource is available in this environment; this is not a real negative result, only an unchecked gap"}
		result.IsPartial = true
		result.Warnings = append(result.Warnings, "traces coverage was not checked -- no Tempo datasource available")
	} else {
		traceResult, err := te.queryTempo(ctx, mustMarshal(TempoQueryArgs{
			Query: fmt.Sprintf(`{resource.service.name=%q}`, args.ServiceName),
			Limit: 1,
		}))
		if err == nil && tempoSearchHasResults(traceResult) {
			result.Traces = coverageCheck{Found: true, Detail: "found trace(s) with a matching resource.service.name"}
		} else {
			result.Traces = coverageCheck{Found: false, Detail: "no traces found with a matching resource.service.name"}
		}
	}

	found, total := 0, 3
	checks := []coverageCheck{result.Logs, result.Metrics, result.Dashboards}
	if tracesChecked {
		total = 4
		checks = append(checks, result.Traces)
	}
	for _, c := range checks {
		if c.Found {
			found++
		}
	}
	if tracesChecked {
		result.Summary = fmt.Sprintf("%s: %d/%d of logs/metrics/dashboards/traces covered", args.ServiceName, found, total)
	} else {
		result.Summary = fmt.Sprintf("%s: %d/%d of logs/metrics/dashboards covered (traces not checked)", args.ServiceName, found, total)
	}
	result.Sources = []string{"loki labels", "prometheus (up{job=~...})", "grafana dashboard search"}
	if tracesChecked {
		result.Sources = append(result.Sources, "tempo (resource.service.name search)")
	}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}

// promResultHasSeries reports whether a query_prometheus JSON result
// contains at least one non-empty result series.
func promResultHasSeries(body string) bool {
	var parsed struct {
		Data struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &parsed) != nil {
		return false
	}
	return len(parsed.Data.Result) > 0
}

// tempoSearchHasResults reports whether a query_tempo search JSON result
// (see tempoSearchResponse) found at least one trace.
func tempoSearchHasResults(body string) bool {
	var parsed tempoSearchResponse
	if json.Unmarshal([]byte(body), &parsed) != nil {
		return false
	}
	return len(parsed.Traces) > 0
}
