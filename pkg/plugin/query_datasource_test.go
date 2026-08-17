package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

func TestValidateReadOnlySQL_AllowsPlainSelect(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"SELECT service, version FROM deployments LIMIT 10",
		"select * from deployments",
		"WITH recent AS (SELECT * FROM deployments) SELECT * FROM recent",
		"SELECT * FROM deployments;", // trailing semicolon is fine
	} {
		if err := validateReadOnlySQL(q); err != nil {
			t.Errorf("query %q: unexpected error: %v", q, err)
		}
	}
}

func TestValidateReadOnlySQL_RejectsWrites(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"INSERT INTO deployments (service) VALUES ('x')",
		"UPDATE deployments SET status = 'failed'",
		"DELETE FROM deployments",
		"DROP TABLE deployments",
		"TRUNCATE deployments",
		"ALTER TABLE deployments ADD COLUMN x TEXT",
		"CREATE TABLE evil (id int)",
		"GRANT ALL ON deployments TO public",
		"SELECT * INTO new_table FROM deployments",
	} {
		if err := validateReadOnlySQL(q); err == nil {
			t.Errorf("query %q: expected an error, got none", q)
		}
	}
}

func TestValidateReadOnlySQL_RejectsMultiStatement(t *testing.T) {
	t.Parallel()
	if err := validateReadOnlySQL("SELECT 1; DROP TABLE deployments"); err == nil {
		t.Error("expected an error for a stacked second statement")
	}
	if err := validateReadOnlySQL("SELECT 1; SELECT 2"); err == nil {
		t.Error("expected an error even when the second statement is itself a SELECT -- no stacking allowed at all")
	}
}

func TestValidateReadOnlySQL_RejectsNonSelectStart(t *testing.T) {
	t.Parallel()
	if err := validateReadOnlySQL("EXPLAIN SELECT * FROM deployments"); err == nil {
		t.Error("expected an error for a non-SELECT/WITH statement")
	}
}

func TestValidateReadOnlySQL_RejectsEmptyOrOverlong(t *testing.T) {
	t.Parallel()
	if err := validateReadOnlySQL(""); err == nil {
		t.Error("expected an error for an empty query")
	}
	if err := validateReadOnlySQL("SELECT '" + strings.Repeat("a", maxQueryDatasourceQueryChars) + "'"); err == nil {
		t.Error("expected an error for an overlong query")
	}
}

// sqlDsQueryMock serves a fixed /api/ds/query response body -- shape
// captured live from a real Grafana instance querying its Postgres
// datasource, not guessed.
func sqlDsQueryMock(t *testing.T, dsType string, columns []string, rows [][]any, queryErr string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/datasources" {
			_, _ = w.Write([]byte(`[{"type":"` + dsType + `","uid":"pg-uid"}]`))
			return
		}
		if queryErr != "" {
			resp := map[string]any{"results": map[string]any{"A": map[string]any{"status": 500, "error": queryErr}}}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		fields := make([]map[string]any, len(columns))
		for i, c := range columns {
			fields[i] = map[string]any{"name": c}
		}
		// Transpose rows (row-major, as callers naturally write them in a
		// test) into the columnar "values" shape Grafana actually returns.
		values := make([][]any, len(columns))
		for c := range columns {
			for _, row := range rows {
				values[c] = append(values[c], row[c])
			}
		}
		resp := map[string]any{
			"results": map[string]any{
				"A": map[string]any{
					"status": 200,
					"frames": []map[string]any{
						{
							"schema": map[string]any{"fields": fields},
							"data":   map[string]any{"values": values},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestQueryDatasource_ReturnsRowsFromRealShapedResponse(t *testing.T) {
	t.Parallel()

	server := sqlDsQueryMock(t, "grafana-postgresql-datasource",
		[]string{"service", "version", "status"},
		[][]any{
			{"demo-app", "v1.5.1", "success"},
			{"demo-app", "v1.5.0", "success"},
		}, "")
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.queryDatasource(context.Background(), `{"query":"SELECT service, version, status FROM deployments ORDER BY deployed_at DESC LIMIT 2"}`)
	if err != nil {
		t.Fatalf("queryDatasource failed: %v", err)
	}

	var parsed queryDatasourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if len(parsed.Columns) != 3 {
		t.Errorf("columns = %v, want 3", parsed.Columns)
	}
	if parsed.RowCount != 2 {
		t.Fatalf("row_count = %d, want 2", parsed.RowCount)
	}
	if parsed.Rows[0][0] != "demo-app" || parsed.Rows[0][1] != "v1.5.1" {
		t.Errorf("rows[0] = %v, want [demo-app v1.5.1 success]", parsed.Rows[0])
	}
	if parsed.Truncated {
		t.Error("truncated = true, want false (fewer rows than max_rows)")
	}
}

func TestQueryDatasource_TruncatesAtMaxRows(t *testing.T) {
	t.Parallel()

	rows := make([][]any, 5)
	for i := range rows {
		rows[i] = []any{"demo-app", "v1.0." + string(rune('0'+i))}
	}
	server := sqlDsQueryMock(t, "grafana-postgresql-datasource", []string{"service", "version"}, rows, "")
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	result, err := te.queryDatasource(context.Background(), `{"query":"SELECT service, version FROM deployments","max_rows":2}`)
	if err != nil {
		t.Fatalf("queryDatasource failed: %v", err)
	}

	var parsed queryDatasourceResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
	}
	if parsed.RowCount != 2 {
		t.Fatalf("row_count = %d, want 2 (capped by max_rows)", parsed.RowCount)
	}
	if !parsed.Truncated {
		t.Error("truncated = false, want true (5 rows returned, capped to 2)")
	}
	if !parsed.IsPartial {
		t.Error("is_partial = false, want true when truncated")
	}
}

func TestQueryDatasource_RejectsUnsupportedDatasourceType(t *testing.T) {
	t.Parallel()

	server := sqlDsQueryMock(t, "mysql", []string{"a"}, [][]any{{"1"}}, "")
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	// No datasource_uid given: auto-resolve-by-type finds zero
	// "grafana-postgresql-datasource" datasources (the only one is mysql)
	// and fails before ever reaching the allowlist check.
	if _, err := te.queryDatasource(context.Background(), `{"query":"SELECT 1"}`); err == nil {
		t.Error("expected an error when no postgres-type datasource exists")
	}

	// An EXPLICIT datasource_uid bypasses the type-filtered auto-resolve
	// entirely (see resolveDatasourceUID) -- this is what actually
	// exercises the allowlist check itself, rejecting a wrong-type
	// datasource even when explicitly targeted by UID.
	_, err := te.queryDatasource(context.Background(), `{"query":"SELECT 1","datasource_uid":"pg-uid"}`)
	if err == nil {
		t.Fatal("expected an error for an explicitly-targeted datasource type not in the allowlist")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %v, want it to explain the type isn't supported", err)
	}
}

func TestQueryDatasource_RejectsWriteQueryBeforeEverCallingGrafana(t *testing.T) {
	t.Parallel()

	// No httptest server at all -- if the tool tried to reach Grafana before
	// validating the query, this would fail with a connection error instead
	// of the intended validation error.
	te := NewToolExecutor("http://127.0.0.1:1", log.DefaultLogger)
	_, err := te.queryDatasource(context.Background(), `{"query":"DROP TABLE deployments"}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Error("query validation should reject before any network call is attempted")
	}
}

func TestQueryDatasource_SurfacesDownstreamQueryError(t *testing.T) {
	t.Parallel()

	server := sqlDsQueryMock(t, "grafana-postgresql-datasource", nil, nil, `pq: relation "nope" does not exist`)
	defer server.Close()

	te := NewToolExecutor(server.URL, log.DefaultLogger)
	_, err := te.queryDatasource(context.Background(), `{"query":"SELECT * FROM nope"}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to surface the real DB error", err)
	}
}
