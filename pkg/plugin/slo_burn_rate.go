package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AnalyzeSLOBurnRateArgs holds parsed arguments for analyze_slo_burn_rate.
type AnalyzeSLOBurnRateArgs struct {
	// GoodQuery and TotalQuery are instant PromQL expressions returning a
	// single number each (e.g. "sum(increase(http_requests_total{status!~
	// \"5..\"}[1h]))" and "sum(increase(http_requests_total[1h]))") -- the
	// window is whatever the caller already baked into the query (e.g.
	// [1h]); call this tool again with a different window baked in for a
	// multi-window burn-rate view, rather than this tool templating window
	// substitution itself.
	GoodQuery    string  `json:"good_query"`
	TotalQuery   string  `json:"total_query"`
	SLOTarget    float64 `json:"slo_target"`
	BudgetWindow string  `json:"budget_window,omitempty"`
	// DatasourceUID targets a specific Prometheus datasource when more than
	// one exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type sloBurnRateResult struct {
	ToolResult
	SLOTarget        float64 `json:"slo_target"`
	ErrorBudget      float64 `json:"error_budget"`
	GoodCount        float64 `json:"good_count"`
	TotalCount       float64 `json:"total_count"`
	ActualErrorRate  float64 `json:"actual_error_rate"`
	BurnRate         float64 `json:"burn_rate"`
	BudgetWindow     string  `json:"budget_window,omitempty"`
	TimeToExhaustion string  `json:"estimated_time_to_exhaustion,omitempty"`
	Note             string  `json:"note"`
}

// analyzeSLOBurnRate computes how fast an error budget is being consumed:
// burn_rate = actual_error_rate / (1 - slo_target). A burn_rate of 1 means
// consuming the budget exactly as fast as the SLO period allows; anything
// above 1 means the budget will run out before the period ends. Requires
// the caller to already have real good/total PromQL queries for their SLI
// -- this tool does no SLO definition of its own, it just does the burn-
// rate arithmetic once real counts are in hand.
func (te *ToolExecutor) analyzeSLOBurnRate(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeSLOBurnRateArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_slo_burn_rate args: %w", err)
	}
	if args.GoodQuery == "" || args.TotalQuery == "" {
		return "", fmt.Errorf("good_query and total_query are required")
	}
	if args.SLOTarget <= 0 || args.SLOTarget >= 1 {
		return "", fmt.Errorf("slo_target must be between 0 and 1 (exclusive), e.g. 0.999 for 99.9%%")
	}

	dsUID, err := te.resolveDatasourceUID(ctx, "prometheus", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find prometheus datasource: %w", err)
	}

	now := time.Now()
	goodResp, err := fetchPromMatrix(ctx, te, dsUID, args.GoodQuery, "60s", now.Add(-1*time.Minute), now)
	if err != nil {
		return "", fmt.Errorf("query good_query: %w", err)
	}
	totalResp, err := fetchPromMatrix(ctx, te, dsUID, args.TotalQuery, "60s", now.Add(-1*time.Minute), now)
	if err != nil {
		return "", fmt.Errorf("query total_query: %w", err)
	}

	goodCount, err := lastScalarValue(goodResp)
	if err != nil {
		return "", fmt.Errorf("good_query: %w", err)
	}
	totalCount, err := lastScalarValue(totalResp)
	if err != nil {
		return "", fmt.Errorf("total_query: %w", err)
	}
	if totalCount == 0 {
		return "", fmt.Errorf("total_query returned 0 -- no requests/events in this window to compute a burn rate from")
	}

	errorBudget := 1 - args.SLOTarget
	actualErrorRate := 1 - (goodCount / totalCount)
	burnRate := actualErrorRate / errorBudget

	result := sloBurnRateResult{
		SLOTarget:       args.SLOTarget,
		ErrorBudget:     errorBudget,
		GoodCount:       goodCount,
		TotalCount:      totalCount,
		ActualErrorRate: actualErrorRate,
		BurnRate:        burnRate,
		BudgetWindow:    args.BudgetWindow,
	}

	if burnRate <= 0 {
		result.Note = "no errors in this window -- error budget is not being consumed right now"
	} else if args.BudgetWindow != "" {
		windowDur, err := parseDayAwareDuration(args.BudgetWindow)
		if err == nil && burnRate > 0 {
			exhaustion := time.Duration(float64(windowDur) / burnRate)
			result.TimeToExhaustion = exhaustion.String()
			if burnRate > 1 {
				result.Note = fmt.Sprintf("burning the error budget %.2fx faster than sustainable for a %s period -- at this rate the budget for the CURRENT period runs out before it ends", burnRate, args.BudgetWindow)
			} else {
				result.Note = fmt.Sprintf("burning the error budget at %.2fx the sustainable rate for a %s period -- within budget at this rate", burnRate, args.BudgetWindow)
			}
		}
	} else if burnRate > 1 {
		result.Note = fmt.Sprintf("burning the error budget %.2fx faster than sustainable -- pass budget_window for an estimated time-to-exhaustion", burnRate)
	} else {
		result.Note = fmt.Sprintf("burning the error budget at %.2fx the sustainable rate -- within budget", burnRate)
	}
	result.Summary = result.Note
	result.Sources = []string{datasourceSource("prometheus", dsUID)}
	result.TimeRange = now.Add(-1*time.Minute).Format(time.RFC3339) + ".." + now.Format(time.RFC3339)

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}

// lastScalarValue extracts the most recent value from a single-series
// Prometheus range response, erroring clearly if the query returned no
// series or more than one (a burn-rate calculation needs exactly one
// number per side).
func lastScalarValue(resp *promMatrixResponse) (float64, error) {
	if len(resp.Data.Result) == 0 {
		return 0, fmt.Errorf("query returned no data")
	}
	if len(resp.Data.Result) > 1 {
		return 0, fmt.Errorf("query returned %d series -- must return exactly one, aggregate first (e.g. sum(...))", len(resp.Data.Result))
	}
	_, values := valuesToFloats(resp.Data.Result[0].Values)
	if len(values) == 0 {
		return 0, fmt.Errorf("query returned no numeric value")
	}
	return values[len(values)-1], nil
}
