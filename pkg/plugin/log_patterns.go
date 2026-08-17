package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// AnalyzeLogPatternsArgs holds parsed arguments for analyze_log_patterns.
type AnalyzeLogPatternsArgs struct {
	Selector string `json:"selector"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	// DatasourceUID targets a specific Loki datasource when more than one
	// exists -- see PrometheusQueryArgs.DatasourceUID.
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

// logPatternNormalizers strip the parts of a log line that vary between
// otherwise-identical occurrences (UUIDs, timestamps, IPs, hex hashes, bare
// numbers) so repeats of the same underlying message group together instead
// of counting as distinct patterns. Order matters: more specific patterns
// (UUID, IP) must run before the generic bare-number one, or their digits
// would already be replaced piecemeal by the number pattern first.
var logPatternNormalizers = []struct {
	pattern     *regexp.Regexp
	placeholder string
}{
	{regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), "<uuid>"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "<ip>"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "<ts>"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{12,}\b`), "<hash>"},
	{regexp.MustCompile(`\b\d+\b`), "<num>"},
}

// normalizeLogLine replaces the variable parts of a log line with stable
// placeholders -- see logPatternNormalizers.
func normalizeLogLine(line string) string {
	for _, n := range logPatternNormalizers {
		line = n.pattern.ReplaceAllString(line, n.placeholder)
	}
	return line
}

type logPatternsResult struct {
	ToolResult
	Selector      string        `json:"selector"`
	LinesScanned  int           `json:"lines_scanned"`
	PatternCount  int           `json:"pattern_count"`
	Patterns      []*logPattern `json:"patterns"`
	Truncated     bool          `json:"truncated"`
	TruncatedNote string        `json:"truncated_note,omitempty"`
}

type logPattern struct {
	Pattern   string `json:"pattern"`
	Count     int    `json:"count"`
	Example   string `json:"example"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

// lokiQueryRangeResponse is the subset of Loki's /loki/api/v1/query_range
// response this tool needs -- every stream's label set plus its
// [nanosecond-timestamp, line] value pairs.
type lokiQueryRangeResponse struct {
	Data struct {
		Result []struct {
			Values [][2]string `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// analyzeLogPatternsMaxPatterns caps how many distinct patterns are
// returned to the LLM -- a query matching thousands of genuinely distinct
// messages would otherwise blow the same token budget this tool exists to
// avoid; the truncation itself is reported rather than silently applied.
const analyzeLogPatternsMaxPatterns = 20

// analyzeLogPatterns groups Loki log lines by their normalized pattern
// instead of returning them raw -- a query matching hundreds of near-
// identical lines (same message, different UUID/timestamp/IP) otherwise
// dumps all of them into the LLM's context with no way to see the
// underlying pattern without re-deriving it token by token.
func (te *ToolExecutor) analyzeLogPatterns(ctx context.Context, arguments string) (string, error) {
	var args AnalyzeLogPatternsArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse analyze_log_patterns args: %w", err)
	}
	if args.Selector == "" {
		return "", fmt.Errorf("selector is required")
	}

	dsUID, err := te.resolveDatasourceUID(ctx, "loki", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find loki datasource: %w", err)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}

	params := url.Values{}
	params.Set("query", args.Selector)
	now := time.Now()
	if args.Start == "" {
		params.Set("start", fmt.Sprintf("%d", now.Add(-1*time.Hour).UnixNano()))
	} else {
		params.Set("start", resolveTimeNano(args.Start, now))
	}
	if args.End == "" {
		params.Set("end", fmt.Sprintf("%d", now.UnixNano()))
	} else {
		params.Set("end", resolveTimeNano(args.End, now))
	}
	params.Set("limit", fmt.Sprintf("%d", limit))

	apiPath := fmt.Sprintf("/api/datasources/proxy/uid/%s/loki/api/v1/query_range?%s", url.PathEscape(dsUID), params.Encode())
	body, err := te.doGrafanaRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", err
	}

	var resp lokiQueryRangeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", fmt.Errorf("parse loki response: %w", err)
	}

	groups := make(map[string]*logPattern)
	var order []string
	totalLines := 0
	for _, stream := range resp.Data.Result {
		for _, v := range stream.Values {
			totalLines++
			tsNano, line := v[0], v[1]
			key := normalizeLogLine(line)
			g, ok := groups[key]
			if !ok {
				g = &logPattern{Pattern: key, Example: line, FirstSeen: tsNano, LastSeen: tsNano}
				groups[key] = g
				order = append(order, key)
			}
			g.Count++
			// Loki returns values newest-first within each stream, but
			// streams themselves aren't globally ordered relative to each
			// other -- compare numerically (nanosecond epoch strings can
			// differ in length, e.g. across a rollover, so a plain string
			// comparison isn't reliable) rather than assuming any incoming
			// order.
			if nanoLess(tsNano, g.FirstSeen) {
				g.FirstSeen = tsNano
			}
			if nanoLess(g.LastSeen, tsNano) {
				g.LastSeen = tsNano
			}
		}
	}

	patterns := make([]*logPattern, 0, len(order))
	for _, key := range order {
		patterns = append(patterns, groups[key])
	}
	sort.Slice(patterns, func(i, j int) bool { return patterns[i].Count > patterns[j].Count })

	truncated := false
	if len(patterns) > analyzeLogPatternsMaxPatterns {
		patterns = patterns[:analyzeLogPatternsMaxPatterns]
		truncated = true
	}

	result := logPatternsResult{
		Selector:      args.Selector,
		LinesScanned:  totalLines,
		PatternCount:  len(order),
		Patterns:      patterns,
		Truncated:     truncated,
		TruncatedNote: truncationNote(truncated, len(order), analyzeLogPatternsMaxPatterns),
	}
	if truncated {
		result.IsPartial = true
		result.Warnings = append(result.Warnings, result.TruncatedNote)
	}
	if totalLines == 0 {
		result.Summary = fmt.Sprintf("no log lines matched selector %q in this window", args.Selector)
	} else {
		result.Summary = fmt.Sprintf("%d lines scanned, %d distinct pattern(s) found", totalLines, len(order))
	}
	result.Sources = []string{datasourceSource("loki", args.DatasourceUID)}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}

// nanoLess compares two nanosecond-epoch timestamp strings numerically.
// Falls back to string comparison on parse failure (defensive only --
// Loki's own timestamps are always well-formed digit strings).
func nanoLess(a, b string) bool {
	an, aErr := strconv.ParseInt(a, 10, 64)
	bn, bErr := strconv.ParseInt(b, 10, 64)
	if aErr != nil || bErr != nil {
		return a < b
	}
	return an < bn
}

func truncationNote(truncated bool, total, shown int) string {
	if !truncated {
		return ""
	}
	return fmt.Sprintf("%d distinct patterns found, showing the top %d by count", total, shown)
}
