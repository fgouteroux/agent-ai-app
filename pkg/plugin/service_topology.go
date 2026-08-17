package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// BuildServiceTopologyArgs holds parsed arguments for build_service_topology.
type BuildServiceTopologyArgs struct {
	// ServiceName seeds the search -- recent traces where this service
	// appears (as root or otherwise) are fetched and their parent-child
	// span relationships assembled into a service call graph. Required:
	// there's no reasonable "all services" default that wouldn't require
	// scanning Tempo's entire trace volume.
	ServiceName string `json:"service_name"`
	// MaxTraces bounds how many recent traces are fetched and walked --
	// each is a full trace fetch (one Tempo API call), so this is also
	// this tool's real cost. Defaults to 10, capped at 25.
	MaxTraces int `json:"max_traces,omitempty"`
	// DatasourceUID targets a specific Tempo datasource when more than one
	// exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type topologyEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	CallCount int    `json:"call_count"`
}

type serviceTopologyResult struct {
	ToolResult
	ServiceName  string         `json:"service_name"`
	TracesWalked int            `json:"traces_walked"`
	Services     []string       `json:"services"`
	Edges        []topologyEdge `json:"edges"`
}

const (
	defaultTopologyMaxTraces = 10
	maxTopologyMaxTraces     = 25
)

// buildServiceTopology derives a service call graph (which service calls
// which, and how often) from real parent-child span relationships across a
// sample of recent traces involving serviceName -- rather than a single
// trace's necessarily-partial view, or a guessed/static architecture
// diagram. Each edge's call count is only over the sampled traces, not the
// service's total traffic -- reported as a relative signal (busiest edges
// in the sample), not an absolute rate.
func (te *ToolExecutor) buildServiceTopology(ctx context.Context, arguments string) (string, error) {
	var args BuildServiceTopologyArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse build_service_topology args: %w", err)
	}
	if args.ServiceName == "" {
		return "", fmt.Errorf("service_name is required")
	}
	maxTraces := args.MaxTraces
	if maxTraces <= 0 {
		maxTraces = defaultTopologyMaxTraces
	}
	if maxTraces > maxTopologyMaxTraces {
		maxTraces = maxTopologyMaxTraces
	}

	searchBody, err := te.queryTempo(ctx, mustMarshal(TempoQueryArgs{
		Query:         fmt.Sprintf(`{resource.service.name=%q}`, args.ServiceName),
		Limit:         maxTraces,
		DatasourceUID: args.DatasourceUID,
	}))
	if err != nil {
		return "", fmt.Errorf("search traces: %w", err)
	}
	var search tempoSearchResponse
	if err := json.Unmarshal([]byte(searchBody), &search); err != nil {
		return "", fmt.Errorf("parse search response: %w", err)
	}
	if len(search.Traces) == 0 {
		return "", fmt.Errorf("no traces found involving service %q in the searched window", args.ServiceName)
	}

	serviceSet := map[string]bool{}
	edgeCounts := map[[2]string]int{}
	tracesWalked := 0
	for _, t := range search.Traces {
		traceBody, err := te.queryTempo(ctx, mustMarshal(TempoQueryArgs{TraceID: t.TraceID, DatasourceUID: args.DatasourceUID}))
		if err != nil {
			continue // one bad trace fetch degrades the sample, not the whole call
		}
		var resp tempoTraceResponse
		if json.Unmarshal([]byte(traceBody), &resp) != nil {
			continue
		}
		spans := flattenTempoSpans(&resp)
		if len(spans) == 0 {
			continue
		}
		tracesWalked++

		byID := make(map[string]flatSpan, len(spans))
		for _, s := range spans {
			serviceSet[s.ServiceName] = true
			byID[s.SpanID] = s
		}
		for _, s := range spans {
			if s.ParentSpanID == "" {
				continue
			}
			parent, ok := byID[s.ParentSpanID]
			if !ok || parent.ServiceName == "" || s.ServiceName == "" || parent.ServiceName == s.ServiceName {
				continue // no cross-service edge to record (same service, or parent not in this trace's span set)
			}
			edgeCounts[[2]string{parent.ServiceName, s.ServiceName}]++
		}
	}
	if tracesWalked == 0 {
		return "", fmt.Errorf("found %d trace(s) but none could be fetched/parsed", len(search.Traces))
	}

	services := make([]string, 0, len(serviceSet))
	for s := range serviceSet {
		services = append(services, s)
	}
	sort.Strings(services)

	edges := make([]topologyEdge, 0, len(edgeCounts))
	for pair, count := range edgeCounts {
		edges = append(edges, topologyEdge{From: pair[0], To: pair[1], CallCount: count})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].CallCount > edges[j].CallCount })

	result := serviceTopologyResult{
		ServiceName:  args.ServiceName,
		TracesWalked: tracesWalked,
		Services:     services,
		Edges:        edges,
	}
	if len(edges) == 0 {
		result.IsPartial = true
		result.Warnings = append(result.Warnings, "no cross-service edges found -- every sampled trace may be single-service, or spans don't carry a parent/child relationship")
		result.Summary = fmt.Sprintf("%d service(s) seen across %d trace(s), but no call edges between them found", len(services), tracesWalked)
	} else {
		result.Summary = fmt.Sprintf("%d service(s), %d edge(s) derived from %d trace(s) involving %q", len(services), len(edges), tracesWalked, args.ServiceName)
	}
	result.Sources = []string{datasourceSource("tempo", args.DatasourceUID)}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
