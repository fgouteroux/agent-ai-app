package plugin

import (
	"context"
	"encoding/json"
	"fmt"
)

// AnalyzeNodeHealthArgs holds parsed arguments for analyze_node_health.
type AnalyzeNodeHealthArgs struct {
	// Instance is node_exporter's own "instance" label value (typically
	// host:port, or just a hostname substring -- matched with a regex so
	// an exact port doesn't need to be known).
	Instance string `json:"instance"`
	// DatasourceUID targets a specific Prometheus datasource when more than
	// one exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type nodeHealthResult struct {
	ToolResult
	Instance     string              `json:"instance"`
	CPUBusyPct   workloadMetricCheck `json:"cpuBusyPercent"`
	IOWaitPct    workloadMetricCheck `json:"ioWaitPercent"`
	MemoryPct    workloadMetricCheck `json:"memoryUsedPercent"`
	DiskUsedPct  workloadMetricCheck `json:"maxDiskUsedPercent"`
	Prerequisite string              `json:"prerequisite"`
}

// analyzeNodeHealth checks a node's own resource pressure (CPU busy, I/O
// wait, memory, worst-filesystem disk usage) via node_exporter -- goes
// beyond diagnose_kubernetes_workload's pod-level view to the underlying
// machine. Requires node_exporter (or an equivalent exporting the same
// metric names) to be scraped by this instance's Prometheus -- NOT
// guaranteed in every deployment, same honesty caveat as the
// kube-state-metrics-dependent tools.
func (te *ToolExecutor) analyzeNodeHealth(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeNodeHealthArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_node_health args: %w", err)
	}
	if args.Instance == "" {
		return "", fmt.Errorf("instance is required")
	}

	instance, dsUID := args.Instance, args.DatasourceUID
	result := nodeHealthResult{
		Instance:     instance,
		Prerequisite: "Requires node_exporter (or an equivalent exporting the same metric names) to be scraped by this instance's Prometheus -- not guaranteed in every deployment.",
	}

	result.CPUBusyPct = te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * (1 - avg(rate(node_cpu_seconds_total{instance=~%q, mode="idle"}[5m])))`, ".*"+instance+".*"))

	result.IOWaitPct = te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * avg(rate(node_cpu_seconds_total{instance=~%q, mode="iowait"}[5m]))`, ".*"+instance+".*"))

	result.MemoryPct = te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * (1 - node_memory_MemAvailable_bytes{instance=~%q} / node_memory_MemTotal_bytes{instance=~%q})`,
		".*"+instance+".*", ".*"+instance+".*"))

	result.DiskUsedPct = te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`max(100 * (1 - node_filesystem_avail_bytes{instance=~%q, fstype!="tmpfs"} / node_filesystem_size_bytes{instance=~%q, fstype!="tmpfs"}))`,
		".*"+instance+".*", ".*"+instance+".*"))

	result.Sources = []string{datasourceSource("prometheus", dsUID)}
	if !result.CPUBusyPct.Found && !result.IOWaitPct.Found && !result.MemoryPct.Found && !result.DiskUsedPct.Found {
		result.IsPartial = true
		result.Warnings = append(result.Warnings, "no node_exporter data found for this instance in this environment's Prometheus")
		result.Summary = fmt.Sprintf("No node_exporter data available for instance %q -- cannot assess node health.", instance)
	} else {
		result.Summary = fmt.Sprintf("instance %s: cpu busy=%s%%, io wait=%s%%, memory=%s%%, max disk used=%s%%",
			instance, workloadCheckSummary(result.CPUBusyPct), workloadCheckSummary(result.IOWaitPct), workloadCheckSummary(result.MemoryPct), workloadCheckSummary(result.DiskUsedPct))
	}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
