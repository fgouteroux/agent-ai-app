// Distinct color per custom agent slot, assigned strictly by position --
// agent-1 always gets palette[0], agent-2 palette[1], and so on -- so an
// agent's color identity in the chat/history UI never changes just because
// the user renamed it. Mirrors pkg/plugin/agents.go's maxCustomAgents (9).
// Deliberately avoids blue (reserved for the Send button, see brand.ts's
// sendBlue) and green/yellow (read as "OK"/"warning" in an ops tool, not
// intended here).
const AGENT_COLOR_PALETTE = [
  '#9C5F3C', // agent-1 -- rust
  '#0E7C86', // agent-2 -- teal
  '#6A3FA0', // agent-3 -- purple
  '#B33F62', // agent-4 -- wine
  '#C2703A', // agent-5 -- burnt orange
  '#8B3FB3', // agent-6 -- orchid
  '#A0522D', // agent-7 -- sienna
  '#C24B4B', // agent-8 -- brick red
  '#705C99', // agent-9 -- slate violet
];

export const MAX_CUSTOM_AGENTS = AGENT_COLOR_PALETTE.length;

export const DEFAULT_AGENT_TAG_COLOR = '#5A5F6E';

const AGENT_SLOT_PATTERN = /^agent-(\d+)$/;

/** "agent-N" -> palette[N-1]; "generic"/unknown ids get the neutral default. */
export function colorForAgent(id: string): string {
  const match = AGENT_SLOT_PATTERN.exec(id);
  if (!match) {
    return DEFAULT_AGENT_TAG_COLOR;
  }
  const index = Number(match[1]) - 1;
  return AGENT_COLOR_PALETTE[index] ?? DEFAULT_AGENT_TAG_COLOR;
}
