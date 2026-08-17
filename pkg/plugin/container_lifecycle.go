package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AnalyzeContainerLifecycleArgs holds parsed arguments for
// analyze_container_lifecycle.
type AnalyzeContainerLifecycleArgs struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	// DatasourceUID targets a specific Prometheus datasource when more than
	// one exists -- see PrometheusQueryArgs.DatasourceUID. Applies only to
	// this tool's kube-state-metrics checks; the internal recent-logs
	// lookup still auto-resolves its own Loki datasource.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type containerLifecycleResult struct {
	ToolResult
	Namespace            string                 `json:"namespace"`
	Pod                  string                 `json:"pod"`
	WaitingIssues        []workloadWaitingIssue `json:"waitingIssues,omitempty"`
	LastTerminatedReason string                 `json:"lastTerminatedReason,omitempty"`
	LastExitCode         workloadMetricCheck    `json:"lastExitCode"`
	MemoryVsLimit        workloadMetricCheck    `json:"memoryVsLimitPercentAtLastSample"`
	RecentLogs           string                 `json:"recentLogs,omitempty"`
	RecentLogsError      string                 `json:"recentLogsError,omitempty"`
	Prerequisite         string                 `json:"prerequisite"`
}

// kubeStateLabelValue runs a PromQL query expected to return a gauge with a
// meaningful LABEL (not a meaningful numeric value) -- kube-state-metrics
// exposes reasons this way, e.g.
// kube_pod_container_status_last_terminated_reason{reason="OOMKilled"} 1 --
// and returns the value of labelName from whichever series is present.
func (te *ToolExecutor) kubeStateLabelValue(ctx context.Context, dsUID, query, labelName string) (string, error) {
	body, err := te.queryPrometheus(ctx, mustMarshal(PrometheusQueryArgs{Query: query, DatasourceUID: dsUID}))
	if err != nil {
		return "", err
	}
	var resp promMatrixResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("parse prometheus response: %w", err)
	}
	if len(resp.Data.Result) == 0 {
		return "", nil
	}
	return resp.Data.Result[0].Metric[labelName], nil
}

// analyzeContainerLifecycle discovers why a container died (kube-state-metrics'
// own last-terminated reason/exit code -- OOMKilled, Error, Completed, ...),
// its memory usage vs limit, and Loki logs from around the same window (via
// analyze_log_patterns, grouped rather than raw) -- AND whether it's
// currently stuck in a bad waiting state (ImagePullBackOff,
// CrashLoopBackOff, ...) instead of having died at all. That last check
// matters for the exact same reason it was added to
// diagnose_kubernetes_workload (see workloadWaitingIssue's doc comment): a
// container that never successfully started has no last-terminated reason
// and no exit code either, so without this check this tool's own "no data"
// fallback would read as "nothing to report" for the one failure mode most
// likely to prompt someone to call a tool named "why did this container
// die". Same kube-state-metrics dependency and honesty caveat as
// diagnose_kubernetes_workload -- not guaranteed to be scraped in every
// deployment.
func (te *ToolExecutor) analyzeContainerLifecycle(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeContainerLifecycleArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_container_lifecycle args: %w", err)
	}
	if args.Namespace == "" || args.Pod == "" {
		return "", fmt.Errorf("namespace and pod are required")
	}

	ns, pod, dsUID := args.Namespace, args.Pod, args.DatasourceUID
	result := containerLifecycleResult{
		Namespace:    ns,
		Pod:          pod,
		Prerequisite: "Requires kube-state-metrics and cAdvisor container metrics to be scraped by this instance's Prometheus -- not guaranteed in every deployment.",
	}

	reason, err := te.kubeStateLabelValue(ctx, dsUID, fmt.Sprintf(
		`kube_pod_container_status_last_terminated_reason{namespace=%q, pod=~%q} == 1`, ns, pod+".*"), "reason")
	if err == nil {
		result.LastTerminatedReason = reason
	}

	result.LastExitCode = te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`max(kube_pod_container_status_last_terminated_exitcode{namespace=%q, pod=~%q})`, ns, pod+".*"))

	result.MemoryVsLimit = te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * max(container_memory_working_set_bytes{namespace=%q, pod=~%q}) / max(kube_pod_container_resource_limits{namespace=%q, pod=~%q, resource="memory"})`,
		ns, pod+".*", ns, pod+".*"))

	result.WaitingIssues = te.promWaitingReasonCheck(ctx, dsUID, ns, pod)

	logSelector := fmt.Sprintf(`{namespace=%q, pod=~%q}`, ns, pod+".*")
	logArgs, _ := json.Marshal(AnalyzeLogPatternsArgs{Selector: logSelector})
	if logResult, err := te.analyzeLogPatterns(ctx, string(logArgs)); err != nil {
		result.RecentLogsError = err.Error()
		result.IsPartial = true
		result.Warnings = append(result.Warnings, "could not fetch recent logs: "+err.Error())
	} else {
		result.RecentLogs = truncateString(logResult, 10000)
	}

	result.Sources = []string{datasourceSource("prometheus", dsUID), "loki (auto-resolved)"}
	if !result.LastExitCode.Found && !result.MemoryVsLimit.Found && result.LastTerminatedReason == "" && len(result.WaitingIssues) == 0 {
		result.IsPartial = true
		result.Warnings = append(result.Warnings, "no kube-state-metrics/cAdvisor data found for this container in this environment's Prometheus")
		result.Summary = fmt.Sprintf("No kube-state-metrics data available for %s/%s -- cannot determine why it died.", ns, pod)
	} else {
		reason := result.LastTerminatedReason
		if reason == "" {
			reason = "unknown"
		}
		result.Summary = fmt.Sprintf("%s/%s: last terminated reason=%s, exit code=%s, memory at last sample=%s%%",
			ns, pod, reason, workloadCheckSummary(result.LastExitCode), workloadCheckSummary(result.MemoryVsLimit))
	}
	// Prepended, not just listed alongside the other fields -- see the same
	// choice in diagnoseKubernetesWorkload and workloadWaitingIssue's doc
	// comment for why this must not be something the model can skim past.
	if len(result.WaitingIssues) > 0 {
		parts := make([]string, 0, len(result.WaitingIssues))
		for _, issue := range result.WaitingIssues {
			parts = append(parts, fmt.Sprintf("%s/%s: %s", issue.Pod, issue.Container, issue.Reason))
		}
		result.Summary = fmt.Sprintf("ISSUE FOUND -- currently stuck (never finished starting, so it hasn't \"died\" in the terminated-reason sense): %s. %s",
			strings.Join(parts, "; "), result.Summary)
	}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
