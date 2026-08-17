package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// SECURITY NOTE (this tool was explicitly flagged in its own roadmap as the
// highest security-risk item on the entire list, deserving dedicated review
// before shipping -- not a formality, an actual constraint on what this
// implementation is allowed to do):
//
// This is a generic SQL-query-execution tool handed to an LLM. The defenses
// below (statement-shape validation, keyword blocklist, datasource-type
// allowlist, row cap) are real and meaningfully reduce risk, but they are
// application-layer, best-effort checks on query TEXT -- they are not a
// database-enforced guarantee, and a sufficiently adversarial query could
// still find a gap a text-level blocklist can't close (e.g. a read-only-
// looking SELECT that still calls a mutating function, materialized view
// refresh functions, dblink, etc.). The actual, non-bypassable guarantee
// MUST come from the datasource's own configured database credentials
// being genuinely read-only (a Postgres role granted only SELECT, never
// this tool's problem to enforce) -- deploying this tool against a
// datasource whose credentials can write is deploying a write-capable tool
// regardless of what this file does. This is documented, not silently
// assumed safe.
//
// Scope: only the "postgres" family is supported today (this plugin's own
// local-stack validation only stood up Postgres) -- Elasticsearch/InfluxDB
// support was explicitly named in the original suggestion but is NOT
// implemented here; each would need its own query-shape validation (SQL's
// keyword blocklist below doesn't transfer to Elasticsearch's Query DSL or
// InfluxQL/Flux) and its own dedicated review before being added, not a
// copy-paste extension of this file.
var queryDatasourceAllowedTypes = map[string]bool{
	"postgres":                      true,
	"grafana-postgresql-datasource": true,
}

// queryDatasourceBlockedKeywords are matched as whole words (case-
// insensitive) anywhere in the query text. Deliberately broad (covers
// non-Postgres dialects' equivalents too, e.g. PRAGMA/ATTACH for SQLite)
// since this same blocklist is meant to keep working if this tool is ever
// extended to another SQL-shaped datasource type -- an incomplete list here
// is a real gap, not a style choice.
var queryDatasourceBlockedKeywords = []string{
	"insert", "update", "delete", "drop", "alter", "truncate", "grant", "revoke",
	"create", "copy", "call", "exec", "execute", "vacuum", "reindex", "lock",
	"listen", "notify", "set", "reset", "do", "merge", "replace", "attach",
	"detach", "pragma", "into", "refresh", "cluster", "comment",
}

var queryDatasourceKeywordPattern = regexp.MustCompile(`(?i)\b(` + strings.Join(queryDatasourceBlockedKeywords, "|") + `)\b`)

// QueryDatasourceArgs holds parsed arguments for query_datasource.
type QueryDatasourceArgs struct {
	Query string `json:"query"`
	// MaxRows caps how many rows are returned -- default
	// defaultQueryDatasourceMaxRows, hard-capped at
	// maxQueryDatasourceMaxRows regardless of what's requested.
	MaxRows       int    `json:"max_rows,omitempty"`
	DatasourceUID string `json:"datasource_uid,omitempty"`
}

type queryDatasourceResult struct {
	ToolResult
	Query     string   `json:"query"`
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

const (
	defaultQueryDatasourceMaxRows = 200
	maxQueryDatasourceMaxRows     = 1000
	maxQueryDatasourceQueryChars  = 4000
)

// validateReadOnlySQL rejects anything that isn't shaped like a single,
// plain read query -- see the SECURITY NOTE above for what this can and
// cannot guarantee.
func validateReadOnlySQL(query string) error {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return fmt.Errorf("query is required")
	}
	if len(trimmed) > maxQueryDatasourceQueryChars {
		return fmt.Errorf("query is too long (%d chars, max %d)", len(trimmed), maxQueryDatasourceQueryChars)
	}

	// Multi-statement stacking guard: a trailing semicolon is fine, one
	// anywhere else means a second statement was appended.
	bareQuery := strings.TrimSuffix(trimmed, ";")
	if strings.Contains(bareQuery, ";") {
		return fmt.Errorf("only a single statement is allowed -- remove the semicolon(s) separating multiple statements")
	}

	lower := strings.ToLower(bareQuery)
	if !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with") {
		return fmt.Errorf("only SELECT (or a WITH ... SELECT common table expression) is allowed -- this tool never writes")
	}

	if m := queryDatasourceKeywordPattern.FindString(bareQuery); m != "" {
		return fmt.Errorf("query contains a disallowed keyword (%q) -- this tool only permits plain read queries", m)
	}

	return nil
}

type sqlDsQueryBody struct {
	Queries []sqlDsQuery `json:"queries"`
	From    string       `json:"from"`
	To      string       `json:"to"`
}

type sqlDsQuery struct {
	RefID      string          `json:"refId"`
	Datasource cloudwatchDsRef `json:"datasource"`
	RawSQL     string          `json:"rawSql"`
	Format     string          `json:"format"`
}

type sqlDsQueryResponse struct {
	Results map[string]struct {
		Status int    `json:"status"`
		Error  string `json:"error,omitempty"`
		Frames []struct {
			Schema struct {
				Fields []struct {
					Name string `json:"name"`
				} `json:"fields"`
			} `json:"schema"`
			Data struct {
				Values [][]any `json:"values"`
			} `json:"data"`
		} `json:"frames"`
	} `json:"results"`
}

// queryDatasource runs a read-only SQL query against an allowlisted
// (currently: Postgres) datasource via Grafana's own /api/ds/query --
// see the SECURITY NOTE above before extending this tool's scope.
func (te *ToolExecutor) queryDatasource(ctx context.Context, arguments string) (string, error) {
	var args QueryDatasourceArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse query_datasource args: %w", err)
	}
	if err := validateReadOnlySQL(args.Query); err != nil {
		return "", err
	}

	// "grafana-postgresql-datasource" is the real type string Grafana's own
	// Postgres datasource reports (verified live) -- the plain "postgres"
	// name is kept in queryDatasourceAllowedTypes only in case an older
	// Grafana version's built-in datasource still uses it.
	dsUID, err := te.resolveDatasourceUID(ctx, "grafana-postgresql-datasource", args.DatasourceUID)
	if err != nil {
		return "", fmt.Errorf("find postgres datasource: %w", err)
	}
	dsType, err := te.datasourceType(ctx, dsUID)
	if err != nil {
		return "", fmt.Errorf("determine datasource type: %w", err)
	}
	if !queryDatasourceAllowedTypes[dsType] {
		return "", fmt.Errorf("datasource type %q is not supported by query_datasource (only Postgres today)", dsType)
	}

	maxRows := args.MaxRows
	if maxRows <= 0 {
		maxRows = defaultQueryDatasourceMaxRows
	}
	if maxRows > maxQueryDatasourceMaxRows {
		maxRows = maxQueryDatasourceMaxRows
	}

	reqBody := sqlDsQueryBody{
		Queries: []sqlDsQuery{{
			RefID:      "A",
			Datasource: cloudwatchDsRef{Type: dsType, UID: dsUID},
			RawSQL:     strings.TrimSuffix(strings.TrimSpace(args.Query), ";"),
			Format:     "table",
		}},
		From: "now-6h",
		To:   "now",
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal ds/query request: %w", err)
	}

	respBody, err := te.doGrafanaRequest(ctx, http.MethodPost, "/api/ds/query", bytes.NewReader(bodyJSON))
	if err != nil {
		return "", fmt.Errorf("query datasource: %w", err)
	}

	var parsed sqlDsQueryResponse
	if err := json.Unmarshal([]byte(respBody), &parsed); err != nil {
		return "", fmt.Errorf("parse ds/query response: %w", err)
	}
	queryResult, ok := parsed.Results["A"]
	if !ok {
		return "", fmt.Errorf("no result returned for this query")
	}
	if queryResult.Error != "" {
		return "", fmt.Errorf("query failed: %s", queryResult.Error)
	}

	result := queryDatasourceResult{Query: args.Query}
	if len(queryResult.Frames) == 0 {
		result.Summary = "query returned no data"
		result.Sources = []string{datasourceSource("postgres", args.DatasourceUID)}
		out, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return truncateString(string(out), 50000), nil
	}

	frame := queryResult.Frames[0]
	for _, f := range frame.Schema.Fields {
		result.Columns = append(result.Columns, f.Name)
	}

	// frame.Data.Values is columnar (one array per field, in field order) --
	// transpose to row-major, which is what a caller reading "one row at a
	// time" actually wants.
	numCols := len(frame.Data.Values)
	numRows := 0
	if numCols > 0 {
		numRows = len(frame.Data.Values[0])
	}
	total := numRows
	if numRows > maxRows {
		numRows = maxRows
	}
	for r := 0; r < numRows; r++ {
		row := make([]any, numCols)
		for c := 0; c < numCols; c++ {
			if r < len(frame.Data.Values[c]) {
				row[c] = frame.Data.Values[c][r]
			}
		}
		result.Rows = append(result.Rows, row)
	}
	result.RowCount = len(result.Rows)

	if total > maxRows {
		result.Truncated = true
		result.IsPartial = true
		result.Warnings = append(result.Warnings, fmt.Sprintf("truncated to the first %d of %d row(s) -- narrow the query (add a WHERE/LIMIT) instead of relying on this cap", maxRows, total))
	}
	result.Summary = fmt.Sprintf("%d row(s), %d column(s)", result.RowCount, numCols)
	result.Sources = []string{datasourceSource("postgres", args.DatasourceUID)}

	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return truncateString(string(out), 50000), nil
}
