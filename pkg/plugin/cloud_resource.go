package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
)

// AnalyzeCloudResourceArgs holds parsed arguments for analyze_cloud_resource.
type AnalyzeCloudResourceArgs struct {
	Namespace  string `json:"namespace"`
	MetricName string `json:"metric_name"`
	// Dimensions scopes the metric to one specific resource (e.g.
	// {"InstanceId": "i-0abc..."}) -- CloudWatch namespaces are shared
	// across every resource of that type, so most real queries need this
	// to mean anything.
	Dimensions map[string]string `json:"dimensions,omitempty"`
	// Statistic is one of CloudWatch's own (Average, Sum, Maximum, Minimum,
	// SampleCount) -- defaults to Average.
	Statistic string `json:"statistic,omitempty"`
	// Region is only needed when it differs from the datasource's own
	// configured default region -- left empty, Grafana's CloudWatch
	// datasource falls back to that default (verified live against this
	// plugin's own local-stack CloudWatch datasource).
	Region        string `json:"region,omitempty"`
	Start         string `json:"start,omitempty"`
	End           string `json:"end,omitempty"`
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type cloudResourceDatapoint struct {
	TimeUnixMs int64   `json:"time_unix_ms"`
	Value      float64 `json:"value"`
}

type cloudResourceResult struct {
	ToolResult
	Namespace       string                   `json:"namespace"`
	MetricName      string                   `json:"metric_name"`
	Statistic       string                   `json:"statistic"`
	Found           bool                     `json:"found"`
	Datapoints      []cloudResourceDatapoint `json:"datapoints,omitempty"`
	Mean            float64                  `json:"mean,omitempty"`
	Latest          float64                  `json:"latest,omitempty"`
	LatestDeviation float64                  `json:"latest_deviation_stddevs,omitempty"`
	IsAnomaly       bool                     `json:"is_anomaly,omitempty"`
}

// cloudResourceAnomalyThreshold mirrors analyze_metric_anomaly's own
// threshold (metric_anomaly.go) -- same statistical reasoning, applied here
// to a single most-recent point against the rest of the window instead of
// a separately-fetched baseline window (CloudWatch's own query cost per
// call is higher than Prometheus's, so this reuses one window instead of
// fetching two).
const cloudResourceAnomalyThreshold = 3.0
const cloudResourceMinBaselinePoints = 5

type cloudwatchDsQueryBody struct {
	Queries []cloudwatchDsQuery `json:"queries"`
	From    string              `json:"from"`
	To      string              `json:"to"`
}

type cloudwatchDsQuery struct {
	RefID      string            `json:"refId"`
	Datasource cloudwatchDsRef   `json:"datasource"`
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metricName"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Statistic  string            `json:"statistic"`
	Region     string            `json:"region,omitempty"`
	Period     string            `json:"period,omitempty"`
}

type cloudwatchDsRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type dsQueryResponse struct {
	Results map[string]struct {
		Status int    `json:"status"`
		Error  string `json:"error,omitempty"`
		Frames []struct {
			Data struct {
				Values [][]float64 `json:"values"`
			} `json:"data"`
		} `json:"frames"`
	} `json:"results"`
}

// analyzeCloudResource queries one CloudWatch metric (namespace + metric
// name + dimensions) via Grafana's own /api/ds/query -- CloudWatch is a
// "backend" datasource in Grafana, not a simple REST proxy like Prometheus/
// Loki/Tempo, so it can't reuse doGrafanaRequest's GET-passthrough pattern
// the way those tools do; this is Grafana's real query-engine request shape,
// verified live against a real Grafana + CloudWatch-datasource-over-
// LocalStack instance. Reports "found: false" honestly (never a false
// "healthy" reading) when CloudWatch has no datapoints for the given
// namespace/metric/dimensions/window -- a very real possibility since a
// CloudWatch namespace's exact metric/dimension names are easy to get
// slightly wrong.
func (te *ToolExecutor) analyzeCloudResource(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeCloudResourceArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_cloud_resource args: %w", err)
	}
	if args.Namespace == "" || args.MetricName == "" {
		return "", fmt.Errorf("namespace and metric_name are required")
	}
	statistic := args.Statistic
	if statistic == "" {
		statistic = "Average"
	}

	dsUID, err := te.resolveDatasourceUID(ctx, "cloudwatch", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find cloudwatch datasource: %w", err)
	}

	start := args.Start
	if start == "" {
		start = "now-1h"
	}
	end := args.End
	if end == "" {
		end = "now"
	}

	reqBody := cloudwatchDsQueryBody{
		Queries: []cloudwatchDsQuery{{
			RefID:      "A",
			Datasource: cloudwatchDsRef{Type: "cloudwatch", UID: dsUID},
			Namespace:  args.Namespace,
			MetricName: args.MetricName,
			Dimensions: args.Dimensions,
			Statistic:  statistic,
			Region:     args.Region,
			Period:     "300",
		}},
		From: start,
		To:   end,
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal ds/query request: %w", err)
	}

	respBody, err := te.doGrafanaRequest(ctx, http.MethodPost, "/api/ds/query", bytes.NewReader(bodyJSON))
	if err != nil {
		return "", fmt.Errorf("query cloudwatch: %w", err)
	}

	var parsed dsQueryResponse
	if err := json.Unmarshal([]byte(respBody), &parsed); err != nil {
		return "", fmt.Errorf("parse ds/query response: %w", err)
	}
	queryResult, ok := parsed.Results["A"]
	if !ok {
		return "", fmt.Errorf("no result returned for this query")
	}
	if queryResult.Error != "" {
		return "", fmt.Errorf("cloudwatch query failed: %s", queryResult.Error)
	}

	result := cloudResourceResult{
		Namespace:  args.Namespace,
		MetricName: args.MetricName,
		Statistic:  statistic,
	}

	if len(queryResult.Frames) == 0 || len(queryResult.Frames[0].Data.Values) < 2 || len(queryResult.Frames[0].Data.Values[1]) == 0 {
		result.Found = false
		result.Summary = fmt.Sprintf("no CloudWatch data for %s/%s in this window -- check the namespace/metric name/dimensions are exactly right", args.Namespace, args.MetricName)
		result.Sources = []string{datasourceSource("cloudwatch", args.DatasourceUID)}
		out, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return truncateString(string(out), 50000), nil
	}

	times := queryResult.Frames[0].Data.Values[0]
	values := queryResult.Frames[0].Data.Values[1]
	result.Found = true
	for i := range values {
		var t int64
		if i < len(times) {
			if timestamp, ok := checkedFloatToInt64(times[i]); ok {
				t = timestamp
			}
		}
		result.Datapoints = append(result.Datapoints, cloudResourceDatapoint{TimeUnixMs: t, Value: values[i]})
	}

	latest := values[len(values)-1]
	result.Latest = latest
	// The baseline excludes a trailing window, not just the single latest
	// point -- a real spike/incident commonly spans more than one
	// datapoint (this plugin's own seeded demo data does), and a baseline
	// that included the second-to-last point of an ongoing spike would
	// mask the anomaly by inflating its own mean/stddev with the spike
	// itself.
	// At least 2 points excluded, not just 1 -- verified live against real
	// seeded CloudWatch data that a real incident/spike commonly shows up
	// across 2+ consecutive datapoints (this plugin's own seed script
	// intentionally models exactly that), and with few total points a
	// 1-point exclusion left the spike's OTHER point inside the baseline,
	// masking the anomaly by inflating the baseline's own mean/stddev.
	recentWindow := len(values) / 5
	if recentWindow < 2 {
		recentWindow = 2
	}
	// live-crash found: a metric with only 1 real datapoint made
	// recentWindow (floored at 2) exceed len(values), so
	// values[:len(values)-recentWindow] sliced with a negative index and
	// panicked ("slice bounds out of range [:-1]") -- taking down the
	// entire plugin backend process for every concurrent user, not just
	// this one request. Clamping keeps baseline empty (not enough data,
	// handled gracefully below) instead of ever going negative.
	if recentWindow > len(values) {
		recentWindow = len(values)
	}
	baseline := values[:len(values)-recentWindow]
	if len(baseline) >= cloudResourceMinBaselinePoints {
		mean, stddev := meanAndStdDev(baseline)
		result.Mean = mean
		if stddev > 0 {
			result.LatestDeviation = math.Abs(latest-mean) / stddev
			result.IsAnomaly = result.LatestDeviation >= cloudResourceAnomalyThreshold
		}
	}

	if result.IsAnomaly {
		result.Summary = fmt.Sprintf("latest value %.2f deviates %.1f standard deviations from the baseline mean %.2f -- likely anomalous", latest, result.LatestDeviation, result.Mean)
	} else {
		result.Summary = fmt.Sprintf("%d datapoint(s), latest=%.2f, no anomaly detected", len(values), latest)
	}
	result.Sources = []string{datasourceSource("cloudwatch", args.DatasourceUID)}
	result.TimeRange = start + ".." + end

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
