package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DiagnoseKubernetesWorkloadArgs holds parsed arguments for
// diagnose_kubernetes_workload.
type DiagnoseKubernetesWorkloadArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// DatasourceUID targets a specific Prometheus datasource when more than
	// one exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type workloadMetricCheck struct {
	Query   string  `json:"query"`
	Value   float64 `json:"value,omitempty"`
	Found   bool    `json:"found"`
	Comment string  `json:"comment,omitempty"`
}

type workloadDiagnosis struct {
	ToolResult
	Namespace       string                 `json:"namespace"`
	Name            string                 `json:"name"`
	WaitingIssues   []workloadWaitingIssue `json:"waitingIssues,omitempty"`
	Restarts        workloadMetricCheck    `json:"restarts"`
	MemoryVsLimit   workloadMetricCheck    `json:"memoryVsLimitPercent"`
	CPUVsLimit      workloadMetricCheck    `json:"cpuVsLimitPercent"`
	ReadyVsDesired  workloadMetricCheck    `json:"readyVsDesiredReplicas"`
	Prerequisite    string                 `json:"prerequisite"`
	NoDataAllAround bool                   `json:"noDataAtAll,omitempty"`
}

// workloadWaitingIssue is one pod+container currently stuck in a bad
// waiting state (ImagePullBackOff, CrashLoopBackOff, ContainerCreating stuck,
// etc.) per kube_pod_container_status_waiting_reason. This is the one signal
// in this tool that directly catches a container that never successfully
// started -- restarts_total is 0 for it (a restart only counts once a
// container has run and exited at least once), and it typically has no
// container_cpu/memory_usage data either (cAdvisor has nothing to report for
// a container that was never actually running). Real, live-reproduced gap
// this closes: asked "are there any pods restarting or crashing right now?"
// against a real ImagePullBackOff pod, both the generic and a
// hand-configured specialist agent answered "no" -- the specialist's own
// domain knowledge told it to check restarts_total specifically, which is
// exactly the metric that stays at 0 for this failure mode.
type workloadWaitingIssue struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Reason    string `json:"reason"`
}

// promScalarCheck runs one PromQL query and reports whether it
// returned a usable scalar value -- shared by all the kube-state-metrics-
// dependent tools (diagnose_kubernetes_workload, analyze_container_lifecycle,
// analyze_node_health), since every one of them is a thin "run this query,
// report the value or its absence" check against metrics this plugin
// cannot guarantee are actually scraped in every deployment.
func (te *ToolExecutor) promScalarCheck(ctx context.Context, dsUID, query string) workloadMetricCheck {
	body, err := te.queryPrometheus(ctx, mustMarshal(PrometheusQueryArgs{Query: query, DatasourceUID: dsUID}))
	check := workloadMetricCheck{Query: query}
	if err != nil {
		check.Comment = "query failed: " + err.Error()
		return check
	}
	var resp promMatrixResponse
	if json.Unmarshal([]byte(body), &resp) != nil || len(resp.Data.Result) == 0 {
		check.Comment = "no data -- this metric may not be scraped in this environment"
		return check
	}
	_, values := valuesToFloats(resp.Data.Result[0].Values)
	if len(values) == 0 {
		check.Comment = "no data -- this metric may not be scraped in this environment"
		return check
	}
	check.Found = true
	check.Value = values[len(values)-1]
	return check
}

// promWaitingReasonCheck queries kube_pod_container_status_waiting_reason
// for this namespace+pod-name-prefix and returns every (pod, container,
// reason) combination currently active (metric value 1) -- see
// workloadWaitingIssue's doc comment for why this is checked as its own,
// separate signal rather than folded into promScalarCheck's single-number
// shape. A query error or "no data" is deliberately swallowed here (not
// surfaced as an issue) -- the caller already reports the restarts/memory/
// cpu/ready checks' own errors, and this metric simply not being scraped is
// exactly the same "no data" case those checks already handle honestly.
func (te *ToolExecutor) promWaitingReasonCheck(ctx context.Context, dsUID, ns, namePrefix string) []workloadWaitingIssue {
	query := fmt.Sprintf(`kube_pod_container_status_waiting_reason{namespace=%q, pod=~%q} == 1`, ns, namePrefix+".*")
	body, err := te.queryPrometheus(ctx, mustMarshal(PrometheusQueryArgs{Query: query, DatasourceUID: dsUID}))
	if err != nil {
		return nil
	}
	var resp promMatrixResponse
	if json.Unmarshal([]byte(body), &resp) != nil {
		return nil
	}
	var issues []workloadWaitingIssue
	for _, series := range resp.Data.Result {
		if len(series.Values) == 0 {
			continue
		}
		_, values := valuesToFloats(series.Values)
		if len(values) == 0 || values[len(values)-1] != 1 {
			continue
		}
		issues = append(issues, workloadWaitingIssue{
			Pod:       series.Metric["pod"],
			Container: series.Metric["container"],
			Reason:    series.Metric["reason"],
		})
	}
	return issues
}

// workloadCheckSummary renders one workloadMetricCheck for a Summary string
// -- shared by every kube-state-metrics-dependent tool's Summary
// construction so "no data" reads the same way everywhere.
func workloadCheckSummary(check workloadMetricCheck) string {
	if !check.Found {
		return "N/A"
	}
	return fmt.Sprintf("%.1f", check.Value)
}

// diagnoseKubernetesWorkload cross-references kube-state-metrics (restarts,
// memory/CPU usage vs configured limits, ready vs desired replicas) for one
// namespace+name. Requires kube-state-metrics and cAdvisor container
// metrics to actually be scraped by the Prometheus this plugin queries --
// NOT guaranteed in every deployment (the local-stack test environment
// this plugin has been validated against does not have them), so every
// check reports "no data" honestly rather than a false "healthy" reading
// when the underlying metric simply doesn't exist here.
func (te *ToolExecutor) diagnoseKubernetesWorkload(ctx context.Context, arguments string) (string, error) {
	var args DiagnoseKubernetesWorkloadArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse diagnose_kubernetes_workload args: %w", err)
	}
	if args.Namespace == "" || args.Name == "" {
		return "", fmt.Errorf("namespace and name are required")
	}

	ns, name, dsUID := args.Namespace, args.Name, args.DatasourceUID
	restarts := te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`sum(kube_pod_container_status_restarts_total{namespace=%q, pod=~%q})`, ns, name+".*"))

	memPct := te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * sum(container_memory_working_set_bytes{namespace=%q, pod=~%q}) / sum(kube_pod_container_resource_limits{namespace=%q, pod=~%q, resource="memory"})`,
		ns, name+".*", ns, name+".*"))

	cpuPct := te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * sum(rate(container_cpu_usage_seconds_total{namespace=%q, pod=~%q}[5m])) / sum(kube_pod_container_resource_limits{namespace=%q, pod=~%q, resource="cpu"})`,
		ns, name+".*", ns, name+".*"))

	readyPct := te.promScalarCheck(ctx, dsUID, fmt.Sprintf(
		`100 * sum(kube_deployment_status_replicas_available{namespace=%q, deployment=%q}) / sum(kube_deployment_spec_replicas{namespace=%q, deployment=%q})`,
		ns, name, ns, name))

	waitingIssues := te.promWaitingReasonCheck(ctx, dsUID, ns, name)

	diagnosis := workloadDiagnosis{
		Namespace:      ns,
		Name:           name,
		WaitingIssues:  waitingIssues,
		Restarts:       restarts,
		MemoryVsLimit:  memPct,
		CPUVsLimit:     cpuPct,
		ReadyVsDesired: readyPct,
		Prerequisite:   "Requires kube-state-metrics and cAdvisor container metrics to be scraped by this instance's Prometheus -- not guaranteed in every deployment. A \"found: false\" check means the metric wasn't available here, not that the value is zero/healthy.",
	}
	if !restarts.Found && !memPct.Found && !cpuPct.Found && !readyPct.Found && len(waitingIssues) == 0 {
		diagnosis.NoDataAllAround = true
		diagnosis.IsPartial = true
		diagnosis.Warnings = append(diagnosis.Warnings, "no kube-state-metrics/cAdvisor data found for this workload in this environment's Prometheus")
		diagnosis.Summary = fmt.Sprintf("No kube-state-metrics data available for %s/%s -- cannot assess restarts, resource usage, or replica health here.", ns, name)
	} else {
		diagnosis.Summary = fmt.Sprintf("%s/%s: restarts=%s, memory=%s%%, cpu=%s%%, ready=%s%%",
			ns, name, workloadCheckSummary(restarts), workloadCheckSummary(memPct), workloadCheckSummary(cpuPct), workloadCheckSummary(readyPct))
	}
	// Prepended, not just listed alongside the other fields -- a container
	// stuck in ImagePullBackOff/CrashLoopBackOff/etc. is a real, currently
	// ongoing problem regardless of what restarts/memory/cpu/ready happen to
	// read (see workloadWaitingIssue's doc comment for why those can all
	// look "healthy" for exactly this failure mode), so it must not be
	// something the model can skim past as one line among several.
	if len(waitingIssues) > 0 {
		parts := make([]string, 0, len(waitingIssues))
		for _, issue := range waitingIssues {
			parts = append(parts, fmt.Sprintf("%s/%s: %s", issue.Pod, issue.Container, issue.Reason))
		}
		diagnosis.Summary = fmt.Sprintf("ISSUE FOUND -- %d container(s) currently stuck: %s. %s",
			len(waitingIssues), strings.Join(parts, "; "), diagnosis.Summary)
	}
	diagnosis.Sources = []string{datasourceSource("prometheus", dsUID)}

	out, err := json.Marshal(diagnosis)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
