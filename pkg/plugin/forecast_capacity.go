package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ForecastCapacityArgs holds parsed arguments for forecast_capacity.
type ForecastCapacityArgs struct {
	Query     string  `json:"query"`
	Start     string  `json:"start,omitempty"`
	End       string  `json:"end,omitempty"`
	Step      string  `json:"step,omitempty"`
	Threshold float64 `json:"threshold"`
	// Direction is "rising" (default -- alert when the metric grows past
	// threshold, e.g. disk usage) or "falling" (e.g. free space, remaining
	// budget shrinking toward zero).
	Direction string `json:"direction,omitempty"`
	// DatasourceUID targets a specific Prometheus datasource when more than
	// one exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type capacityForecastResult struct {
	ToolResult
	Query               string  `json:"query"`
	Threshold           float64 `json:"threshold"`
	Direction           string  `json:"direction"`
	SamplePoints        int     `json:"sample_points"`
	CurrentValue        float64 `json:"current_value"`
	SlopePerHour        float64 `json:"slope_per_hour"`
	WillCrossThreshold  bool    `json:"will_cross_threshold"`
	EstimatedCrossingAt string  `json:"estimated_crossing_at,omitempty"`
	Note                string  `json:"note,omitempty"`
}

// forecastCapacityMinPoints is the fewest samples this tool trusts before
// fitting a trend line -- 2 points always "fit" a line perfectly but say
// nothing reliable about a real trend.
const forecastCapacityMinPoints = 5

// linearRegression fits y = slope*x + intercept via ordinary least squares
// over (x, y) pairs -- x here is always seconds-since-the-first-sample, to
// keep the numbers small and numerically stable regardless of how far in
// the past the window starts.
func linearRegression(xs, ys []float64) (slope, intercept float64) {
	n := float64(len(xs))
	var sumX, sumY, sumXY, sumXX float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
		sumXY += xs[i] * ys[i]
		sumXX += xs[i] * xs[i]
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, sumY / n
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept = (sumY - slope*sumX) / n
	return slope, intercept
}

// forecastCapacity fits a simple linear trend over a Prometheus metric's
// recent history and projects forward to estimate when it crosses a given
// threshold (e.g. "when does disk usage hit 100%") -- instead of requiring
// the model or user to eyeball a graph and guess.
func (te *ToolExecutor) forecastCapacity(ctx context.Context, arguments string) (string, error) {
	var args ForecastCapacityArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse forecast_capacity args: %w", err)
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	if strings.TrimSpace(args.Query) == "*" {
		return "", fmt.Errorf("%q is not a valid PromQL query", args.Query)
	}
	direction := args.Direction
	if direction == "" {
		direction = "rising"
	}
	if direction != "rising" && direction != "falling" {
		return "", fmt.Errorf(`direction must be "rising" or "falling", got %q`, direction)
	}

	step := args.Step
	if step == "" {
		step = "5m"
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

	dsUID, err := te.resolveDatasourceUID(ctx, "prometheus", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find prometheus datasource: %w", err)
	}
	resp, err := fetchPromMatrix(ctx, te, dsUID, args.Query, step, start, end)
	if err != nil {
		return "", fmt.Errorf("query prometheus: %w", err)
	}
	if len(resp.Data.Result) == 0 {
		return "", fmt.Errorf("query returned no series to forecast from")
	}

	// Forecasting a trend across multiple distinct series (e.g. one per
	// pod) conflates unrelated capacity curves into one meaningless
	// average -- this tool is deliberately single-series: use a query
	// specific enough to return exactly one (e.g. aggregate with sum()/avg()
	// first) rather than a raw multi-series selector.
	if len(resp.Data.Result) > 1 {
		return "", fmt.Errorf("query returned %d series -- forecast_capacity needs exactly one; aggregate first (e.g. sum(...) or avg(...))", len(resp.Data.Result))
	}

	timestamps, values := valuesToFloats(resp.Data.Result[0].Values)
	if len(timestamps) < forecastCapacityMinPoints {
		return "", fmt.Errorf("only %d data point(s) in range -- need at least %d for a meaningful trend", len(timestamps), forecastCapacityMinPoints)
	}

	firstTS := timestamps[0]
	xs := make([]float64, len(timestamps))
	for i, ts := range timestamps {
		xs[i] = float64(ts - firstTS)
	}
	slope, intercept := linearRegression(xs, values)
	slopePerHour := slope * 3600

	currentValue := values[len(values)-1]

	result := capacityForecastResult{
		Query:        args.Query,
		Threshold:    args.Threshold,
		Direction:    direction,
		SamplePoints: len(values),
		CurrentValue: currentValue,
		SlopePerHour: slopePerHour,
	}

	willCross := (direction == "rising" && slope > 0 && currentValue < args.Threshold) ||
		(direction == "falling" && slope < 0 && currentValue > args.Threshold)

	if !willCross {
		result.Note = fmt.Sprintf("trend is not moving toward the threshold (direction=%s, slope/hour=%.4g) -- no crossing projected from current data", direction, slopePerHour)
	} else {
		// x where slope*x + intercept == threshold
		crossingX := (args.Threshold - intercept) / slope
		secondsFromFirst := crossingX
		crossingTime := time.Unix(firstTS, 0).Add(time.Duration(secondsFromFirst) * time.Second)
		result.WillCrossThreshold = true
		result.EstimatedCrossingAt = crossingTime.UTC().Format(time.RFC3339)
		if crossingTime.Before(now) {
			result.Note = "projected crossing point is in the past relative to now -- the threshold may already have been crossed, or the trend changed since"
		}
	}
	if result.Note != "" {
		result.Summary = result.Note
	} else {
		result.Summary = fmt.Sprintf("projected to cross %.4g at %s (current=%.4g, slope/hour=%.4g)", args.Threshold, result.EstimatedCrossingAt, currentValue, slopePerHour)
	}
	result.Sources = []string{datasourceSource("prometheus", dsUID)}
	result.TimeRange = start.Format(time.RFC3339) + ".." + end.Format(time.RFC3339)

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
