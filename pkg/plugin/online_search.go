package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	openai "github.com/sashabaranov/go-openai"
	"golang.org/x/time/rate"
)

const (
	defaultOnlineSearchMaxResults     = 5
	maxOnlineSearchMaxResults         = 8
	defaultOnlineSearchTimeoutSeconds = 6
	maxOnlineSearchTimeoutSeconds     = 15

	maxOnlineSearchQueryChars           = 320
	maxOnlineSearchQueryWords           = 40
	maxOnlineSearchTitleChars           = 160
	maxOnlineSearchSnippetChars         = 320
	minOnlineSearchRelevanceScore       = 2
	maxOnlineSearchCallsPerTurn         = 2
	maxOnlineSearchGatewayResults       = 10
	maxOnlineSearchGatewayResponseBytes = 256 * 1024
	maxOnlineSearchToolOutputBytes      = 24 * 1024
	internetHealthTimeout               = 1500 * time.Millisecond
	internetHealthTTL                   = 60 * time.Second
)

// onlineSearchToolName is the single source of truth for the internet-search
// tool's name -- the function/tool definition, the ToolExecutor dispatch
// case, the pseudo-tool-call budget gate, and the streaming UI metadata
// (newToolCallInfo) all compare against this exact string, so renaming the
// tool is a one-line change instead of a multi-file find-and-replace.
const onlineSearchToolName = "search_web"

type OnlineSearchArgs struct {
	Query                string `json:"query"`
	Reason               string `json:"reason"`
	Intent               string `json:"intent,omitempty"`
	PreferredFormat      string `json:"preferred_format,omitempty"`
	ContinuationApproved bool   `json:"continuation_approved,omitempty"`
	MaxResults           int    `json:"max_results,omitempty"`
}

type OnlineSearchBackend string

const (
	OnlineSearchBackendGateway OnlineSearchBackend = "gateway"
	// OnlineSearchBackendSearxng is the built-in direct provider: a
	// self-hosted SearXNG metasearch instance (open source, no API key/
	// account/billing needed -- the admin only points at their own
	// instance's URL). DuckDuckGo's Instant Answer API is used as an
	// automatic, best-effort fallback when SearXNG itself is unavailable
	// (see fetchSearchResults) -- it is NOT a general web-search API (mostly
	// infobox/abstract results), so it only ever adds a result on top of, or
	// instead of, an empty/failed SearXNG response, never replaces SearXNG
	// as the primary source.
	OnlineSearchBackendSearxng OnlineSearchBackend = "searxng"
	// OnlineSearchBackendDuckDuckGo is the zero-config default: no admin setup,
	// no container/service to run, no API key. It calls DuckDuckGo's public
	// Instant Answer API directly -- a real network call, but honestly limited:
	// that API only returns a short encyclopedia-style definition for a known
	// product/entity term, never a general list of web-search results. There is
	// no free, ToS-compliant way to get real DuckDuckGo search results without
	// a key (their HTML/Lite endpoints actively block non-browser requests
	// with an anti-bot challenge). See duckDuckGoCandidateQueries for the
	// retry logic that raises the hit rate for compound technical queries by
	// falling back to the known entity term inside them (e.g. "Grafana").
	// When an admin configures a real SearXNG instance or search gateway,
	// OnlineSearchBackend switches away from this value and full,
	// non-limited search results take over -- this backend never mixes with
	// or degrades those.
	OnlineSearchBackendDuckDuckGo OnlineSearchBackend = "duckduckgo"
)

type OnlineSearchClient struct {
	backend      OnlineSearchBackend
	gatewayURL   string
	gatewayToken string
	searxngURL   string
	httpClient   *http.Client
	maxResults   int
	limiter      *rate.Limiter
	logger       log.Logger

	healthMu         sync.Mutex
	healthOK         bool
	healthCheckedAt  time.Time
	healthRefreshing bool
}

type InternetToolState string

const (
	InternetToolsDisabled                 InternetToolState = "disabled"
	InternetToolsEnabledNoConfiguredTools InternetToolState = "enabled_no_configured_tools"
	InternetToolsEnabledButUnavailable    InternetToolState = "enabled_but_unavailable"
	InternetToolsEnabledWithSearch        InternetToolState = "enabled_with_search"
)

type webSearchScope struct {
	Host                         string
	PathPrefixes                 []string
	DenyPathParts                []string
	SnippetAllowed               bool
	SourceKind                   string
	TrustTier                    int
	TrustLabel                   string
	Official                     bool
	RequiresStrongGrafanaContext bool
}

// Scope gate: a query must be about Grafana/observability. Kubernetes terms
// alone are NOT enough; "kubectl delete pod" must not pass this tool. K8s is
// accepted only when paired with a strong Grafana/observability term such as
// Grafana, Prometheus, Loki, Tempo, OpenTelemetry, alerting, or
// kube-state-metrics. Generic "logs"/"metrics" alone are deliberately not
// enough for Kubernetes queries.
var grafanaSearchTopicPattern = regexp.MustCompile(`(?i)\b(grafana|dashboard|dashboards|painel|panel|datasource|data source|fonte de dados|plugin|plugins|grafana cloud|prometheus|promql|mimir|loki|logql|tempo|trace|traces|metric|metrics|metrica|metricas|logs|alert|alerta|alerting|alertmanager|alloy|k6|opentelemetry|otel|pyroscope|cloudwatch|azure monitor|google cloud monitoring|slo|observability|observabilidade|kube-state-metrics|elasticsearch|elastic|opensearch|influxdb|graphite|mysql|postgres|postgresql|sql server|mssql|clickhouse|snowflake|mongodb|oracle|jaeger|zipkin|datadog|new relic|victoriametrics|zabbix|splunk|redis|druid|bigquery|timestream)\b`)
var strongGrafanaSearchTopicPattern = regexp.MustCompile(`(?i)\b(grafana|dashboard|dashboards|painel|panel|datasource|data source|fonte de dados|plugin|plugins|grafana cloud|prometheus|promql|mimir|loki|logql|tempo|trace|traces|alertmanager|alloy|k6|opentelemetry|otel|pyroscope|cloudwatch|azure monitor|google cloud monitoring|slo|observability|observabilidade|kube-state-metrics|elasticsearch|elastic|opensearch|influxdb|graphite|mysql|postgres|postgresql|sql server|mssql|clickhouse|snowflake|mongodb|oracle|jaeger|zipkin|datadog|new relic|victoriametrics|zabbix|splunk|redis|druid|bigquery|timestream)\b`)
var kubernetesTopicPattern = regexp.MustCompile(`(?i)\b(kubernetes|k8s|pod|pods|node|nodes|deployment|daemonset|statefulset|kubectl)\b`)
var blockedOnlineSearchContentPattern = regexp.MustCompile(`(?i)\b(porn|pornography|xxx|adult content|escort|casino|gambling|betting|ad[s]?|sponsored|coupon|malware|virus|ransomware|trojan|phishing|credential theft|steal token|bypass|jailbreak|prompt injection|ignore previous instructions|drug trafficking|illegal drugs|weapon|explosive|terrorism|child abuse|csam|piracy|crack|keygen)\b`)
var blockedDownloadPathPattern = regexp.MustCompile(`(?i)\.(zip|tar|tgz|gz|bz2|xz|7z|rar|exe|msi|dmg|pkg|deb|rpm|apk|jar|war|dll|so|bin|iso|pdf|json|yaml|yml|csv|tsv)(?:$|[?#])`)
var htmlTagPattern = regexp.MustCompile(`<[^>]+>`)
var whitespacePattern = regexp.MustCompile(`\s+`)

var defaultGrafanaSearchScopes = []webSearchScope{
	{Host: "grafana.com", PathPrefixes: []string{"/docs/", "/developers/plugin-tools/", "/grafana/dashboards/", "/grafana/plugins/"}, SnippetAllowed: true, SourceKind: "official_grafana", TrustTier: 100, TrustLabel: "Official Grafana", Official: true},
	{Host: "prometheus.io", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_upstream", TrustTier: 90, TrustLabel: "Official Prometheus", Official: true},
	{Host: "opentelemetry.io", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_upstream", TrustTier: 90, TrustLabel: "Official OpenTelemetry", Official: true},
	{Host: "kubernetes.io", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_upstream", TrustTier: 90, TrustLabel: "Official Kubernetes", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.aws.amazon.com", PathPrefixes: []string{"/grafana/", "/prometheus/", "/AmazonCloudWatch/latest/monitoring/", "/timestream/"}, SnippetAllowed: true, SourceKind: "official_vendor", TrustTier: 90, TrustLabel: "Official AWS docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "learn.microsoft.com", PathPrefixes: []string{"/azure/managed-grafana/", "/azure/azure-monitor/"}, SnippetAllowed: true, SourceKind: "official_vendor", TrustTier: 90, TrustLabel: "Official Microsoft Learn", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "cloud.google.com", PathPrefixes: []string{"/monitoring/", "/stackdriver/docs/", "/managed-service-prometheus/", "/bigquery/docs/"}, SnippetAllowed: true, SourceKind: "official_vendor", TrustTier: 90, TrustLabel: "Official Google Cloud docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "www.elastic.co", PathPrefixes: []string{"/guide/en/elasticsearch/", "/guide/en/logstash/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Elastic docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "opensearch.org", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official OpenSearch docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.influxdata.com", PathPrefixes: []string{"/influxdb/", "/influxdb3/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official InfluxData docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "graphite.readthedocs.io", PathPrefixes: []string{"/en/latest/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Graphite docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "dev.mysql.com", PathPrefixes: []string{"/doc/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official MySQL docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "www.postgresql.org", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official PostgreSQL docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "learn.microsoft.com", PathPrefixes: []string{"/sql/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official SQL Server docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "clickhouse.com", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official ClickHouse docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.snowflake.com", PathPrefixes: []string{"/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Snowflake docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "www.mongodb.com", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official MongoDB docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.oracle.com", PathPrefixes: []string{"/database/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Oracle docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "jaegertracing.io", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Jaeger docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "zipkin.io", PathPrefixes: []string{"/pages/documentation", "/zipkin/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Zipkin docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.datadoghq.com", PathPrefixes: []string{"/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Datadog docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.newrelic.com", PathPrefixes: []string{"/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official New Relic docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.victoriametrics.com", PathPrefixes: []string{"/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official VictoriaMetrics docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "www.zabbix.com", PathPrefixes: []string{"/documentation/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Zabbix docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "docs.splunk.com", PathPrefixes: []string{"/Documentation/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Splunk docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "redis.io", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Redis docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "druid.apache.org", PathPrefixes: []string{"/docs/"}, SnippetAllowed: true, SourceKind: "official_datasource", TrustTier: 85, TrustLabel: "Official Apache Druid docs", Official: true, RequiresStrongGrafanaContext: true},
	{Host: "github.com", PathPrefixes: []string{"/grafana/", "/grafana-plugins/"}, DenyPathParts: []string{"/issues/", "/pull/", "/discussions/", "/commit/"}, SnippetAllowed: false, SourceKind: "official_code", TrustTier: 75, TrustLabel: "Grafana GitHub repository", Official: true},
	{Host: "community.grafana.com", PathPrefixes: []string{"/"}, SnippetAllowed: false, SourceKind: "known_community", TrustTier: 55, TrustLabel: "Grafana community forum", Official: false, RequiresStrongGrafanaContext: true},
	{Host: "stackoverflow.com", PathPrefixes: []string{"/questions/"}, SnippetAllowed: false, SourceKind: "known_community", TrustTier: 55, TrustLabel: "Stack Overflow community", Official: false, RequiresStrongGrafanaContext: true},
	{Host: "en.wikipedia.org", PathPrefixes: []string{"/wiki/"}, SnippetAllowed: true, SourceKind: "encyclopedia", TrustTier: 50, TrustLabel: "Wikipedia encyclopedia", Official: false, RequiresStrongGrafanaContext: true},
	{Host: "pt.wikipedia.org", PathPrefixes: []string{"/wiki/"}, SnippetAllowed: true, SourceKind: "encyclopedia", TrustTier: 50, TrustLabel: "Wikipedia encyclopedia", Official: false, RequiresStrongGrafanaContext: true},
	{Host: "reddit.com", PathPrefixes: []string{"/r/grafana/", "/r/PrometheusMonitoring/", "/r/devops/", "/r/sre/"}, SnippetAllowed: false, SourceKind: "low_weight_community", TrustTier: 35, TrustLabel: "Reddit community signal", Official: false, RequiresStrongGrafanaContext: true},
	{Host: "www.reddit.com", PathPrefixes: []string{"/r/grafana/", "/r/PrometheusMonitoring/", "/r/devops/", "/r/sre/"}, SnippetAllowed: false, SourceKind: "low_weight_community", TrustTier: 35, TrustLabel: "Reddit community signal", Official: false, RequiresStrongGrafanaContext: true},
	{Host: "old.reddit.com", PathPrefixes: []string{"/r/grafana/", "/r/PrometheusMonitoring/", "/r/devops/", "/r/sre/"}, SnippetAllowed: false, SourceKind: "low_weight_community", TrustTier: 35, TrustLabel: "Reddit community signal", Official: false, RequiresStrongGrafanaContext: true},
}

func NewOnlineSearchClient(backend OnlineSearchBackend, gatewayURL string, gatewayToken string, searxngURL string, maxResults int, timeoutSeconds int, logger log.Logger) (*OnlineSearchClient, error) {
	if maxResults <= 0 {
		maxResults = defaultOnlineSearchMaxResults
	}
	if maxResults > maxOnlineSearchMaxResults {
		maxResults = maxOnlineSearchMaxResults
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultOnlineSearchTimeoutSeconds
	}
	if timeoutSeconds > maxOnlineSearchTimeoutSeconds {
		timeoutSeconds = maxOnlineSearchTimeoutSeconds
	}

	backend = normalizeOnlineSearchBackend(backend)
	normalizedGatewayURL := ""
	if backend == OnlineSearchBackendGateway {
		var err error
		normalizedGatewayURL, err = normalizeExternalSearchURL(gatewayURL)
		if err != nil {
			return nil, fmt.Errorf("search gateway URL: %w", err)
		}
	}
	normalizedSearxngURL := ""
	if backend == OnlineSearchBackendSearxng {
		var err error
		normalizedSearxngURL, err = normalizeExternalSearchURL(searxngURL)
		if err != nil {
			return nil, fmt.Errorf("SearXNG instance URL: %w", err)
		}
	}

	return &OnlineSearchClient{
		backend:      backend,
		gatewayURL:   normalizedGatewayURL,
		gatewayToken: strings.TrimSpace(gatewayToken),
		searxngURL:   normalizedSearxngURL,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResults: maxResults,
		limiter:    rate.NewLimiter(rate.Every(2*time.Second), 2),
		logger:     logger,
	}, nil
}

func normalizeOnlineSearchBackend(backend OnlineSearchBackend) OnlineSearchBackend {
	switch backend {
	case OnlineSearchBackendGateway:
		return OnlineSearchBackendGateway
	case OnlineSearchBackendSearxng:
		return OnlineSearchBackendSearxng
	default:
		return OnlineSearchBackendDuckDuckGo
	}
}

// isPrivateOrLoopbackHost reports whether host (already lowercased hostname,
// no port) is loopback, a well-known private-network literal, or a bare
// single-label hostname (e.g. a Docker/Compose/K8s service name like
// "searxng") -- self-hosted SearXNG is extremely commonly run on an internal
// network the admin already trusts, without a public TLS certificate. https
// is still required for any other host (the on-path MITM threat this guards
// against is real once traffic can leave a trusted internal network).
func isPrivateOrLoopbackHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// normalizeExternalSearchURL validates an admin-configured search backend
// URL (the SearXNG instance or the Admin Search Gateway). https is required
// unless the host is loopback/private/a bare service name (see
// isPrivateOrLoopbackHost) -- an admin running the backend on their own
// internal network doesn't need a public cert to get the same protection a
// public https requirement is for. Query string and fragment are always
// stripped: only fixed sub-paths are ever called (see gatewayEndpoint/
// searxngEndpoint), never a URL influenced by a prompt or tool call.
func normalizeExternalSearchURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Hostname() == "" || (scheme != "https" && (scheme != "http" || !isPrivateOrLoopbackHost(u.Hostname()))) {
		return "", fmt.Errorf("URL must be an absolute https URL (http only allowed for loopback/private/internal hosts)")
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (c *OnlineSearchClient) gatewayEndpoint(path string) string {
	base, _ := url.Parse(c.gatewayURL)
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func (c *OnlineSearchClient) setGatewayHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	if c.gatewayToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.gatewayToken)
	}
}

// AdvertisedAvailable reports whether search_web should be exposed to the
// model this turn -- the single gate both allTools (tools.go) and
// internetToolState (app.go) must agree on. For
// OnlineSearchBackendDuckDuckGo this is always true: it's a fixed public API
// with no admin-configured URL to be down, so gating it on HealthyCached
// would only ever produce a false negative from that cache's own staleness
// window (any turn landing more than internetHealthTTL after the last
// check reports unavailable even when DuckDuckGo is completely healthy --
// confirmed live: the very first chat message after enabling always hit
// this). A genuinely unreachable DuckDuckGo is still caught right before
// Search actually runs (CheckNow in tool_executor.go), gracefully, so
// nothing is lost by not gating advertisement on it too. SearXNG/Gateway are
// real admin-configured services that can be down for a real reason, so
// they keep the HealthyCached gate.
func (c *OnlineSearchClient) AdvertisedAvailable() bool {
	if c == nil {
		return false
	}
	if c.backend == OnlineSearchBackendDuckDuckGo {
		return true
	}
	return c.HealthyCached()
}

func (c *OnlineSearchClient) HealthyCached() bool {
	if c == nil || (c.backend == OnlineSearchBackendGateway && c.gatewayURL == "") || (c.backend == OnlineSearchBackendSearxng && c.searxngURL == "") {
		return false
	}
	now := time.Now()
	c.healthMu.Lock()
	fresh := !c.healthCheckedAt.IsZero() && now.Sub(c.healthCheckedAt) < internetHealthTTL
	ok := fresh && c.healthOK
	shouldRefresh := !fresh && !c.healthRefreshing
	if shouldRefresh {
		c.healthRefreshing = true
	}
	c.healthMu.Unlock()
	if shouldRefresh {
		go c.refreshHealth(context.Background())
	}
	return ok
}

func (c *OnlineSearchClient) CheckNow(ctx context.Context) bool {
	if c == nil || (c.backend == OnlineSearchBackendGateway && c.gatewayURL == "") || (c.backend == OnlineSearchBackendSearxng && c.searxngURL == "") {
		return false
	}
	c.healthMu.Lock()
	if c.healthRefreshing {
		c.healthMu.Unlock()
		return c.HealthyCached()
	}
	c.healthRefreshing = true
	c.healthMu.Unlock()
	return c.refreshHealth(ctx)
}

func (c *OnlineSearchClient) refreshHealth(ctx context.Context) bool {
	if c == nil || (c.backend == OnlineSearchBackendGateway && c.gatewayURL == "") || (c.backend == OnlineSearchBackendSearxng && c.searxngURL == "") {
		return false
	}
	ok := false
	defer func() {
		c.healthMu.Lock()
		c.healthOK = ok
		c.healthCheckedAt = time.Now()
		c.healthRefreshing = false
		c.healthMu.Unlock()
	}()

	healthCtx, cancel := context.WithTimeout(ctx, internetHealthTimeout)
	defer cancel()

	req, err := c.newHealthRequest(healthCtx)
	if err != nil {
		return false
	}

	resp, err := c.httpClient.Do(req)
	ok = err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !ok && c.backend == OnlineSearchBackendSearxng {
		// SearXNG itself didn't answer -- DuckDuckGo is a real backup, so
		// the tool must not go dark just because the primary is having an
		// outage; fall back to checking DDG's own reachability instead.
		ok = duckDuckGoReachable(ctx, c.httpClient)
	}
	return ok
}

// refreshHealth (via this and duckDuckGoReachable) treats the backend as
// healthy when EITHER the primary (SearXNG or Gateway) OR the DuckDuckGo
// fallback is reachable -- "DuckDuckGo is a real backup" means the tool must
// stay available (and Search must still try DDG) even during an extended
// SearXNG outage, not only for a single transient request. See
// fetchSearchResults for the matching per-query fallback.
func (c *OnlineSearchClient) newHealthRequest(ctx context.Context) (*http.Request, error) {
	switch c.backend {
	case OnlineSearchBackendSearxng:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.searxngEndpoint("/healthz"), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/plain, application/json")
		req.Header.Set("Accept-Encoding", "identity")
		return req, nil
	case OnlineSearchBackendDuckDuckGo:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, duckDuckGoInstantAnswerEndpoint("ping"), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		return req, nil
	default:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.gatewayEndpoint("/v1/health"), nil)
		if err != nil {
			return nil, err
		}
		c.setGatewayHeaders(req)
		return req, nil
	}
}

func (c *OnlineSearchClient) searxngEndpoint(path string) string {
	base, _ := url.Parse(c.searxngURL)
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

// duckDuckGoReachable is a cheap, fixed-endpoint reachability probe for the
// DuckDuckGo Instant Answer API fallback -- always the real, fixed public
// endpoint, never admin/prompt-configurable.
func duckDuckGoReachable(ctx context.Context, httpClient *http.Client) bool {
	healthCtx, cancel := context.WithTimeout(ctx, internetHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, duckDuckGoInstantAnswerEndpoint("ping"), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := httpClient.Do(req)
	ok := err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return ok
}

func duckDuckGoInstantAnswerEndpoint(query string) string {
	reqURL, _ := url.Parse("https://api.duckduckgo.com/")
	params := reqURL.Query()
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("no_html", "1")
	params.Set("no_redirect", "1")
	params.Set("skip_disambig", "1")
	reqURL.RawQuery = params.Encode()
	return reqURL.String()
}

// onlineSearchTool returns the LLM-facing tool definition for the internet
// search tool. Its Name MUST stay in sync with onlineSearchToolName -- every
// other file (ToolExecutor.Execute's switch, the pseudo-tool-call budget
// gate, newToolCallInfo) compares against that same constant.
func onlineSearchTool() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        onlineSearchToolName,
			Description: "Search current public web results strictly scoped to Grafana and its observability ecosystem. Use when the user explicitly asks to search the internet and the request is in authorized scope, or when a clear technical uncertainty/freshness need would materially improve the answer, such as current Grafana docs, plugin/data source behavior, dashboards, Grafana Cloud, Prometheus, Loki, Tempo, OpenTelemetry, Kubernetes observability references, or compatibility/version details. Do not use for trivial questions, when local Grafana tools/context are enough, for general internet search, or for live state from this user's Grafana instance. Always provide a short reason explaining what uncertainty or explicit user request the search resolves.",
			Parameters: json.RawMessage(`{
				"type": "object",
					"properties": {
						"query": { "type": "string", "description": "Observability-specific search query. No secrets." },
						"reason": { "type": "string", "description": "Short reason for searching now: explicit user request, exact uncertainty, freshness need, or documentation/compatibility question this resolves. No secrets." },
						"intent": { "type": "string", "enum": ["docs", "plugins", "dashboards", "datasources", "cloud", "github", "kubernetes", "all"] },
						"preferred_format": { "type": "string", "enum": ["auto", "short", "steps", "checklist", "table", "comparison", "troubleshooting", "code", "json"], "description": "Optional safe output shape requested by the user or best for the task. Backend returns formatting hints only; no HTML." },
						"continuation_approved": { "type": "boolean", "description": "True only when the user explicitly approved a second/refined search after the first attempt could not find enough authorized information." },
						"max_results": { "type": "integer", "description": "Max results to return (capped by backend)." }
					},
				"required": ["query", "reason"]
			}`),
		},
	}
}

func internetToolsLine(state InternetToolState) string {
	switch state {
	case InternetToolsEnabledWithSearch:
		return "Internet access: enabled by admin. Public web search is available only through the " + onlineSearchToolName + " tool and only for authorized Grafana/observability references. If the user explicitly asks to search the internet and the request is in scope, use this tool. Otherwise use it only with a clear technical reason: current/fresh documentation, uncertain behavior/version compatibility, plugin/data source/dashboard public reference, Grafana Cloud behavior, or avoiding a likely incorrect answer. Do not use it for trivial questions, general browsing, or when local Grafana tools/context are enough. Every call must include a short reason. If the tool returns continuation.needs_user_confirmation=true, do not search again in the same turn; ask the user whether to continue and only pass continuation_approved=true after explicit approval."
	case InternetToolsEnabledButUnavailable:
		return "Internet access: enabled by admin, but the configured internet provider is currently unreachable or failed its health check. Operate as local-only for this turn and do not suggest web search."
	case InternetToolsEnabledNoConfiguredTools:
		return "Internet access: enabled by admin, but no internet-backed tool is configured for this turn. Do not claim you can browse or search the web."
	default:
		return "Internet access: disabled by admin. Operate as a local Grafana assistant only: use local Grafana tools, provided context, and safe general knowledge. Do not suggest using web search or internet tools."
	}
}

// webSearchDecisionPolicy teaches the model WHEN to decide to search, not
// just that the tool exists -- only appended (via
// internetToolsPromptAddition) when InternetToolsEnabledWithSearch.
const webSearchDecisionPolicy = `Web search decision policy:
- If the user explicitly asks to search the internet, use ` + onlineSearchToolName + ` when internet is enabled/healthy and the request is inside the authorized Grafana/observability scope.
- Otherwise, use ` + onlineSearchToolName + ` only when it materially improves the answer.
- Good reasons: current/recent Grafana docs, uncertain feature behavior, version compatibility, plugin/data source/dashboard public reference, Grafana Cloud details, or avoiding a likely wrong answer.
- Bad reasons: curiosity, generic internet lookup, trivial explanation, live state of this user's Grafana, or when local Grafana tools/context already answer the question.
- Be efficient: prefer one targeted search with 3-5 results. Do not search repeatedly unless the first search returns no authorized useful result or reveals an official term needed for one refinement.
- If ` + onlineSearchToolName + ` returns continuation.needs_user_confirmation=true, stop searching in this turn, answer with what is known, and ask the suggested question. Only set continuation_approved=true after the user explicitly says to continue.
- If you search without the user explicitly asking, include a concise reason in the tool argument reason.
- Treat source metadata as part of the answer quality: official=true/trust_tier>=90 beats community results. Community and Reddit results are only supplemental signals.
- Use format_hints from ` + onlineSearchToolName + ` to shape the final answer, especially for code examples, tables, troubleshooting steps, comparisons and JSON.
- Obey format_hints.visible_limits: never flood the chat with too many links, bullets, rows, code blocks or long code.
- In the final answer, when useful, briefly say that public references were checked and why; do not over-explain the tool mechanics.
- When citing results, distinguish "official docs" from "community discussion" or "repository reference". Never present community content as official Grafana behavior.
- Match the user's requested output format. If they ask for a table, checklist, JSON, steps, short answer, comparison, or troubleshooting plan, format the internet-backed answer that way.
- If internet is disabled/unavailable/out of scope or returns no authorized relevant result, say that plainly and continue with local context/safe knowledge. Do not make the failure feel like a crash.
- If the search returns no authorized relevant result, continue locally. Do not suggest broader web search; only ask for a second, narrower authorized search when the tool explicitly returns continuation.needs_user_confirmation=true.`

// internetToolsPromptAddition is what streaming.go/llm.go append to the
// system prompt every turn, right after buildSystemPrompt -- the base
// internetToolsLine state sentence, plus the fuller decision policy only
// when a search tool is actually usable this turn.
func internetToolsPromptAddition(state InternetToolState) string {
	line := internetToolsLine(state)
	if state == InternetToolsEnabledWithSearch {
		return line + "\n\n" + webSearchDecisionPolicy
	}
	return line
}

type onlineSearchResponse struct {
	ToolResult
	Query        string                   `json:"query"`
	Intent       string                   `json:"intent"`
	ResultCount  int                      `json:"result_count"`
	Results      []onlineSearchResult     `json:"results"`
	FormatHints  onlineSearchFormatHints  `json:"format_hints"`
	Continuation onlineSearchContinuation `json:"continuation"`
}

type onlineSearchContinuation struct {
	NeedsUserConfirmation bool   `json:"needs_user_confirmation"`
	Reason                string `json:"reason,omitempty"`
	SuggestedQuestion     string `json:"suggested_question,omitempty"`
}

type onlineSearchResult struct {
	Title          string `json:"title"`
	URL            string `json:"url"`
	Source         string `json:"source"`
	SourceKind     string `json:"source_kind"`
	TrustTier      int    `json:"trust_tier"`
	TrustLabel     string `json:"trust_label"`
	Official       bool   `json:"official"`
	Snippet        string `json:"snippet,omitempty"`
	RelevanceScore int    `json:"relevance_score,omitempty"`
}

type onlineSearchFormatHints struct {
	PreferredFormat   string   `json:"preferred_format"`
	CodeFenceLanguage string   `json:"code_fence_language,omitempty"`
	SuggestedSections []string `json:"suggested_sections"`
	TableColumns      []string `json:"table_columns,omitempty"`
	Template          string   `json:"template"`
	CitationStyle     string   `json:"citation_style"`
	LinkPolicy        string   `json:"link_policy"`
	VisibleLimits     struct {
		MaxBullets     int `json:"max_bullets"`
		MaxLinks       int `json:"max_links"`
		MaxTableRows   int `json:"max_table_rows"`
		MaxCodeBlocks  int `json:"max_code_blocks"`
		MaxCodeRunes   int `json:"max_code_runes"`
		MaxAnswerRunes int `json:"max_answer_runes"`
	} `json:"visible_limits"`
}

type rawOnlineSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

type searchGatewayRequest struct {
	Query        string   `json:"query"`
	Intent       string   `json:"intent,omitempty"`
	MaxResults   int      `json:"max_results"`
	SafeMode     string   `json:"safe_mode"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

type searchGatewayResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet,omitempty"`
	} `json:"results"`
	Warnings []string `json:"warnings,omitempty"`
}

// searxngResponse mirrors SearXNG's own JSON search response shape (its
// `json` output format, enabled on the admin's instance) -- a fixed,
// well-known contract, not something the plugin or admin can redefine.
type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// duckDuckGoInstantAnswerResponse mirrors DuckDuckGo's real, fixed public
// Instant Answer API shape. This API returns an abstract/definition-style
// answer (often sourced from Wikipedia) plus loosely related topic links --
// it is NOT a general web-search results list, so it typically yields zero
// or one usable result even when it responds successfully. That is why it
// is only ever used as a fallback underneath SearXNG, never as the primary
// backend on its own.
type duckDuckGoInstantAnswerResponse struct {
	AbstractText  string `json:"AbstractText"`
	AbstractURL   string `json:"AbstractURL"`
	Heading       string `json:"Heading"`
	RelatedTopics []struct {
		Text     string `json:"Text"`
		FirstURL string `json:"FirstURL"`
	} `json:"RelatedTopics"`
}

func (c *OnlineSearchClient) Search(ctx context.Context, arguments string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("online search is not configured")
	}
	if c.backend == OnlineSearchBackendGateway && c.gatewayURL == "" {
		return "", fmt.Errorf("online search gateway is not configured")
	}
	if c.backend == OnlineSearchBackendSearxng && c.searxngURL == "" {
		return "", fmt.Errorf("SearXNG instance URL is not configured")
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return "", fmt.Errorf("online search rate limited: %w", err)
	}

	var args OnlineSearchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	query, err := sanitizeOnlineSearchQuery(args.Query)
	if err != nil {
		return "", err
	}
	reason, err := sanitizeOnlineSearchReason(args.Reason)
	if err != nil {
		return c.softResult(query, normalizeSearchIntent(args.Intent), "Online search skipped: no clear technical reason was provided.", []string{"Continue without web search. Search should be used only when it resolves a concrete Grafana/observability uncertainty."})
	}

	intent := normalizeSearchIntent(args.Intent)
	preferredFormat := normalizePreferredFormat(args.PreferredFormat)
	maxResults := args.MaxResults
	if maxResults <= 0 || maxResults > c.maxResults {
		maxResults = c.maxResults
	}

	if blockedOnlineSearchContentPattern.MatchString(query) {
		return c.softResult(query, intent, "Online search skipped: query matches blocked content policy.", []string{"Continue without web search; never surface prohibited or unrelated content through this plugin."})
	}
	if blockedOnlineSearchContentPattern.MatchString(reason) {
		return c.softResult(query, intent, "Online search skipped: reason matches blocked content policy.", []string{"Continue without web search; never surface prohibited or unrelated content through this plugin."})
	}

	if !onlineSearchQueryInScope(query) {
		return c.softResult(query, intent, "Online search skipped: query is outside the allowed Grafana/observability scope.", []string{"Continue without web search; use local Grafana tools or answer from safe general knowledge when appropriate."})
	}
	if !onlineSearchReasonIsValid(reason) {
		return c.softResult(query, intent, "Online search skipped: reason is too weak for autonomous web search.", []string{"Continue without web search. Use internet only when the answer materially depends on current or uncertain Grafana/observability documentation."})
	}

	scopes := searchScopesForIntent(intent)
	rawResults, providerWarnings, retryable, err := c.fetchSearchResults(ctx, query, intent, maxResults, scopes)
	if err != nil {
		if retryable {
			return c.softResultNeedsConfirmation(query, intent, "Online search unavailable or timed out within the safe first-attempt budget.", []string{"Continue with local context for now. Ask the user whether to continue searching.", safeOnlineSearchWarning(err.Error())})
		}
		return c.softResult(query, intent, "Online search backend rejected the request or is misconfigured.", []string{"Operate as local-only until an admin verifies the selected search backend configuration.", safeOnlineSearchWarning(err.Error())})
	}

	scored := make([]onlineSearchResult, 0, maxResults*2)
	for _, r := range rawResults {
		normalizedURL, scope, allowSnippet, ok := allowedSearchURL(r.URL, scopes)
		if !ok {
			continue
		}
		title := cleanSearchText(r.Title, maxOnlineSearchTitleChars)
		snippet := ""
		if allowSnippet {
			snippet = cleanSearchText(r.Snippet, maxOnlineSearchSnippetChars)
		}
		if title == "" {
			continue
		}
		if scope.RequiresStrongGrafanaContext && !strongGrafanaSearchTopicPattern.MatchString(query+" "+title+" "+normalizedURL) {
			continue
		}
		if !authorizedSearchResult(title, snippet, normalizedURL) {
			continue
		}
		if !hasSearchTermMatch(query, title, snippet, normalizedURL) {
			continue
		}
		score := relevanceScore(query, intent, title, snippet, normalizedURL, scope)
		if score < minOnlineSearchRelevanceScore {
			continue
		}
		scored = append(scored, onlineSearchResult{
			Title:          title,
			URL:            normalizedURL,
			Source:         scope.Host,
			SourceKind:     scope.SourceKind,
			TrustTier:      scope.TrustTier,
			TrustLabel:     scope.TrustLabel,
			Official:       scope.Official,
			Snippet:        snippet,
			RelevanceScore: score,
		})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].RelevanceScore > scored[j].RelevanceScore
	})
	if len(scored) > maxResults {
		scored = scored[:maxResults]
	}

	out := onlineSearchResponse{Query: query, Intent: intent, ResultCount: len(scored), Results: scored}
	out.FormatHints = formatHintsForSearch(preferredFormat, intent, query, scored)
	out.Sources = []string{fmt.Sprintf("%s online search backend, filtered by allowlist, source trust tiers, and output budget", c.backend)}
	out.Warnings = safeOnlineSearchWarnings(providerWarnings)
	if len(scored) == 0 {
		out.Summary = "No relevant allowed web results found."
		out.Warnings = append(out.Warnings, "Search was restricted to the Grafana/observability allowlist and local relevance filter.", "Do not retry automatically. Ask the user whether to continue with a second, narrower search.")
		out.Continuation = onlineSearchContinuation{
			NeedsUserConfirmation: true,
			Reason:                "No relevant allowed result was found in the first search attempt.",
			SuggestedQuestion:     "I didn't find a good authorized source in this first search. Would you like me to try a more detailed search?",
		}
	}
	return marshalOnlineSearchResponse(out)
}

// fetchSearchResults dispatches to the admin-selected backend. For
// OnlineSearchBackendSearxng specifically, a retryable SearXNG failure
// (timeout, non-2xx, unreachable) is followed by one real attempt against
// the DuckDuckGo Instant Answer fallback before giving up -- this is what
// makes DuckDuckGo an actual backup rather than a health-check-only
// placeholder. A DDG result (even a single abstract link) is returned as a
// successful, non-retryable outcome with a warning noting the fallback was
// used; if DDG also fails/returns nothing, the original SearXNG error is
// what's surfaced to Search's existing soft-fail path.
func (c *OnlineSearchClient) fetchSearchResults(ctx context.Context, query string, intent string, maxResults int, scopes []webSearchScope) ([]rawOnlineSearchResult, []string, bool, error) {
	switch c.backend {
	case OnlineSearchBackendGateway:
		return c.searchViaGateway(ctx, query, intent, maxResults, scopes)
	case OnlineSearchBackendDuckDuckGo:
		results, err := c.searchViaDuckDuckGo(ctx, query)
		if err != nil {
			return nil, nil, true, err
		}
		if len(results) == 0 {
			return nil, nil, false, fmt.Errorf("DuckDuckGo has no known definition for this query")
		}
		return results, []string{"DuckDuckGo free lookup: a short product definition only, not full web-search results. Configure a SearXNG instance or search gateway for complete results."}, false, nil
	default: // OnlineSearchBackendSearxng
		results, warnings, retryable, err := c.searchViaSearxng(ctx, query, maxResults, scopes)
		if err == nil {
			return results, warnings, false, nil
		}
		if !retryable {
			return nil, nil, false, err
		}
		ddgResults, ddgErr := c.searchViaDuckDuckGo(ctx, query)
		if ddgErr != nil || len(ddgResults) == 0 {
			return nil, nil, true, err
		}
		return ddgResults, []string{"SearXNG was unavailable; results came from the DuckDuckGo fallback instead."}, false, nil
	}
}

func (c *OnlineSearchClient) searchViaGateway(ctx context.Context, query string, intent string, maxResults int, scopes []webSearchScope) ([]rawOnlineSearchResult, []string, bool, error) {
	payload := searchGatewayRequest{
		Query:        query,
		Intent:       intent,
		MaxResults:   min(maxResults*2, maxOnlineSearchGatewayResults),
		SafeMode:     "strict",
		AllowedHosts: allowedSearchHosts(scopes),
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.gatewayEndpoint("/v1/search"), bytes.NewReader(body))
	if err != nil {
		return nil, nil, false, fmt.Errorf("create search gateway request: %w", err)
	}
	c.setGatewayHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, true, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, nil, false, fmt.Errorf("search gateway rejected configured credentials")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, true, fmt.Errorf("search gateway returned status %d", resp.StatusCode)
	}

	responseBody, err := readLimitedSearchBody(resp.Body)
	if err != nil {
		return nil, nil, true, err
	}
	var parsed searchGatewayResponse
	if err := json.NewDecoder(bytes.NewReader(responseBody)).Decode(&parsed); err != nil {
		return nil, nil, true, err
	}

	results := make([]rawOnlineSearchResult, 0, len(parsed.Results))
	for _, result := range parsed.Results {
		results = append(results, rawOnlineSearchResult{Title: result.Title, URL: result.URL, Snippet: result.Snippet})
	}
	return results, safeOnlineSearchWarnings(parsed.Warnings), false, nil
}

// searchViaSearxng queries the admin's own self-hosted SearXNG instance.
// Only the fixed /search sub-path is ever called (see searxngEndpoint); the
// query text itself is the only thing built from user/LLM input, same
// site:-scoping trick already used to keep results inside the allowlist's
// hosts (see buildProviderSiteQuery) as every other backend.
func (c *OnlineSearchClient) searchViaSearxng(ctx context.Context, query string, maxResults int, scopes []webSearchScope) ([]rawOnlineSearchResult, []string, bool, error) {
	scopedQuery := buildProviderSiteQuery(query, scopes)
	reqURL, err := url.Parse(c.searxngEndpoint("/search"))
	if err != nil {
		return nil, nil, false, fmt.Errorf("build SearXNG search URL: %w", err)
	}
	params := reqURL.Query()
	params.Set("q", scopedQuery)
	params.Set("format", "json")
	params.Set("safesearch", "2")
	params.Set("categories", "general")
	reqURL.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("create SearXNG search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, true, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, nil, false, fmt.Errorf("SearXNG instance rejected the request")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, true, fmt.Errorf("SearXNG instance returned status %d", resp.StatusCode)
	}

	body, err := readLimitedSearchBody(resp.Body)
	if err != nil {
		return nil, nil, true, err
	}
	var parsed searxngResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, nil, true, err
	}

	limit := min(maxResults*2, maxOnlineSearchGatewayResults)
	results := make([]rawOnlineSearchResult, 0, min(len(parsed.Results), limit))
	for i, result := range parsed.Results {
		if i >= limit {
			break
		}
		results = append(results, rawOnlineSearchResult{Title: result.Title, URL: result.URL, Snippet: result.Content})
	}
	return results, nil, false, nil
}

// maxDuckDuckGoCandidateQueries bounds how many sequential Instant Answer
// lookups searchViaDuckDuckGo will try for one call (see
// duckDuckGoCandidateQueries) -- each is a fast, fixed-endpoint call bounded
// by c.httpClient's own timeout, so this only caps worst-case latency when
// every candidate fails or times out.
const maxDuckDuckGoCandidateQueries = 3

// duckDuckGoAmbiguousEntityTerms lists observability/Grafana-family product
// names that resolve to a much more prominent, unrelated Wikipedia topic
// when queried bare -- confirmed by hand against the real API (2026-07-31):
// "loki" resolves to the Norse deity, "tempo" to musical tempo, "mimir" to a
// disambiguation page for the Norse figure, "alloy" to the metallurgy term,
// "druid" to the Celtic priesthood. None of these have their own
// correctly-disambiguating Wikipedia entry the way "Grafana", "Kubernetes",
// or "Prometheus (software)" do (see duckDuckGoDisambiguationHints).
// Excluded from duckDuckGoCandidateQueries entirely: better to return
// nothing than a confidently wrong definition (e.g. presenting Norse
// mythology as the answer to a Loki/LogQL question).
var duckDuckGoAmbiguousEntityTerms = map[string]bool{
	"loki": true, "tempo": true, "mimir": true, "alloy": true, "druid": true,
}

// duckDuckGoDisambiguationHints rewrites a known-ambiguous term into the
// query that actually resolves to the intended tech topic -- confirmed by
// hand (2026-07-31). Only add an entry here after manually verifying the
// rewritten query resolves to the CORRECT topic: guessing a "(software)"
// suffix is not reliable on its own -- "Tempo (software)" was tried and
// resolves to an unrelated iOS calendar app, not Grafana Tempo, which is why
// "tempo" is a block in duckDuckGoAmbiguousEntityTerms instead of a hint here.
var duckDuckGoDisambiguationHints = map[string]string{
	"prometheus": "Prometheus (software)",
}

// duckDuckGoCandidateQueries returns query terms to try against the Instant
// Answer API, most specific first. The API is an encyclopedia-style topic
// lookup, not a search engine: a full technical question (e.g. "loki logql
// label filter") almost never matches a topic, but a bare product/entity
// name inside it sometimes does (e.g. "Grafana" alone) -- reusing
// grafanaSearchTopicPattern (already the source of truth for which
// observability terms this plugin recognizes) to extract those names avoids
// a second, separately-maintained keyword list. Deliberately does NOT reuse
// buildProviderSiteQuery's "site:host OR site:host" scoping -- confirmed by
// hand that appending it makes the Instant Answer API return an empty
// result even for a query it would otherwise resolve, since it has no
// concept of search operators, only plain topic names.
func duckDuckGoCandidateQueries(query string) []string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil
	}
	candidates := []string{trimmed}
	seen := map[string]bool{strings.ToLower(trimmed): true}
	for _, term := range grafanaSearchTopicPattern.FindAllString(query, -1) {
		key := strings.ToLower(term)
		if seen[key] {
			continue
		}
		seen[key] = true
		if hint, ok := duckDuckGoDisambiguationHints[key]; ok {
			candidates = append(candidates, hint)
			continue
		}
		if duckDuckGoAmbiguousEntityTerms[key] {
			continue
		}
		candidates = append(candidates, term)
	}
	return candidates
}

// searchViaDuckDuckGo queries DuckDuckGo's real, fixed public Instant Answer
// API -- either as the primary (OnlineSearchBackendDuckDuckGo) or as a
// fallback when SearXNG itself failed (see fetchSearchResults). This is a
// real network call to a real DuckDuckGo endpoint (not a mock), but it is
// not a general web-search API: it returns at most a small handful of
// abstract/related-topic links for a recognized topic, most often pointing
// at Wikipedia, and nothing at all for a query it doesn't recognize as a
// topic. Results still go through the exact same allowlist/scope/relevance
// filtering as every other backend in Search -- this function only collects
// raw candidates. Tries duckDuckGoCandidateQueries in order and returns on
// the first one that yields any result, so a compound technical question
// still surfaces the definition of the product/entity it mentions instead
// of nothing.
func (c *OnlineSearchClient) searchViaDuckDuckGo(ctx context.Context, query string) ([]rawOnlineSearchResult, error) {
	var lastErr error
	for i, candidate := range duckDuckGoCandidateQueries(query) {
		if i >= maxDuckDuckGoCandidateQueries {
			break
		}
		results, err := c.fetchDuckDuckGoInstantAnswer(ctx, candidate)
		if err != nil {
			lastErr = err
			continue
		}
		if len(results) > 0 {
			return results, nil
		}
	}
	return nil, lastErr
}

func (c *OnlineSearchClient) fetchDuckDuckGoInstantAnswer(ctx context.Context, query string) ([]rawOnlineSearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, duckDuckGoInstantAnswerEndpoint(query), nil)
	if err != nil {
		return nil, fmt.Errorf("create DuckDuckGo request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("DuckDuckGo Instant Answer API returned status %d", resp.StatusCode)
	}

	body, err := readLimitedSearchBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed duckDuckGoInstantAnswerResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, err
	}

	var results []rawOnlineSearchResult
	if parsed.AbstractText != "" && parsed.AbstractURL != "" {
		title := parsed.Heading
		if title == "" {
			title = parsed.AbstractText
		}
		results = append(results, rawOnlineSearchResult{Title: title, URL: parsed.AbstractURL, Snippet: parsed.AbstractText})
	}
	for _, topic := range parsed.RelatedTopics {
		if topic.FirstURL == "" || topic.Text == "" {
			continue
		}
		results = append(results, rawOnlineSearchResult{Title: topic.Text, URL: topic.FirstURL, Snippet: topic.Text})
	}
	return results, nil
}

func allowedSearchHosts(scopes []webSearchScope) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, scope := range scopes {
		if !seen[scope.Host] {
			seen[scope.Host] = true
			hosts = append(hosts, scope.Host)
		}
	}
	return hosts
}

func (c *OnlineSearchClient) softResult(query string, intent string, summary string, warnings []string) (string, error) {
	out := onlineSearchResponse{
		Query:       query,
		Intent:      intent,
		ResultCount: 0,
		Results:     []onlineSearchResult{},
	}
	out.Summary = safeOnlineSearchWarning(summary)
	out.Warnings = safeOnlineSearchWarnings(warnings)
	out.IsPartial = true
	out.Sources = []string{"online search skipped or unavailable"}
	out.FormatHints = formatHintsForSearch("short", intent, query, nil)
	return marshalOnlineSearchResponse(out)
}

func (c *OnlineSearchClient) softResultNeedsConfirmation(query string, intent string, summary string, warnings []string) (string, error) {
	out := onlineSearchResponse{
		Query:       query,
		Intent:      intent,
		ResultCount: 0,
		Results:     []onlineSearchResult{},
	}
	out.Summary = safeOnlineSearchWarning(summary)
	out.Warnings = safeOnlineSearchWarnings(warnings)
	out.IsPartial = true
	out.Sources = []string{"online search first attempt incomplete"}
	out.FormatHints = formatHintsForSearch("short", intent, query, nil)
	out.Continuation = onlineSearchContinuation{
		NeedsUserConfirmation: true,
		Reason:                summary,
		SuggestedQuestion:     "I couldn't confirm this with authorized sources within the safe time budget. Would you like me to keep searching?",
	}
	return marshalOnlineSearchResponse(out)
}

func (c *OnlineSearchClient) unavailableResult(arguments string) (string, error) {
	return onlineSearchUnavailableResult(arguments, "Online search is unavailable because the internet provider failed health check.", []string{"Use local Grafana tools, provided context, and safe general knowledge. Do not suggest web search in this turn."})
}

func onlineSearchUnavailableResult(arguments string, summary string, warnings []string) (string, error) {
	var args OnlineSearchArgs
	_ = json.Unmarshal([]byte(arguments), &args)
	query := safeEchoSearchQuery(args.Query)
	intent := normalizeSearchIntent(args.Intent)
	out := onlineSearchResponse{
		Query:       query,
		Intent:      intent,
		ResultCount: 0,
		Results:     []onlineSearchResult{},
	}
	out.Summary = safeOnlineSearchWarning(summary)
	out.Warnings = safeOnlineSearchWarnings(warnings)
	out.IsPartial = true
	out.Sources = []string{"online search disabled or unavailable"}
	out.FormatHints = formatHintsForSearch("short", intent, query, nil)
	return marshalOnlineSearchResponse(out)
}

func onlineSearchContinuationRequiredResult(arguments string, summary string, warnings []string) (string, error) {
	var args OnlineSearchArgs
	_ = json.Unmarshal([]byte(arguments), &args)
	query := safeEchoSearchQuery(args.Query)
	intent := normalizeSearchIntent(args.Intent)
	out := onlineSearchResponse{
		Query:       query,
		Intent:      intent,
		ResultCount: 0,
		Results:     []onlineSearchResult{},
	}
	out.Summary = safeOnlineSearchWarning(summary)
	out.Warnings = safeOnlineSearchWarnings(warnings)
	out.IsPartial = true
	out.Sources = []string{"online search continuation requires user confirmation"}
	out.FormatHints = formatHintsForSearch("short", intent, query, nil)
	out.Continuation = onlineSearchContinuation{
		NeedsUserConfirmation: true,
		Reason:                out.Summary,
		SuggestedQuestion:     "Would you like me to keep searching authorized public sources?",
	}
	return marshalOnlineSearchResponse(out)
}

func safeEchoSearchQuery(raw string) string {
	return cleanSearchText(redactSecrets(raw), maxOnlineSearchQueryChars)
}

func safeOnlineSearchWarnings(warnings []string) []string {
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if clean := safeOnlineSearchWarning(warning); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func safeOnlineSearchWarning(raw string) string {
	return cleanSearchText(redactSecrets(raw), 240)
}

func readLimitedSearchBody(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxOnlineSearchGatewayResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxOnlineSearchGatewayResponseBytes {
		return nil, fmt.Errorf("search backend response exceeded %d bytes", maxOnlineSearchGatewayResponseBytes)
	}
	return body, nil
}

func marshalOnlineSearchResponse(out onlineSearchResponse) (string, error) {
	out.Query = truncateRunes(out.Query, maxOnlineSearchQueryChars)
	for i := range out.Results {
		out.Results[i].Title = truncateRunes(out.Results[i].Title, maxOnlineSearchTitleChars)
		out.Results[i].URL = truncateRunes(out.Results[i].URL, 512)
		out.Results[i].Snippet = truncateRunes(out.Results[i].Snippet, maxOnlineSearchSnippetChars)
	}

	for {
		body, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("marshal online search response: %w", err)
		}
		if len(body) <= maxOnlineSearchToolOutputBytes {
			return string(body), nil
		}

		if removeLowestTrustSnippet(&out) {
			continue
		}
		if len(out.Results) > 0 {
			out.Results = out.Results[:len(out.Results)-1]
			out.ResultCount = len(out.Results)
			out.Warnings = append(out.Warnings, "Some authorized results were omitted to keep the tool output within the safe size budget.")
			continue
		}
		out.Warnings = append(out.Warnings, "Online search output was reduced to stay within the safe size budget.")
		out.Summary = truncateRunes(out.Summary, 240)
		body, err = json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("marshal reduced online search response: %w", err)
		}
		if len(body) > maxOnlineSearchToolOutputBytes {
			return "", fmt.Errorf("online search output exceeded %d bytes after reduction", maxOnlineSearchToolOutputBytes)
		}
		return string(body), nil
	}
}

func removeLowestTrustSnippet(out *onlineSearchResponse) bool {
	idx := -1
	lowest := 101
	for i, result := range out.Results {
		if result.Snippet == "" {
			continue
		}
		if result.TrustTier < lowest {
			lowest = result.TrustTier
			idx = i
		}
	}
	if idx == -1 {
		return false
	}
	out.Results[idx].Snippet = ""
	out.Warnings = append(out.Warnings, "A snippet was omitted to keep the tool output small.")
	return true
}

func sanitizeOnlineSearchQuery(raw string) (string, error) {
	var b strings.Builder
	for _, r := range raw {
		if r == '\n' || r == '\r' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	q := strings.TrimSpace(whitespacePattern.ReplaceAllString(b.String(), " "))
	if q == "" {
		return "", fmt.Errorf("query is required")
	}
	words := strings.Fields(q)
	if len(words) > maxOnlineSearchQueryWords {
		q = strings.Join(words[:maxOnlineSearchQueryWords], " ")
	}
	q = truncateRunes(q, maxOnlineSearchQueryChars)
	if looksLikeSecretish(q) {
		return "", fmt.Errorf("query contains a secret; refused")
	}
	return q, nil
}

func sanitizeOnlineSearchReason(raw string) (string, error) {
	r := cleanSearchText(raw, 220)
	if r == "" {
		return "", fmt.Errorf("search reason is required")
	}
	if looksLikeSecretish(r) {
		return "", fmt.Errorf("search reason contains a secret; refused")
	}
	return r, nil
}

func onlineSearchReasonIsValid(reason string) bool {
	reason = strings.ToLower(reason)
	decisionTerms := []string{
		"user requested", "explicit user request", "asked to search", "requested internet",
		"current", "latest", "recent", "docs", "documentation", "version", "compatibility",
		"uncertain", "confirm", "verify", "behavior", "plugin", "datasource", "data source",
		"dashboard", "grafana cloud", "api", "breaking change", "feature", "deprecation",
		"atual", "recente", "versao", "versão", "compatibilidade", "funcionamento", "funciona",
		"usuario pediu", "usuário pediu", "pedido explicito", "pedido explícito", "pesquisar na internet",
		"nao tenho certeza", "não tenho certeza", "confirmar", "verificar", "documentacao", "documentação",
	}
	for _, term := range decisionTerms {
		if strings.Contains(reason, term) {
			return true
		}
	}
	return false
}

func normalizePreferredFormat(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "short", "steps", "checklist", "table", "comparison", "troubleshooting", "code", "json":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "auto"
	}
}

func formatHintsForSearch(preferred string, intent string, query string, results []onlineSearchResult) onlineSearchFormatHints {
	if preferred == "" || preferred == "auto" {
		preferred = inferPreferredFormat(intent, query)
	}
	h := onlineSearchFormatHints{
		PreferredFormat: preferred,
		CitationStyle:   "Use short inline source labels like (Official Grafana) or (Community discussion). Do not paste long URLs inline; use markdown links only when useful.",
		LinkPolicy:      "Show at most 5 authorized HTTPS links. Prefer official sources. Do not show raw query strings, fragments, download links, or blocked sources.",
	}
	h.VisibleLimits.MaxBullets = 8
	h.VisibleLimits.MaxLinks = 5
	h.VisibleLimits.MaxTableRows = 8
	h.VisibleLimits.MaxCodeBlocks = 2
	h.VisibleLimits.MaxCodeRunes = 1800
	h.VisibleLimits.MaxAnswerRunes = 6000

	switch preferred {
	case "code":
		h.CodeFenceLanguage = inferCodeFenceLanguage(query)
		h.SuggestedSections = []string{"What to use", "Safe example", "Notes from sources", "Links"}
		h.Template = "Use: short intro, one fenced code block with code_fence_language, 2-4 notes, then up to 3 source links. Never include more than 2 code blocks."
	case "table", "comparison":
		h.SuggestedSections = []string{"Summary", "Comparison", "Recommendation", "Sources"}
		h.TableColumns = []string{"Option", "When to use", "Source quality", "Link"}
		h.Template = "Use: one-sentence summary, compact table using table_columns, recommendation, then sources. Keep rows within visible_limits.max_table_rows."
	case "troubleshooting":
		h.SuggestedSections = []string{"Likely cause", "Checks", "Fix", "Sources"}
		h.Template = "Use: likely cause, numbered checks, minimal fix, sources. Keep it actionable and avoid dumping logs."
	case "steps", "checklist":
		h.SuggestedSections = []string{"Steps", "Validation", "Sources"}
		h.Template = "Use: numbered steps, validation checklist, sources. Keep bullets within visible_limits.max_bullets."
	case "json":
		h.SuggestedSections = []string{"JSON"}
		h.Template = "Use: one small JSON block only when the user asked for JSON. No comments, no markdown table, no extra prose unless needed."
	default:
		h.SuggestedSections = []string{"Short answer", "Important details", "Sources"}
		h.Template = "Use: short answer, up to 5 bullets of important details, then up to 3 source links."
	}

	if len(results) == 0 {
		h.PreferredFormat = "short"
		h.SuggestedSections = []string{"What I could verify", "How to proceed locally"}
	}
	return h
}

func inferPreferredFormat(intent string, query string) string {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "example") || strings.Contains(q, "exemplo") || strings.Contains(q, "query") || strings.Contains(q, "promql") || strings.Contains(q, "logql") || strings.Contains(q, "sql"):
		return "code"
	case strings.Contains(q, "compare") || strings.Contains(q, "comparar") || strings.Contains(q, "vs "):
		return "comparison"
	case strings.Contains(q, "troubleshoot") || strings.Contains(q, "debug") || strings.Contains(q, "erro") || strings.Contains(q, "error"):
		return "troubleshooting"
	case intent == "dashboards" || intent == "plugins" || intent == "datasources":
		return "table"
	default:
		return "short"
	}
}

func inferCodeFenceLanguage(query string) string {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "promql") || strings.Contains(q, "prometheus"):
		return "promql"
	case strings.Contains(q, "logql") || strings.Contains(q, "loki"):
		return "logql"
	case strings.Contains(q, "sql") || strings.Contains(q, "mysql") || strings.Contains(q, "postgres") || strings.Contains(q, "clickhouse"):
		return "sql"
	case strings.Contains(q, "json"):
		return "json"
	case strings.Contains(q, "yaml") || strings.Contains(q, "kubernetes"):
		return "yaml"
	case strings.Contains(q, "terraform") || strings.Contains(q, "hcl"):
		return "hcl"
	default:
		return "text"
	}
}

func looksLikeSecretish(q string) bool { return redactSecrets(q) != q }

func authorizedSearchResult(title string, snippet string, rawURL string) bool {
	combined := title + " " + snippet + " " + rawURL
	if blockedOnlineSearchContentPattern.MatchString(combined) {
		return false
	}
	if looksLikeSecretish(combined) {
		return false
	}
	if suspicious, _ := looksLikeInjectionAttempt(combined); suspicious {
		return false
	}
	return true
}

func onlineSearchQueryInScope(query string) bool {
	if kubernetesTopicPattern.MatchString(query) {
		return strongGrafanaSearchTopicPattern.MatchString(query)
	}
	if grafanaSearchTopicPattern.MatchString(query) {
		return true
	}
	return false
}

func normalizeSearchIntent(intent string) string {
	switch strings.ToLower(strings.TrimSpace(intent)) {
	case "docs", "plugins", "dashboards", "datasources", "cloud", "github", "kubernetes":
		return strings.ToLower(strings.TrimSpace(intent))
	default:
		return "all"
	}
}

func searchScopesForIntent(intent string) []webSearchScope {
	if intent == "all" {
		return defaultGrafanaSearchScopes
	}
	var out []webSearchScope
	for _, scope := range defaultGrafanaSearchScopes {
		if scopeMatchesIntent(scope, intent) {
			out = append(out, scope)
		}
	}
	if len(out) == 0 {
		return defaultGrafanaSearchScopes
	}
	return out
}

func scopeMatchesIntent(scope webSearchScope, intent string) bool {
	switch intent {
	case "plugins":
		return scope.Host == "grafana.com" || scope.Host == "github.com" || scope.Host == "community.grafana.com" || scope.Host == "stackoverflow.com"
	case "dashboards":
		return scope.Host == "grafana.com" || scope.Host == "github.com" || strings.Contains(scope.SourceKind, "community")
	case "datasources", "cloud":
		return scope.Official || scope.Host == "community.grafana.com" || scope.Host == "stackoverflow.com"
	case "github":
		return scope.Host == "github.com"
	case "kubernetes":
		return scope.Host == "grafana.com" || scope.Host == "kubernetes.io" || scope.Host == "prometheus.io" || scope.Host == "opentelemetry.io" || strings.Contains(scope.SourceKind, "community")
	case "docs":
		return scope.Official
	default:
		return true
	}
}

func relevanceScore(query string, intent string, title string, snippet string, rawURL string, scope webSearchScope) int {
	qTerms := meaningfulSearchTerms(query)
	haystackTitle := strings.ToLower(title)
	haystackBody := strings.ToLower(snippet + " " + rawURL)
	score := 0
	for _, term := range qTerms {
		if strings.Contains(haystackTitle, term) {
			score += 3
			continue
		}
		if strings.Contains(haystackBody, term) {
			score += 1
		}
	}
	switch intent {
	case "docs":
		if strings.Contains(rawURL, "/docs/") {
			score += 2
		}
	case "plugins":
		if strings.Contains(rawURL, "/grafana/plugins/") || strings.Contains(rawURL, "/developers/plugin-tools/") {
			score += 2
		}
	case "dashboards":
		if strings.Contains(rawURL, "/grafana/dashboards/") {
			score += 2
		}
	case "github":
		if strings.Contains(rawURL, "github.com/grafana") || strings.Contains(rawURL, "github.com/grafana-plugins") {
			score += 2
		}
	case "kubernetes":
		if strings.Contains(rawURL, "kubernetes.io/docs/") || strings.Contains(rawURL, "kube-state-metrics") {
			score += 2
		}
	}
	if strings.Contains(rawURL, "grafana.com/docs/") {
		score += 1
	}
	score += sourceTrustBonus(scope)
	return score
}

func hasSearchTermMatch(query string, title string, snippet string, rawURL string) bool {
	terms := meaningfulSearchTerms(query)
	haystack := strings.ToLower(title + " " + snippet + " " + rawURL)
	if len(terms) == 0 {
		return strings.Contains(haystack, "grafana") || strings.Contains(haystack, "/docs/")
	}
	for _, term := range terms {
		if strings.Contains(haystack, term) {
			return true
		}
	}
	return false
}

func sourceTrustBonus(scope webSearchScope) int {
	switch {
	case scope.TrustTier >= 100:
		return 5
	case scope.TrustTier >= 90:
		return 4
	case scope.TrustTier >= 75:
		return 2
	case scope.TrustTier >= 55:
		return 1
	default:
		return 0
	}
}

func meaningfulSearchTerms(query string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, term := range strings.Fields(strings.ToLower(query)) {
		term = strings.Trim(term, `"'()[]{}.,:;!?`)
		if len(term) < 3 || searchStopWords[term] || seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
	}
	return terms
}

var searchStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "com": true, "uma": true, "para": true,
	"como": true, "what": true, "with": true, "from": true, "sobre": true, "grafana": true,
}

func buildProviderSiteQuery(query string, scopes []webSearchScope) string {
	var sites []string
	seen := map[string]bool{}
	for _, s := range scopes {
		if !seen[s.Host] {
			sites = append(sites, "site:"+s.Host)
			seen[s.Host] = true
		}
	}
	q := query
	if len(sites) > 0 {
		q += " (" + strings.Join(sites, " OR ") + ")"
	}
	return truncateRunes(q, 380)
}

func allowedSearchURL(raw string, scopes []webSearchScope) (string, webSearchScope, bool, bool) {
	u, err := url.Parse(raw)
	if err != nil || strings.ToLower(u.Scheme) != "https" {
		return "", webSearchScope{}, false, false
	}
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if blockedDownloadPathPattern.MatchString(path) {
		return "", webSearchScope{}, false, false
	}
	for _, scope := range scopes {
		if host == scope.Host {
			for _, prefix := range scope.PathPrefixes {
				if strings.HasPrefix(path, prefix) {
					for _, denied := range scope.DenyPathParts {
						if strings.Contains(path, denied) {
							return "", webSearchScope{}, false, false
						}
					}
					u.RawQuery = ""
					u.Fragment = ""
					return u.String(), scope, scope.SnippetAllowed, true
				}
			}
		}
	}
	return "", webSearchScope{}, false, false
}

func cleanSearchText(raw string, maxRunes int) string {
	s := strings.TrimSpace(whitespacePattern.ReplaceAllString(htmlTagPattern.ReplaceAllString(html.UnescapeString(raw), " "), " "))
	return truncateRunes(s, maxRunes)
}
