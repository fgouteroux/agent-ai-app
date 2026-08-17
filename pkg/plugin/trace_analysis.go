package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// tempoAttribute is one key/value pair from Tempo's OTLP-JSON span/resource
// attributes -- only the two value shapes this plugin's own seeded demo
// data and typical OTel SDKs actually use (string, int) are read; anything
// else (bool/double/array) is simply not extracted, never causing a parse
// error.
type tempoAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue,omitempty"`
		IntValue    string `json:"intValue,omitempty"`
	} `json:"value"`
}

// tempoSpan is the subset of Tempo's /api/traces/{id} span shape this
// plugin needs. traceId/spanId/parentSpanId are base64 (OTLP-JSON
// convention) -- compared as opaque strings, never decoded, since nothing
// here needs the raw bytes.
type tempoSpan struct {
	SpanID            string           `json:"spanId"`
	ParentSpanID      string           `json:"parentSpanId,omitempty"`
	Name              string           `json:"name"`
	StartTimeUnixNano string           `json:"startTimeUnixNano"`
	EndTimeUnixNano   string           `json:"endTimeUnixNano"`
	Attributes        []tempoAttribute `json:"attributes,omitempty"`
	Status            struct {
		Message string `json:"message,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"status"`
}

type tempoBatch struct {
	Resource struct {
		Attributes []tempoAttribute `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []struct {
		Spans []tempoSpan `json:"spans"`
	} `json:"scopeSpans"`
}

// tempoTraceResponse is Tempo's full /api/traces/{traceID} response shape.
type tempoTraceResponse struct {
	Batches []tempoBatch `json:"batches"`
}

// tempoSearchResponse is Tempo's /api/search response shape (see queryTempo).
type tempoSearchResponse struct {
	Traces []struct {
		TraceID         string `json:"traceID"`
		RootServiceName string `json:"rootServiceName"`
	} `json:"traces"`
}

// resourceServiceName extracts "service.name" from a batch's resource
// attributes -- "" if absent (defensive: not every span source is
// guaranteed to set it, though every real OTel SDK does).
func resourceServiceName(attrs []tempoAttribute) string {
	for _, a := range attrs {
		if a.Key == "service.name" {
			return a.Value.StringValue
		}
	}
	return ""
}

// flatSpan is one span with its owning batch's service name already
// resolved -- the shape every consumer in this file actually wants,
// instead of re-walking batches/scopeSpans/spans at every call site.
type flatSpan struct {
	ServiceName  string
	SpanID       string
	ParentSpanID string
	Name         string
	StartNano    int64
	EndNano      int64
	StatusError  bool
	StatusMsg    string
}

// flattenTempoSpans walks every batch/scopeSpan and returns one flatSpan per
// span, with its service name resolved and start/end parsed to int64 --
// spans with an unparseable timestamp are skipped (defensive; never seen
// from a real OTel SDK, but a single malformed span must not abort the
// whole trace's analysis).
func flattenTempoSpans(resp *tempoTraceResponse) []flatSpan {
	var out []flatSpan
	for _, batch := range resp.Batches {
		serviceName := resourceServiceName(batch.Resource.Attributes)
		for _, scopeSpan := range batch.ScopeSpans {
			for _, s := range scopeSpan.Spans {
				start, errStart := strconv.ParseInt(s.StartTimeUnixNano, 10, 64)
				end, errEnd := strconv.ParseInt(s.EndTimeUnixNano, 10, 64)
				if errStart != nil || errEnd != nil {
					continue
				}
				out = append(out, flatSpan{
					ServiceName:  serviceName,
					SpanID:       s.SpanID,
					ParentSpanID: s.ParentSpanID,
					Name:         s.Name,
					StartNano:    start,
					EndNano:      end,
					StatusError:  s.Status.Code == "STATUS_CODE_ERROR",
					StatusMsg:    s.Status.Message,
				})
			}
		}
	}
	return out
}

// AnalyzeTraceBottlenecksArgs holds parsed arguments for
// analyze_trace_bottlenecks.
type AnalyzeTraceBottlenecksArgs struct {
	TraceID string `json:"trace_id"`
	// MinPercentOfTrace is the share of the total trace duration a span
	// must account for to be flagged as a bottleneck. Defaults to 20 --
	// low enough to catch a genuinely dominant span, high enough that
	// ordinary nested spans in a healthy trace don't all get flagged.
	MinPercentOfTrace float64 `json:"min_percent_of_trace,omitempty"`
	// DatasourceUID targets a specific Tempo datasource when more than one
	// exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type bottleneckSpan struct {
	Service        string  `json:"service"`
	Name           string  `json:"name"`
	DurationMs     float64 `json:"duration_ms"`
	PercentOfTrace float64 `json:"percent_of_trace"`
}

type errorSpan struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

type traceBottlenecksResult struct {
	ToolResult
	TraceID         string           `json:"trace_id"`
	TotalDurationMs float64          `json:"total_duration_ms"`
	SpanCount       int              `json:"span_count"`
	Bottlenecks     []bottleneckSpan `json:"bottlenecks"`
	Errors          []errorSpan      `json:"errors"`
}

const defaultMinPercentOfTrace = 20.0

// analyzeTraceBottlenecks fetches one trace by ID and identifies which
// span(s) account for a disproportionate share of the total trace duration
// (the actual latency bottleneck), plus any span that came back with an
// error status -- instead of leaving the model to eyeball a raw trace JSON
// dump and manually compute percentages itself.
func (te *ToolExecutor) analyzeTraceBottlenecks(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeTraceBottlenecksArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_trace_bottlenecks args: %w", err)
	}
	if args.TraceID == "" {
		// See the identical comment on queryTempo's "query or traceID is
		// required" error (tool_executor.go) -- a live incident showed the
		// model punt to the user instead of self-correcting on this bare
		// message.
		return "", fmt.Errorf(`trace_id is required -- if you don't have one yet, call query_tempo with a TraceQL "query" selector (e.g. {resource.service.name="checkout"}) first to find real trace IDs, then retry with one of them`)
	}
	minPercent := args.MinPercentOfTrace
	if minPercent <= 0 {
		minPercent = defaultMinPercentOfTrace
	}

	body, err := te.queryTempo(ctx, mustMarshal(TempoQueryArgs{TraceID: args.TraceID, DatasourceUID: args.DatasourceUID}))
	if err != nil {
		return "", fmt.Errorf("fetch trace: %w", err)
	}

	var resp tempoTraceResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("parse trace response: %w", err)
	}
	spans := flattenTempoSpans(&resp)
	if len(spans) == 0 {
		return "", fmt.Errorf("trace %q has no spans (or was not found) -- check the trace ID", args.TraceID)
	}

	minStart, maxEnd := spans[0].StartNano, spans[0].EndNano
	hasChildren := make(map[string]bool, len(spans))
	for _, s := range spans {
		if s.StartNano < minStart {
			minStart = s.StartNano
		}
		if s.EndNano > maxEnd {
			maxEnd = s.EndNano
		}
		if s.ParentSpanID != "" {
			hasChildren[s.ParentSpanID] = true
		}
	}
	totalDurationNs := float64(maxEnd - minStart)

	var bottlenecks []bottleneckSpan
	var errs []errorSpan
	for _, s := range spans {
		durationMs := float64(s.EndNano-s.StartNano) / 1e6
		var percent float64
		if totalDurationNs > 0 {
			percent = float64(s.EndNano-s.StartNano) / totalDurationNs * 100
		}
		// Only leaf spans (no children of their own) are real bottleneck
		// candidates -- a parent span's duration trivially includes
		// everything its children did, so it always looks "dominant"
		// without itself being where the time was actually spent (the
		// same reasoning a flamegraph's self-time view uses, instead of
		// raw/inclusive duration).
		if percent >= minPercent && !hasChildren[s.SpanID] {
			bottlenecks = append(bottlenecks, bottleneckSpan{Service: s.ServiceName, Name: s.Name, DurationMs: durationMs, PercentOfTrace: percent})
		}
		if s.StatusError {
			errs = append(errs, errorSpan{Service: s.ServiceName, Name: s.Name, Message: s.StatusMsg})
		}
	}
	sort.Slice(bottlenecks, func(i, j int) bool { return bottlenecks[i].PercentOfTrace > bottlenecks[j].PercentOfTrace })

	result := traceBottlenecksResult{
		TraceID:         args.TraceID,
		TotalDurationMs: totalDurationNs / 1e6,
		SpanCount:       len(spans),
		Bottlenecks:     bottlenecks,
		Errors:          errs,
	}
	if len(bottlenecks) == 0 && len(errs) == 0 {
		result.Summary = fmt.Sprintf("no span accounts for >=%.0f%% of the trace's %.1fms total, and no span errored", minPercent, result.TotalDurationMs)
	} else {
		result.Summary = fmt.Sprintf("%d bottleneck span(s), %d error span(s) out of %d total (trace duration %.1fms)", len(bottlenecks), len(errs), len(spans), result.TotalDurationMs)
	}
	result.Sources = []string{datasourceSource("tempo", args.DatasourceUID)}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
