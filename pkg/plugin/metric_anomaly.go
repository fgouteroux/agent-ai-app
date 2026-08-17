package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AnalyzeMetricAnomalyArgs holds parsed arguments for analyze_metric_anomaly.
type AnalyzeMetricAnomalyArgs struct {
	Query          string `json:"query"`
	Start          string `json:"start,omitempty"`
	End            string `json:"end,omitempty"`
	Step           string `json:"step,omitempty"`
	BaselineOffset string `json:"baseline_offset,omitempty"`
	// DatasourceUID targets a specific Prometheus datasource when more than
	// one exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

// minBaselinePoints is the fewest baseline data points this tool trusts
// before computing mean/stddev off them -- a "baseline" of 1-2 points isn't
// a real statistical baseline, and flagging deviations against it would
// just be noise dressed up as an anomaly.
const minBaselinePoints = 5

// anomalyStdDevThreshold is how many standard deviations from the baseline
// mean a point must be before it's called an anomaly. 3 is the common
// "far outside normal variation" convention (~99.7% of a normal
// distribution falls within 3 stddev), chosen over a lower bound to avoid
// flagging routine noise as an incident.
const anomalyStdDevThreshold = 3.0

type anomalyPoint struct {
	Timestamp    int64   `json:"timestamp"`
	Value        float64 `json:"value"`
	BaselineMean float64 `json:"baseline_mean"`
	Deviation    float64 `json:"deviation_stddevs"`
}

type metricAnomalySeriesResult struct {
	Metric         map[string]string `json:"metric"`
	BaselineMean   float64           `json:"baseline_mean"`
	BaselineStdDev float64           `json:"baseline_stddev"`
	BaselinePoints int               `json:"baseline_points"`
	Warning        string            `json:"warning,omitempty"`
	Anomalies      []anomalyPoint    `json:"anomalies"`
}

type metricAnomalyResult struct {
	ToolResult
	Query          string                      `json:"query"`
	BaselineOffset string                      `json:"baseline_offset"`
	Series         []metricAnomalySeriesResult `json:"series"`
}

// promMatrixResponse is the subset of Prometheus's /api/v1/query_range
// response this tool needs.
type promMatrixResponse struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"` // [unix_seconds(float), "value string"]
		} `json:"result"`
	} `json:"data"`
}

// parseDayAwareDuration extends time.ParseDuration with a "d" (day) suffix,
// since PromQL/human-written offsets commonly use "1d"/"7d" but Go's
// standard duration parser only understands up to hours.
func parseDayAwareDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day-based duration %q: %w", s, err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func fetchPromMatrix(ctx context.Context, te *ToolExecutor, dsUID, query, step string, start, end time.Time) (*promMatrixResponse, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("step", step)
	params.Set("start", fmt.Sprintf("%d", start.Unix()))
	params.Set("end", fmt.Sprintf("%d", end.Unix()))

	apiPath := fmt.Sprintf("/api/datasources/proxy/uid/%s/api/v1/query_range?%s", url.PathEscape(dsUID), params.Encode())
	body, err := te.doGrafanaRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var resp promMatrixResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("parse prometheus response: %w", err)
	}
	return &resp, nil
}

func valuesToFloats(values [][2]any) ([]int64, []float64) {
	timestamps := make([]int64, 0, len(values))
	floats := make([]float64, 0, len(values))
	for _, v := range values {
		ts, ok := v[0].(float64)
		if !ok {
			continue
		}
		strVal, ok := v[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strVal, 64)
		if err != nil {
			continue
		}
		timestamps = append(timestamps, int64(ts))
		floats = append(floats, f)
	}
	return timestamps, floats
}

func meanAndStdDev(values []float64) (mean, stddev float64) {
	if len(values) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean = sum / float64(len(values))

	var sumSq float64
	for _, v := range values {
		d := v - mean
		sumSq += d * d
	}
	stddev = math.Sqrt(sumSq / float64(len(values)))
	return mean, stddev
}

// seriesKey builds a stable identity for a Prometheus series from its
// label set, so the same series can be matched between the current-window
// response and the baseline-window response (label sets are otherwise
// unordered maps with no guaranteed correspondence by position).
func seriesKey(metric map[string]string) string {
	keys := make([]string, 0, len(metric))
	for k := range metric {
		keys = append(keys, k)
	}
	// Sort for a deterministic key regardless of map iteration order.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(metric[k])
		b.WriteByte(',')
	}
	return b.String()
}

// analyzeMetricAnomaly compares a metric's current values against the SAME
// query run over a baseline window shifted back by baseline_offset (e.g.
// "1d" for yesterday, "7d" for last week), flagging points that deviate
// more than anomalyStdDevThreshold standard deviations from the baseline's
// own mean -- instead of requiring the model (or the user) to eyeball a
// graph to notice something's off.
func (te *ToolExecutor) analyzeMetricAnomaly(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeMetricAnomalyArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_metric_anomaly args: %w", err)
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	if strings.TrimSpace(args.Query) == "*" {
		return "", fmt.Errorf("%q is not a valid PromQL query", args.Query)
	}

	baselineOffsetStr := args.BaselineOffset
	if baselineOffsetStr == "" {
		baselineOffsetStr = "1d"
	}
	baselineOffset, err := parseDayAwareDuration(baselineOffsetStr)
	if err != nil {
		return "", fmt.Errorf("invalid baseline_offset: %w", err)
	}

	step := args.Step
	if step == "" {
		step = "60s"
	}

	now := time.Now()
	end := now
	if args.End != "" {
		if t, ok := resolveTimeValue(args.End, now); ok {
			end = t
		}
	}
	start := end.Add(-1 * time.Hour)
	if args.Start != "" {
		if t, ok := resolveTimeValue(args.Start, now); ok {
			start = t
		}
	}

	dsUID, err := te.resolveDatasourceUID(ctx, "prometheus", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find prometheus datasource: %w", err)
	}

	current, err := fetchPromMatrix(ctx, te, dsUID, args.Query, step, start, end)
	if err != nil {
		return "", fmt.Errorf("query current window: %w", err)
	}
	baseline, err := fetchPromMatrix(ctx, te, dsUID, args.Query, step, start.Add(-baselineOffset), end.Add(-baselineOffset))
	if err != nil {
		return "", fmt.Errorf("query baseline window: %w", err)
	}

	baselineBySeries := make(map[string][]float64, len(baseline.Data.Result))
	for _, s := range baseline.Data.Result {
		_, floats := valuesToFloats(s.Values)
		baselineBySeries[seriesKey(s.Metric)] = floats
	}

	results := make([]metricAnomalySeriesResult, 0, len(current.Data.Result))
	anomalyCount := 0
	for _, s := range current.Data.Result {
		key := seriesKey(s.Metric)
		baselineValues := baselineBySeries[key]
		sr := metricAnomalySeriesResult{Metric: s.Metric, Anomalies: []anomalyPoint{}}

		if len(baselineValues) < minBaselinePoints {
			sr.Warning = fmt.Sprintf("only %d baseline point(s) available (need at least %d) -- skipping anomaly detection for this series, baseline too small to be statistically meaningful", len(baselineValues), minBaselinePoints)
			results = append(results, sr)
			continue
		}

		mean, stddev := meanAndStdDev(baselineValues)
		sr.BaselineMean = mean
		sr.BaselineStdDev = stddev
		sr.BaselinePoints = len(baselineValues)

		timestamps, currentValues := valuesToFloats(s.Values)
		for i, v := range currentValues {
			if stddev == 0 {
				if v != mean {
					sr.Anomalies = append(sr.Anomalies, anomalyPoint{Timestamp: timestamps[i], Value: v, BaselineMean: mean, Deviation: math.Inf(1)})
				}
				continue
			}
			deviation := math.Abs(v-mean) / stddev
			if deviation >= anomalyStdDevThreshold {
				sr.Anomalies = append(sr.Anomalies, anomalyPoint{Timestamp: timestamps[i], Value: v, BaselineMean: mean, Deviation: deviation})
			}
		}
		anomalyCount += len(sr.Anomalies)
		results = append(results, sr)
	}

	result := metricAnomalyResult{
		Query:          args.Query,
		BaselineOffset: baselineOffsetStr,
		Series:         results,
	}
	if len(current.Data.Result) == 0 {
		result.IsPartial = true
		result.Warnings = append(result.Warnings, "query returned no series in the current window")
		result.Summary = fmt.Sprintf("no series returned for query %q", args.Query)
	} else {
		result.Summary = fmt.Sprintf("%d series checked against a %s-shifted baseline, %d anomal(y/ies) found", len(results), baselineOffsetStr, anomalyCount)
	}
	result.Sources = []string{datasourceSource("prometheus", args.DatasourceUID)}
	result.TimeRange = start.Format(time.RFC3339) + ".." + end.Format(time.RFC3339)

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
