package plugin

// ToolResult is the common envelope an analysis tool's JSON result embeds
// (via Go struct embedding) alongside its own specific fields -- adds a
// consistent Summary/Warnings/IsPartial/Sources/TimeRange/Links surface
// across every tool without discarding any tool's existing, specific
// fields. Embedding rather than replacing keeps every already-live-tested
// tool's JSON backward compatible: existing fields stay exactly where they
// were, this just adds a few more keys alongside them.
//
// Only tools that produce a genuine analysis result (not a raw
// pass-through of another API's response, e.g. query_prometheus/
// query_loki/query_tempo/list_datasources/list_dashboards) embed this --
// wrapping a raw upstream JSON body would lose fidelity without adding
// real value, since the model already benefits from seeing exactly what
// Grafana/Prometheus/Loki returned for those.
type ToolResult struct {
	// Summary is a short, one-or-two-sentence human-readable takeaway --
	// set only when a tool has something worth saying beyond its own
	// structured fields (e.g. "no kube-state-metrics data found for this
	// workload in this environment").
	Summary string `json:"summary,omitempty"`
	// Warnings lists caveats specific to THIS result: partial data, a
	// missing prerequisite, truncated evidence -- anything that should
	// change how much weight the model gives the rest of the payload.
	// Never the tool's generic, always-present disclaimer text (e.g. "this
	// metric may not be scraped everywhere") -- only when that disclaimer
	// actually applied this call.
	Warnings []string `json:"warnings,omitempty"`
	// IsPartial is true when this result is missing evidence it would
	// normally include (a datasource was unavailable, a cap was hit) -- the
	// model should say so rather than treating the result as complete.
	IsPartial bool `json:"is_partial,omitempty"`
	// TimeRange is the effective start/end actually queried, once relative
	// times ("now-1h") are resolved to something concrete -- omitted when a
	// tool has no single time window (e.g. it queries an instant value).
	TimeRange string `json:"time_range,omitempty"`
	// Sources names where this evidence came from (datasource type/UID,
	// names of tools called internally) -- lets the model, and a human
	// reading the transcript, trace what backed a claim.
	Sources []string `json:"sources,omitempty"`
	// Links are URLs a human could open to see the same evidence directly
	// (e.g. a Grafana Explore link) -- omitted entirely when a tool has no
	// real way to construct one; never a guessed/templated URL.
	Links []string `json:"links,omitempty"`
}

// datasourceSource formats one Sources entry for a resolved datasource --
// distinguishes an explicit datasource_uid from this plugin's own
// auto-resolution so a reader can tell whether the caller pinned a specific
// cluster/tenant or let the plugin pick the (only) one it found.
func datasourceSource(dsType, providedUID string) string {
	if providedUID != "" {
		return dsType + " (datasource_uid=" + providedUID + ")"
	}
	return dsType + " (auto-resolved)"
}
