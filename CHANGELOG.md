# Changelog

All notable changes to this plugin will be documented in this file, starting from its first public release.

## 1.1.0

- French added as a reply language, in the configuration page and in the backend's own language handling.
- Datasource allowlist: an admin can restrict every tool call to a named set of datasource UIDs, enforced server-side at the single point where a datasource is resolved.
- Tool call transparency: each tool call now shows the query it ran, the Grafana API requests it made, and the function name behind it.
- Rate limit and concurrency settings moved into the configuration page (requests per minute, concurrent chats, queue wait, queue depth), instead of being fixed at build time.
- The internal Grafana URL is now a setting rather than a hardcoded `http://localhost:3000`.
- In-conversation search and a back-to-top control in the chat.
- Panel data is sent to the model instead of being re-queried: the values a panel has already loaded travel with the question, downsampled, so the assistant reads what is on screen rather than running the panel's query a second time.
- Panel menu: a new "Ask Agent AI in a new tab" entry opens the full chat page in a browser tab, carrying the panel, its data, its dashboard uid and its time range -- a real conversation instead of the read-only modal preview.

## 1.0.0

Initial public release.

- AI assistant chat (standalone page, command palette, panel menu, Dashboard Chat) backed by an OpenAI-compatible LLM endpoint of your choice.
- Live, read-only tool-calling against Grafana: datasources, folders, dashboards, Prometheus/Loki/Tempo queries, and alerts -- no fixed assumptions about folder structure or environment naming.
- Automated root-cause investigation: ask about a firing alert and the assistant automatically gathers the relevant logs, traces, and any historical correlation around it in a single step.
- Optional long-term memory via the standalone Brain Agent plugin, when installed and enabled: the assistant can remember facts across sessions, automatically recall relevant memories for the dashboard/panel you're viewing, and queue its own inferred observations for admin review before they're saved.
- Up to 3 custom specialist agents with their own domain context, temperature, and context-window settings.
- Configurable backup/fallback LLM providers, tried automatically if the primary fails before any content has been shown.
- Optional audit logging (metadata by default, full message content opt-in) to Grafana's own backend logs.
- Admin-configurable guardrails appended on top of the built-in safety rules.
- Basic role-awareness (the assistant knows the requester's Grafana role) and settings-save conflict detection for safe multi-admin use.
- Optional zero-config integrations with `grafana-llm-app`, if installed: an additional LLM provider fallback, and (opt-in, off by default) extra read-only tools from its bundled MCP server (OnCall, ClickHouse, CloudWatch, Elasticsearch, roles/permissions, Pyroscope, and more).
- Requires Grafana >=12.0.0 -- Grafana 11 isn't supported (its plugin platform doesn't expose `react/jsx-runtime` as a shared module, which Grafana 13's React 19 runtime requires this plugin to use).
- Hardened against two real failure modes found live against a 14B-parameter model (qwen2.5:14b-instruct): a pseudo-tool-call JSON blob leaking into the visible response instead of executing, and a response answering in the wrong language entirely (not just switching mid-answer) relative to the user's prompt. Both are now detected and corrected (one bounded retry for the language case) rather than shown to the user as-is.
- Fixed a real authorization-bypass vulnerability (GO-2026-4762) in a transitive gRPC dependency, found via `govulncheck` -- bumped `google.golang.org/grpc` to the patched version.
