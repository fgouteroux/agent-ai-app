package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// InvestigateIncidentArgs holds parsed arguments for investigate_incident.
type InvestigateIncidentArgs struct {
	// Seed is any starting point for the investigation -- an alert name,
	// a service/pod/job name, or a dashboard title. Unlike investigate_alert
	// (which requires an exact, currently-firing alertname), this is
	// resolved best-effort: an exact/near alert match if one exists, else a
	// Loki label whose real values contain it.
	Seed string `json:"seed"`
	// MetricQuery is an optional PromQL expression to also run through
	// analyze_metric_anomaly for this incident's window. Left empty when
	// omitted -- there's no reliable way to derive a meaningful PromQL
	// query from a free-text seed alone, so this only runs when the caller
	// (the model, usually after already looking at a dashboard) supplies
	// a real query.
	MetricQuery string `json:"metric_query,omitempty"`
	Project     string `json:"project,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
}

// incidentInvestigation is the evidence gathered for investigate_incident --
// same non-goal as alertInvestigation: never computes a root cause itself,
// only assembles resumed evidence (log patterns, metric anomalies, traces,
// historical correlation) for the model to reason about.
type investigateIncidentResult struct {
	ToolResult
	Seed           string                  `json:"seed"`
	Matches        int                     `json:"matches"`
	Investigations []incidentInvestigation `json:"investigations"`
}

type incidentInvestigation struct {
	ToolResult
	Seed string `json:"seed"`
	// MatchedAs describes how the free-text seed was matched -- e.g. "an
	// ACTIVE (currently firing) alert" or "Loki label x=y" -- NEVER whether
	// the underlying alert/incident itself is over. Deliberately avoids the
	// word "resolved" anywhere in its values: a live incident (2026-08-11)
	// had this field's predecessor read "resolved as active alert", and a
	// model synthesizing the final answer misread that as "the alert has
	// been resolved" (the opposite of true -- the alert was firing) instead
	// of "the seed was matched/interpreted as an active alert".
	MatchedAs             string `json:"matchedAs"`
	Alert                 any    `json:"alert,omitempty"`
	WindowStart           string `json:"windowStart"`
	WindowEnd             string `json:"windowEnd"`
	LogPatternsQuery      string `json:"logPatternsQuery,omitempty"`
	LogPatterns           string `json:"logPatterns,omitempty"`
	LogPatternsError      string `json:"logPatternsError,omitempty"`
	LogPatternsSkipped    string `json:"logPatternsSkippedReason,omitempty"`
	TracesQuery           string `json:"tracesQuery,omitempty"`
	Traces                string `json:"traces,omitempty"`
	TracesError           string `json:"tracesError,omitempty"`
	TracesSkippedReason   string `json:"tracesSkippedReason,omitempty"`
	MetricAnomaly         string `json:"metricAnomaly,omitempty"`
	MetricAnomalyError    string `json:"metricAnomalyError,omitempty"`
	HistoricalCorrelation string `json:"historicalCorrelation,omitempty"`
}

// investigateIncidentLokiLabelCandidates are the label keys checked, in
// order, when seed doesn't match any active alert -- the same keys
// buildLogSelector/buildTraceSelector already treat as service-identifying.
var investigateIncidentLokiLabelCandidates = []string{"namespace", "pod", "job", "service", "app"}

// resolveSeedToLokiLabel checks each candidate Loki label's real values
// (via listLokiLabels, already-existing discovery, never guessed) for one
// containing seed case-insensitively -- returns the label key and matched
// value, or "" if none matched. This is deliberately a real lookup against
// what Loki actually has, not a blind guess at the label scheme.
func (te *ToolExecutor) resolveSeedToLokiLabel(ctx context.Context, seed string) (labelKey, labelValue string) {
	lowerSeed := strings.ToLower(seed)
	for _, key := range investigateIncidentLokiLabelCandidates {
		argsJSON, _ := json.Marshal(map[string]string{"label": key})
		result, err := te.listLokiLabels(ctx, string(argsJSON))
		if err != nil {
			continue
		}
		var parsed struct {
			Data []string `json:"data"`
		}
		if json.Unmarshal([]byte(result), &parsed) != nil {
			continue
		}
		for _, v := range parsed.Data {
			if strings.Contains(strings.ToLower(v), lowerSeed) {
				return key, v
			}
		}
	}
	return "", ""
}

// investigateIncident generalizes investigateAlert to start from any seed
// (alert name, service/pod/job name, or anything else a user might
// describe an incident by), not just an exact, currently-firing alertname.
// Reuses analyze_log_patterns and analyze_metric_anomaly internally instead
// of dumping raw evidence, and the same alertname-resolution/correlation
// logic investigateAlert already has when the seed does match a real
// alert.
func (te *ToolExecutor) investigateIncident(ctx context.Context, arguments string) (string, error) {
	var args InvestigateIncidentArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse investigate_incident args: %w", err)
	}
	if strings.TrimSpace(args.Seed) == "" {
		return "", fmt.Errorf("seed is required")
	}

	body, err := te.doGrafanaRequest(ctx, http.MethodGet, "/api/alertmanager/grafana/api/v2/alerts", nil)
	if err != nil {
		return "", err
	}
	var alerts []map[string]any
	if err := json.Unmarshal([]byte(body), &alerts); err != nil {
		return "", fmt.Errorf("parse alerts JSON: %w", err)
	}

	var alertMatches []map[string]any
	for _, alert := range alerts {
		state, _ := alert["state"].(string)
		if state == "" {
			if status, ok := alert["status"].(map[string]any); ok {
				state, _ = status["state"].(string)
			}
		}
		if state != "active" {
			continue
		}
		name, labels, _ := extractAlertIdentity(alert)
		if !strings.EqualFold(name, args.Seed) {
			continue
		}
		if args.Namespace != "" && labels["namespace"] != args.Namespace {
			continue
		}
		alertMatches = append(alertMatches, alert)
		if len(alertMatches) >= alertInvestigationMaxMatches {
			break
		}
	}

	now := time.Now()
	var investigations []incidentInvestigation

	if len(alertMatches) > 0 {
		for _, alert := range alertMatches {
			_, labels, keywords := extractAlertIdentity(alert)

			windowStart, windowEnd := now.Add(-15*time.Minute), now
			if startsAtStr, ok := alert["startsAt"].(string); ok && startsAtStr != "" {
				if startsAt, err := time.Parse(time.RFC3339, startsAtStr); err == nil {
					windowStart, windowEnd = startsAt.Add(-5*time.Minute), now
				}
			}

			inv := incidentInvestigation{
				Seed:        args.Seed,
				MatchedAs:   "an ACTIVE, currently-firing alert",
				Alert:       alert,
				WindowStart: windowStart.UTC().Format(time.RFC3339),
				WindowEnd:   windowEnd.UTC().Format(time.RFC3339),
			}
			te.gatherIncidentEvidence(ctx, &inv, buildLogSelector(labels), buildTraceSelector(labels), windowStart, windowEnd, args.MetricQuery, args.Project, keywords)
			investigations = append(investigations, inv)
		}
	} else {
		// No active alert matches this seed -- try resolving it against a
		// real Loki label's known values instead of guessing a query likely
		// to return nothing.
		windowStart, windowEnd := now.Add(-1*time.Hour), now
		inv := incidentInvestigation{
			Seed:        args.Seed,
			WindowStart: windowStart.UTC().Format(time.RFC3339),
			WindowEnd:   windowEnd.UTC().Format(time.RFC3339),
		}

		labelKey, labelValue := te.resolveSeedToLokiLabel(ctx, args.Seed)
		var logSelector, traceSelector string
		if labelKey != "" {
			inv.MatchedAs = fmt.Sprintf("Loki label %s=%q (no active alert by this name -- this says nothing about whether an alert is firing)", labelKey, labelValue)
			logSelector = fmt.Sprintf(`{%s=%q} |~ "(?i)error|exception|fail|panic|timeout"`, labelKey, labelValue)
			traceSelector = fmt.Sprintf(`{resource.service.name=%q}`, labelValue)
		} else {
			inv.MatchedAs = "no match found -- no active alert and no known Loki label value contain this seed"
			// Still worth trying a best-effort trace selector -- TraceQL
			// regex matching is more forgiving than a Loki stream selector,
			// so this isn't as blind a guess as it would be for logs.
			traceSelector = fmt.Sprintf(`{resource.service.name=~%q}`, "(?i).*"+args.Seed+".*")
		}
		te.gatherIncidentEvidence(ctx, &inv, logSelector, traceSelector, windowStart, windowEnd, args.MetricQuery, args.Project, []string{args.Seed})
		investigations = append(investigations, inv)
	}

	result := investigateIncidentResult{Seed: args.Seed, Matches: len(investigations), Investigations: investigations}
	if len(investigations) == 1 && investigations[0].IsPartial {
		result.IsPartial = true
		result.Warnings = investigations[0].Warnings
	}
	if len(investigations) == 0 {
		result.Summary = fmt.Sprintf("no active alert or Loki label matched seed %q", args.Seed)
	} else {
		result.Summary = fmt.Sprintf("%d investigation(s) gathered for seed %q", len(investigations), args.Seed)
	}
	out, _ := json.Marshal(result)
	return truncateString(string(out), 50000), nil
}

// gatherIncidentEvidence runs the log-pattern, trace, metric-anomaly, and
// brain-agent lookups for one incidentInvestigation -- shared between the
// alert-matched and free-text-seed paths in investigateIncident. A failure
// in any one source degrades only that section, never aborts the rest --
// same non-negotiable as investigateAlert.
func (te *ToolExecutor) gatherIncidentEvidence(ctx context.Context, inv *incidentInvestigation, logSelector, traceSelector string, windowStart, windowEnd time.Time, metricQuery, project string, keywords []string) {
	inv.TimeRange = windowStart.UTC().Format(time.RFC3339) + ".." + windowEnd.UTC().Format(time.RFC3339)
	var sources []string

	if logSelector == "" {
		inv.LogPatternsSkipped = "no usable label to build a log query from"
		inv.IsPartial = true
		inv.Warnings = append(inv.Warnings, inv.LogPatternsSkipped)
	} else {
		inv.LogPatternsQuery = logSelector
		logArgs, _ := json.Marshal(AnalyzeLogPatternsArgs{
			Selector: logSelector,
			Start:    fmt.Sprintf("%d", windowStart.UnixNano()),
			End:      fmt.Sprintf("%d", windowEnd.UnixNano()),
		})
		if result, err := te.analyzeLogPatterns(ctx, string(logArgs)); err != nil {
			inv.LogPatternsError = err.Error()
			inv.IsPartial = true
			inv.Warnings = append(inv.Warnings, "log patterns: "+err.Error())
		} else {
			inv.LogPatterns = truncateString(result, 10000)
			sources = append(sources, "analyze_log_patterns")
		}
	}

	if traceSelector == "" {
		inv.TracesSkippedReason = "no usable service label to build a trace query from"
		inv.IsPartial = true
		inv.Warnings = append(inv.Warnings, inv.TracesSkippedReason)
	} else {
		inv.TracesQuery = traceSelector
		traceArgs, _ := json.Marshal(TempoQueryArgs{
			Query: traceSelector,
			Start: fmt.Sprintf("%d", windowStart.Unix()),
			End:   fmt.Sprintf("%d", windowEnd.Unix()),
			Limit: 5,
		})
		if result, err := te.queryTempo(ctx, string(traceArgs)); err != nil {
			inv.TracesError = err.Error()
			inv.IsPartial = true
			inv.Warnings = append(inv.Warnings, "traces: "+err.Error())
		} else {
			inv.Traces = truncateString(result, 10000)
			sources = append(sources, "query_tempo")
		}
	}

	if metricQuery != "" {
		anomalyArgs, _ := json.Marshal(AnalyzeMetricAnomalyArgs{
			Query: metricQuery,
			Start: fmt.Sprintf("%d", windowStart.Unix()),
			End:   fmt.Sprintf("%d", windowEnd.Unix()),
		})
		if result, err := te.analyzeMetricAnomaly(ctx, string(anomalyArgs)); err != nil {
			inv.MetricAnomalyError = err.Error()
			inv.IsPartial = true
			inv.Warnings = append(inv.Warnings, "metric anomaly: "+err.Error())
		} else {
			inv.MetricAnomaly = truncateString(result, 10000)
			sources = append(sources, "analyze_metric_anomaly")
		}
	}

	if te.mcp != nil && len(keywords) > 0 {
		searchArgs := map[string]string{"query": strings.Join(keywords, " ")}
		if project != "" {
			searchArgs["project"] = project
		}
		searchJSON, _ := json.Marshal(searchArgs)
		if memResult, err := te.mcp.Call(ctx, "search_memory", string(searchJSON)); err == nil &&
			memResult != "" && !strings.Contains(memResult, "currently empty") && !strings.Contains(memResult, "No matches found") {
			inv.HistoricalCorrelation = memResult
			sources = append(sources, "brain-agent search_memory")
		}
	}
	inv.Sources = sources
	inv.Summary = fmt.Sprintf("seed matched as: %s. Evidence gathered from: %s. This tool never determines whether an incident is over -- absence of matching log/trace errors in this window means nothing was found in those specific sources, NOT that the alert is resolved; check the alert's own status field for that.", inv.MatchedAs, strings.Join(sources, ", "))
}
