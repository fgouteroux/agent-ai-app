package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// InspectKubernetesEventsArgs holds parsed arguments for
// inspect_kubernetes_events.
type InspectKubernetesEventsArgs struct {
	// Selector is a LogQL stream selector for wherever this Grafana
	// instance's Kubernetes events are actually shipped to Loki (e.g. via
	// promtail/alloy watching the K8s events API) -- there is no single
	// standard label scheme across clusters, so this is required rather
	// than guessed. Use list_loki_labels first if unsure.
	Selector string `json:"selector"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	// DatasourceUID targets a specific Loki datasource when more than one
	// exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type kubernetesEventsResult struct {
	ToolResult
	Selector       string           `json:"selector"`
	LinesScanned   int              `json:"lines_scanned"`
	StructuredJSON bool             `json:"structured_json"`
	GroupCount     int              `json:"group_count"`
	Events         []*k8sEventGroup `json:"events"`
	Truncated      bool             `json:"truncated"`
	Note           string           `json:"note"`
}

type k8sEventGroup struct {
	Key       string `json:"key"`
	Count     int    `json:"count"`
	Example   string `json:"example"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

// k8sEventJSON is the standard shape a Kubernetes Event object serializes
// to -- checked defensively (this plugin has no live Loki instance
// shipping real K8s events to verify against), falling back to plain
// pattern-normalization (same as analyze_log_patterns) for any line that
// doesn't parse this way.
type k8sEventJSON struct {
	Reason         string `json:"reason"`
	Type           string `json:"type"`
	Message        string `json:"message"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
}

// inspectKubernetesEventsMaxGroups caps how many distinct
// involvedObject+reason groups (or, on unstructured logs, normalized
// patterns) are returned -- top-N by occurrence count, per the roadmap.
const inspectKubernetesEventsMaxGroups = 10

// inspectKubernetesEvents groups Kubernetes events (already shipped to
// Loki by whatever means this instance uses -- promtail/alloy watching the
// events API, deliberately NOT a direct Kubernetes API call: that would
// need a new authentication model this plugin's Viewer-role-only service
// account design doesn't have) by involvedObject+reason when they parse as
// real structured K8s Event JSON, or by normalized text pattern otherwise
// (reusing analyze_log_patterns' own normalizer) -- either way, deduplicated
// and capped at the top occurrences instead of a raw dump.
func (te *ToolExecutor) inspectKubernetesEvents(ctx context.Context, arguments string) (string, error) {
	var args InspectKubernetesEventsArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse inspect_kubernetes_events args: %w", err)
	}
	if args.Selector == "" {
		return "", fmt.Errorf("selector is required -- use list_loki_labels first if you don't know how Kubernetes events are shipped to Loki in this environment")
	}

	dsUID, err := te.resolveDatasourceUID(ctx, "loki", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find loki datasource: %w", err)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}

	params := url.Values{}
	params.Set("query", args.Selector)
	now := time.Now()
	if args.Start == "" {
		params.Set("start", fmt.Sprintf("%d", now.Add(-1*time.Hour).UnixNano()))
	} else {
		params.Set("start", resolveTimeNano(args.Start, now))
	}
	if args.End == "" {
		params.Set("end", fmt.Sprintf("%d", now.UnixNano()))
	} else {
		params.Set("end", resolveTimeNano(args.End, now))
	}
	params.Set("limit", fmt.Sprintf("%d", limit))

	apiPath := fmt.Sprintf("/api/datasources/proxy/uid/%s/loki/api/v1/query_range?%s", url.PathEscape(dsUID), params.Encode())
	body, err := te.doGrafanaRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", err
	}
	var resp lokiQueryRangeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("parse loki response: %w", err)
	}

	groups := make(map[string]*k8sEventGroup)
	var order []string
	structuredCount, totalLines := 0, 0
	for _, stream := range resp.Data.Result {
		for _, v := range stream.Values {
			totalLines++
			tsNano, line := v[0], v[1]

			var key, example string
			var event k8sEventJSON
			if json.Unmarshal([]byte(line), &event) == nil && event.Reason != "" {
				structuredCount++
				key = fmt.Sprintf("%s: %s/%s", event.Reason, event.InvolvedObject.Kind, event.InvolvedObject.Name)
				example = line
			} else {
				key = normalizeLogLine(line)
				example = line
			}

			g, ok := groups[key]
			if !ok {
				g = &k8sEventGroup{Key: key, Example: example, FirstSeen: tsNano, LastSeen: tsNano}
				groups[key] = g
				order = append(order, key)
			}
			g.Count++
			if nanoLess(tsNano, g.FirstSeen) {
				g.FirstSeen = tsNano
			}
			if nanoLess(g.LastSeen, tsNano) {
				g.LastSeen = tsNano
			}
		}
	}

	events := make([]*k8sEventGroup, 0, len(order))
	for _, key := range order {
		events = append(events, groups[key])
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Count > events[j].Count })

	truncated := false
	if len(events) > inspectKubernetesEventsMaxGroups {
		events = events[:inspectKubernetesEventsMaxGroups]
		truncated = true
	}

	result := kubernetesEventsResult{
		Selector:       args.Selector,
		LinesScanned:   totalLines,
		StructuredJSON: structuredCount > 0,
		GroupCount:     len(order),
		Events:         events,
		Truncated:      truncated,
		Note:           "Sourced from Loki only (wherever this instance's Kubernetes events are shipped) -- not a direct Kubernetes API call. Grouped by involvedObject+reason when events parse as structured JSON, otherwise by normalized text pattern.",
	}
	if truncated {
		result.IsPartial = true
		result.Warnings = append(result.Warnings, fmt.Sprintf("only the top %d of %d groups are shown", inspectKubernetesEventsMaxGroups, len(order)))
	}
	if totalLines == 0 {
		result.Summary = fmt.Sprintf("no events matched selector %q in this window", args.Selector)
	} else {
		result.Summary = fmt.Sprintf("%d lines scanned, %d distinct event group(s)", totalLines, len(order))
	}
	result.Sources = []string{datasourceSource("loki", args.DatasourceUID)}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
