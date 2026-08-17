package plugin

import (
	"fmt"
	"strconv"
	"strings"
)

// AgentInfo describes a selectable specialist persona for the frontend picker.
type AgentInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// HasContext is true once the user has saved non-empty specialization
	// text for this agent from the Agents tab. The frontend uses this to
	// refuse selecting a specialist that isn't configured yet, instead of
	// silently running it as an empty, indistinguishable copy of Default.
	HasContext bool `json:"hasContext"`
}

// maxAgentContextChars caps how much specialization text a single custom
// agent can carry. This text is injected into every request's system
// prompt for that agent, on top of the skill pack/persona/guardrails --
// realistic free-tier LLM endpoints (e.g. Groq's free tier tops out around
// 6-12k tokens per minute total) can be pushed into rate-limit errors by a
// system prompt that's too large before the user has typed a single
// message. ~4000 characters is roughly 800-1000 tokens: enough for a solid
// page of real domain guidance without dominating the token budget.
const maxAgentContextChars = 4000

// maxCustomAgents caps how many "agent-N" slots can exist beyond the
// built-in "generic" default, i.e. 10 selectable agents total. This mirrors
// the fixed-size color palette (agentColors.ts on the frontend): each slot's
// position -- not its user-chosen name -- determines its color, so the
// palette needs a hard upper bound.
const maxCustomAgents = 9

// defaultAgentActiveCount is how many custom slots a fresh install (or one
// predating the add/remove feature) starts with, matching the 3 fixed
// slots this plugin originally shipped with.
const defaultAgentActiveCount = 3

// fullAgentList is the fixed catalog of every possible slot, in display
// order. Beyond "generic", each slot is a blank specialist: its actual
// persona/domain knowledge is user-provided text (Settings.AgentContexts),
// edited in the app's Agents tab, not hardcoded here. This plugin ships with
// no built-in specialization -- it's a generic Grafana assistant that anyone
// can shape into their own specialists. Only the first Settings.AgentActiveCount
// custom slots are actually exposed to the frontend at any time (see
// effectiveAgentList) -- the rest exist here only so their position (and
// therefore their color and ID) stays fixed regardless of how many agents
// are currently active.
var fullAgentList = buildFullAgentList()

func buildFullAgentList() []AgentInfo {
	out := make([]AgentInfo, 0, maxCustomAgents+1)
	// HasContext: true -- Default never needs user-provided context to be
	// usable, unlike the custom agent-N slots below, whose HasContext starts
	// false until effectiveAgentList finds saved context for them.
	out = append(out, AgentInfo{ID: "generic", Label: "Default", Description: "General-purpose Grafana assistant -- metrics, logs, traces, dashboards, alerts.", HasContext: true})
	for i := 1; i <= maxCustomAgents; i++ {
		out = append(out, AgentInfo{
			ID:          fmt.Sprintf("agent-%d", i),
			Label:       fmt.Sprintf("Agent %d", i),
			Description: "Custom specialist. Configure its context in the Agents tab.",
		})
	}
	return out
}

// effectiveAgentList returns "generic" plus the first activeCount custom
// slots, with any user-renamed labels (Settings.AgentLabels) and HasContext
// flags (Settings.AgentContexts) applied -- the version actually served to
// the frontend, so the agent picker always reflects what's really
// configured and how many custom agents currently exist.
func effectiveAgentList(labels map[string]string, contexts map[string]string, activeCount int) []AgentInfo {
	if activeCount < 0 {
		activeCount = 0
	}
	if activeCount > maxCustomAgents {
		activeCount = maxCustomAgents
	}

	out := make([]AgentInfo, 0, activeCount+1)
	for _, a := range fullAgentList {
		if a.ID != "generic" {
			n, err := agentSlotNumber(a.ID)
			if err != nil || n > activeCount {
				continue
			}
		}
		if label, ok := labels[a.ID]; ok && label != "" {
			a.Label = label
		}
		if text, ok := contexts[a.ID]; ok && text != "" {
			a.HasContext = true
		}
		out = append(out, a)
	}
	return dedupeAgentLabels(out)
}

// dedupeAgentLabels renames later agents whose label collides (case- and
// whitespace-insensitively) with an earlier one -- agent names must be
// unique so the picker never shows two indistinguishable entries. The
// Agents admin page already blocks saving a duplicate name, but
// Settings.AgentLabels can also be set directly (provisioning, the Admin
// HTTP API), bypassing that UI check, so this is enforced again here.
func dedupeAgentLabels(agents []AgentInfo) []AgentInfo {
	seen := make(map[string]int, len(agents))
	for i, a := range agents {
		key := strings.ToLower(strings.TrimSpace(a.Label))
		seen[key]++
		if seen[key] > 1 {
			agents[i].Label = fmt.Sprintf("%s (%d)", a.Label, seen[key])
		}
	}
	return agents
}

// agentSlotNumber extracts N from an "agent-N" ID.
func agentSlotNumber(id string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "agent-"))
	if err != nil {
		return 0, fmt.Errorf("not a custom agent slot: %s", id)
	}
	return n, nil
}

// specialistFraming is prepended to every custom agent's specialization
// text, regardless of how well-written that text is. Being a specialist has
// to mean something structural, not just "the generic agent plus some extra
// text a busy admin may have thrown together carelessly" -- so this always
// applies three things on top of whatever the admin wrote: (1) treat the
// specialization content below as authoritative for this domain and use it
// directly and confidently, don't hedge or ask for information already
// given here, even if it's terse or informally written; (2) calibrate
// reasoning depth to the question -- answer a direct question that's
// clearly covered by the specialization immediately, exactly as fast as the
// default assistant would, but when a question is ambiguous, spans several
// of the facts above, or has real consequences if wrong, take a brief,
// explicit moment to reason through which facts apply before answering.
// This is not "always think longer" (that would make every simple question
// slower than the default assistant for no reason) -- it's "think when the
// question actually calls for it"; (3) correlate MORE tool calls/signals
// than the default assistant would before concluding "nothing is wrong" for
// an investigation-style question in this domain -- see its own doc comment
// below for the real, live-reproduced finding this closes.
const specialistFraming = `You are operating as a configured domain specialist for this organization, not the general-purpose default assistant. Three things follow from that:
- Treat the specialization context below as authoritative ground truth for your domain. Use it directly and confidently in your answer -- don't hedge, don't ask the user for information already given to you here, even if this context is terse or informally written.
- Calibrate how much you reason before answering to the question, not to a fixed rule. If the question is direct and clearly answered by a specific fact below, answer immediately and concisely -- just as fast as the default assistant would for an easy question. If the question is ambiguous, touches several facts at once, or getting it wrong would have real consequences, take a brief, explicit moment first to reason through which of the facts below actually apply before giving your final answer.
- ` + specialistCorrelationDirective

// specialistCorrelationDirective is the real, structural answer to a
// live-reproduced, data-validated finding: asked "are there any pods
// restarting or crashing right now?" against a real, ongoing
// ImagePullBackOff incident, a hand-configured Kubernetes specialist agent
// ran exactly ONE PromQL check (the one its own specialization text named)
// and confidently answered "no problems" -- the generic default agent did
// the same with one log search. Neither was wrong about what it checked;
// both were wrong to stop there, because a single metric/log source can
// structurally miss a failure mode it was never designed to catch (a
// restart counter is 0 for a container that has never successfully
// started). A specialist agent is exactly the case where the cost of one
// more tool call is worth it and the cost of a false "all clear" is
// highest, so its bar for stopping an investigation must be higher than the
// default assistant's, not the same: for any question asking whether
// something in your domain is broken, unhealthy, or has a problem right
// now, check at least one additional, structurally DIFFERENT signal (a
// different metric family, a log source, an event feed, or a dedicated
// diagnostic tool) before concluding nothing is wrong -- never let a single
// query's empty/clean result be the entire basis for an "all clear" answer.
const specialistCorrelationDirective = `Before concluding "no problem" for any question asking whether something in your domain is broken, unhealthy, or has an issue right now, cross-check at least one additional, structurally DIFFERENT signal (a different metric family, a log source, an event feed, or a dedicated diagnostic tool) -- never let a single query's empty/clean result be the entire basis for an "all clear" answer, since a signal can structurally miss the exact failure mode present (e.g. a restart counter reads 0 for a container that has never successfully started at all).`

// specializationBlock returns the extra prompt text for a specialist agent --
// specialistFraming plus the user-provided context for that agent ID, or ""
// for "generic"/unknown/not-yet-configured agents (Default never gets this
// framing -- it's what makes a configured specialist behaviorally different
// from Default in the first place, not just a copy with a different name).
func specializationBlock(agent string, contexts map[string]string) string {
	if contexts == nil {
		return ""
	}
	text, ok := contexts[agent]
	if !ok || text == "" {
		return ""
	}
	return "\n\n" + specialistFraming + "\n\nSpecialization (user-defined context for this agent):\n" + text
}

// genericSpecialistSuggestion tells the Default agent, only in the abstract,
// that one or more custom specialist agents are configured on this
// deployment -- unlike platform-ai (whose 3 specialists are fixed, always
// real, and known by name), this plugin ships blank: agent-N slots only
// become real specialists once an admin writes context for them in the
// Agents tab, and their labels are entirely user-chosen, so this can never
// name a specific agent by label the way platform-ai's equivalent does.
// Only fires when at least one non-generic slot actually HasContext (a
// renamed-but-still-empty slot doesn't count as "configured" -- see
// AgentInfo.HasContext's own doc comment).
func genericSpecialistSuggestion(agent string, labels map[string]string, contexts map[string]string, activeCount int) string {
	if agent != "generic" {
		return ""
	}
	list := effectiveAgentList(labels, contexts, activeCount)
	hasConfiguredSpecialist := false
	for _, a := range list {
		if a.ID != "generic" && a.HasContext {
			hasConfiguredSpecialist = true
			break
		}
	}
	if !hasConfiguredSpecialist {
		return ""
	}
	return "\n\nThis deployment has one or more custom specialist agents configured (visible in the agent picker). If a question is squarely and deeply about a specific technical domain that a differently-configured specialist agent might handle better (not just tangentially related), you may briefly mention that switching agents in the picker could give a more thorough answer -- you don't know what each one is actually configured for, so don't name or guess at a specific one, just point at the picker in general terms. Still answer the question yourself with what you can. Do not suggest switching for every question, only when a deeper specialization would genuinely help."
}

// resolveAgentActiveCount reads Settings.AgentActiveCount with the same
// nil-safe fallback used elsewhere (handleAgents) -- a zero-value Settings{}
// (common in tests that don't go through the normal settings-load/defaulting
// path) leaves this nil, and dereferencing it directly panics.
func resolveAgentActiveCount(activeCount *int) int {
	if activeCount != nil {
		return *activeCount
	}
	return defaultAgentActiveCount
}

// restrictAgentForRole applies Settings.RestrictSpecialistAgentsForViewers:
// when that admin opt-in is on and the requester's Grafana role is exactly
// "Viewer", any non-generic agent selection is silently downgraded to
// "generic" rather than rejected outright -- the request still succeeds,
// just without the specialist framing, matching resolveAgent's own
// never-hard-fail-over-agent-selection philosophy. An empty role (a
// non-user-initiated request) or any role other than "Viewer" is never
// restricted.
func restrictAgentForRole(agent string, role string, restrict *bool) string {
	if viewerSpecialistsBlocked(role, restrict) {
		return "generic"
	}
	return agent
}

// viewerSpecialistsBlocked is the shared condition behind restrictAgentForRole,
// factored out so the /agents list endpoint (handleAgents) can hide custom
// agent slots from a restricted Viewer's picker entirely, instead of only
// silently downgrading a request that already named one.
func viewerSpecialistsBlocked(role string, restrict *bool) bool {
	if restrict == nil || !*restrict {
		return false
	}
	return role == "Viewer"
}

func isValidAgent(id string) bool {
	if id == "" {
		return true
	}
	for _, a := range fullAgentList {
		if a.ID == id {
			return true
		}
	}
	return false
}

// resolvedTemperature returns the user-configured sampling temperature
// override for this agent, if any, clamped to the API's valid 0-2 range.
// ok is false when no override is set, meaning "use the provider's default".
//
// A literal 0.0 override is nudged to a tiny epsilon because
// openai.ChatCompletionRequest.Temperature uses `omitempty`: encoding/json
// only looks at the value, not whether the caller meant to set it, so a
// genuine "fully deterministic" choice would otherwise be silently dropped
// from the request and the provider's own (usually ~1.0) default would
// apply instead -- the opposite of what the user asked for.
func resolvedTemperature(temps map[string]float64, agent string) (float32, bool) {
	v, ok := temps[agent]
	if !ok {
		return 0, false
	}
	if v < 0 {
		v = 0
	}
	if v > 2 {
		v = 2
	}
	if v == 0 {
		v = 0.0001
	}
	return float32(v), true
}
