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

// BuildChangeTimelineArgs holds parsed arguments for build_change_timeline.
type BuildChangeTimelineArgs struct {
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// Tags optionally filters to annotations carrying at least one of
	// these tags (e.g. "deploy", "config-change") -- Grafana's own
	// annotation API supports this filter natively.
	Tags  []string `json:"tags,omitempty"`
	Limit int      `json:"limit,omitempty"`
}

type timelineEvent struct {
	Time string   `json:"time"`
	Text string   `json:"text"`
	Tags []string `json:"tags,omitempty"`
}

type changeTimelineResult struct {
	ToolResult
	Start  string          `json:"start"`
	End    string          `json:"end"`
	Events []timelineEvent `json:"events"`
	Note   string          `json:"note"`
}

// changeTimelineMaxEvents caps how many annotations are returned -- an
// unfiltered window on a busy instance could return thousands, most of
// them irrelevant to any one investigation.
const changeTimelineMaxEvents = 200

// buildChangeTimeline lists Grafana annotations (deploys, config changes,
// manual incident markers -- anything already recorded via the Annotations
// API) in chronological order for a time window. Deliberately scoped to
// only this source: a build-info Prometheus metric or CI/CD system
// integration would add real value here too, but neither is something this
// plugin can assume exists in every deployment, unlike the Annotations API
// (a stable part of Grafana core).
func (te *ToolExecutor) buildChangeTimeline(ctx context.Context, arguments string) (string, error) {
	var args BuildChangeTimelineArgs
	if arguments != "" && arguments != "{}" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("parse build_change_timeline args: %w", err)
		}
	}

	now := time.Now()
	end := now
	if args.End != "" {
		if t, ok := resolveTimeValue(args.End, now); ok {
			end = t
		}
	}
	start := end.Add(-24 * time.Hour)
	if args.Start != "" {
		if t, ok := resolveTimeValue(args.Start, now); ok {
			start = t
		}
	}

	params := url.Values{}
	params.Set("from", fmt.Sprintf("%d", start.UnixMilli()))
	params.Set("to", fmt.Sprintf("%d", end.UnixMilli()))
	for _, tag := range args.Tags {
		params.Add("tags", tag)
	}
	limit := args.Limit
	if limit <= 0 || limit > changeTimelineMaxEvents {
		limit = changeTimelineMaxEvents
	}
	params.Set("limit", fmt.Sprintf("%d", limit))

	body, err := te.doGrafanaRequest(ctx, http.MethodGet, "/api/annotations?"+params.Encode(), nil)
	if err != nil {
		return "", err
	}
	var raw []struct {
		Time int64    `json:"time"`
		Text string   `json:"text"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return "", fmt.Errorf("parse annotations: %w", err)
	}

	events := make([]timelineEvent, 0, len(raw))
	for _, a := range raw {
		events = append(events, timelineEvent{
			Time: time.UnixMilli(a.Time).UTC().Format(time.RFC3339),
			Text: a.Text,
			Tags: a.Tags,
		})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time < events[j].Time })

	result := changeTimelineResult{
		Start:  start.UTC().Format(time.RFC3339),
		End:    end.UTC().Format(time.RFC3339),
		Events: events,
		Note:   "Sourced from Grafana's Annotations API only (deploys/changes/incidents that were actually recorded as annotations) -- does not include build-info metrics or external CI/CD systems.",
	}
	result.IsPartial = true
	result.Warnings = append(result.Warnings, "only Grafana annotations are included -- no build-info metric or external CI/CD source")
	result.Summary = fmt.Sprintf("%d annotation event(s) between %s and %s", len(events), result.Start, result.End)
	result.Sources = []string{"grafana annotations API (/api/annotations)"}
	result.TimeRange = result.Start + ".." + result.End

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
