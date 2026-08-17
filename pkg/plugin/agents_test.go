package plugin

import (
	"strings"
	"testing"
)

// Being a specialist has to mean something structural, not just "generic
// plus whatever text an admin typed" -- specialistFraming must apply even
// when the admin's own text is terse/low-effort, and must never leak into
// Default (which has no configured domain, so nothing to treat as
// authoritative).
func TestSpecializationBlock_AppliesFramingEvenToTerseContext(t *testing.T) {
	contexts := map[string]string{"agent-1": "esc: 15min->maria. 45min->carlos."}
	block := specializationBlock("agent-1", contexts)

	if !strings.Contains(block, specialistFraming) {
		t.Error("expected specialistFraming to be present even for a terse/low-effort admin context")
	}
	if !strings.Contains(block, "esc: 15min->maria. 45min->carlos.") {
		t.Error("expected the admin's own text to still be present verbatim")
	}
}

func TestSpecializationBlock_GenericGetsNoFraming(t *testing.T) {
	contexts := map[string]string{"agent-1": "some context"}
	if block := specializationBlock("generic", contexts); block != "" {
		t.Errorf("specializationBlock(%q) = %q, want empty -- Default must never get specialist framing", "generic", block)
	}
	if block := specializationBlock("", contexts); block != "" {
		t.Errorf("specializationBlock(%q) = %q, want empty", "", block)
	}
}

// Real user request, following directly from the ImagePullBackOff finding
// above: a specialist's bar for stopping an investigation must be higher
// than the generic default's, not the same -- specialistFraming must
// instruct cross-checking an additional signal before an "all clear"
// answer, and generic (no specializationBlock at all) must not get that
// extra instruction, since it's specifically a specialist behavior.
func TestSpecializationBlock_IncludesCorrelationDirective(t *testing.T) {
	contexts := map[string]string{"agent-1": "some context"}
	block := specializationBlock("agent-1", contexts)
	if !strings.Contains(block, specialistCorrelationDirective) {
		t.Error("expected specialistCorrelationDirective to be present for a configured specialist")
	}
}

func TestSpecializationBlock_UnconfiguredAgentGetsNoFraming(t *testing.T) {
	// An agent slot that exists but has no saved context yet must behave
	// identically to Default -- no framing, no empty "Specialization:" block.
	if block := specializationBlock("agent-2", map[string]string{"agent-1": "x"}); block != "" {
		t.Errorf("specializationBlock for an unconfigured agent = %q, want empty", block)
	}
}

func TestEffectiveAgentList_GenericAlwaysHasContext(t *testing.T) {
	// Default never needs user-provided context to be usable -- unlike a
	// custom agent-N slot, it must never be reported as "not configured"
	// regardless of what AgentContexts/AgentLabels contain.
	list := effectiveAgentList(nil, nil, 3)
	if len(list) == 0 || list[0].ID != "generic" {
		t.Fatalf("expected generic to be the first entry, got %+v", list)
	}
	if !list[0].HasContext {
		t.Error("expected generic.HasContext to always be true")
	}
}

func TestEffectiveAgentList_ActiveCountTrimsCustomSlots(t *testing.T) {
	list := effectiveAgentList(nil, nil, 2)
	if len(list) != 3 { // generic + agent-1 + agent-2
		t.Fatalf("expected 3 agents (generic + 2 custom), got %d: %+v", len(list), list)
	}
	for _, a := range list {
		if a.ID != "generic" && a.ID != "agent-1" && a.ID != "agent-2" {
			t.Errorf("unexpected agent beyond activeCount: %s", a.ID)
		}
	}
}

func TestEffectiveAgentList_CustomSlotHasContextOnlyWhenConfigured(t *testing.T) {
	contexts := map[string]string{"agent-1": "Focus on Kubernetes."}
	list := effectiveAgentList(nil, contexts, 2)

	var agent1, agent2 *AgentInfo
	for i := range list {
		switch list[i].ID {
		case "agent-1":
			agent1 = &list[i]
		case "agent-2":
			agent2 = &list[i]
		}
	}
	if agent1 == nil || !agent1.HasContext {
		t.Error("expected agent-1 to have HasContext true (context configured)")
	}
	if agent2 == nil || agent2.HasContext {
		t.Error("expected agent-2 to have HasContext false (no context configured)")
	}
}

func TestEffectiveAgentList_DedupesCollidingLabels(t *testing.T) {
	// Settings.AgentLabels can be set directly (provisioning, Admin API),
	// bypassing the Agents page's own duplicate-name check -- the served
	// list must still never show two indistinguishable entries.
	labels := map[string]string{"agent-1": "Security", "agent-2": "security", "agent-3": "  Security  "}
	list := effectiveAgentList(labels, nil, 3)

	byID := make(map[string]string, len(list))
	for _, a := range list {
		byID[a.ID] = a.Label
	}
	if byID["agent-1"] != "Security" {
		t.Errorf("agent-1 label = %q, want unchanged %q", byID["agent-1"], "Security")
	}
	if byID["agent-2"] == "security" {
		t.Errorf("agent-2 label = %q, want a de-duplicated variant, not the raw collision", byID["agent-2"])
	}
	if byID["agent-3"] == "  Security  " {
		t.Errorf("agent-3 label = %q, want a de-duplicated variant, not the raw collision", byID["agent-3"])
	}
	if byID["agent-2"] == byID["agent-3"] {
		t.Errorf("agent-2 and agent-3 ended up with the same label %q after dedup", byID["agent-2"])
	}
}

func TestEffectiveAgentList_DedupesAgainstReservedDefaultLabel(t *testing.T) {
	labels := map[string]string{"agent-1": "Default"}
	list := effectiveAgentList(labels, nil, 1)

	var generic, agent1 string
	for _, a := range list {
		if a.ID == "generic" {
			generic = a.Label
		}
		if a.ID == "agent-1" {
			agent1 = a.Label
		}
	}
	if generic == agent1 {
		t.Errorf("generic and agent-1 both ended up labeled %q", generic)
	}
}
