package plugin

import (
	"context"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	openai "github.com/sashabaranov/go-openai"
)

// genericMaxContextTokens caps the Default/generic agent's context budget
// below the specialized agents' (which use a.settings.MaxContextTokens,
// normally 120k) -- deliberately nudges long, deep investigations toward a
// specialized agent instead of the general-purpose default.
const genericMaxContextTokens = 100000

// maxCustomAgentContextTokens caps how high a per-agent context override
// (Settings.AgentContextTokens, set from the Agents tab) can go.
const maxCustomAgentContextTokens = 120000

// maxContextTokensForAgent returns the effective context budget for the
// given agent ID. A per-agent override (perAgent[agent]) takes priority when
// set; otherwise falls back to the generic-agent cap or the globally
// configured budget.
func maxContextTokensForAgent(agent string, configured int, perAgent map[string]int) int {
	if v, ok := perAgent[agent]; ok && v > 0 {
		// The Agents page's slider only ever offers 100k-120k, but
		// Settings.AgentContextTokens can also be set directly (provisioning,
		// the Admin API), bypassing that UI -- clamp both ends here too so an
		// absurdly small override (e.g. a typo'd "500") can never starve a
		// request of the budget its own system prompt alone requires.
		if v < genericMaxContextTokens {
			v = genericMaxContextTokens
		}
		if v > maxCustomAgentContextTokens {
			v = maxCustomAgentContextTokens
		}
		return v
	}
	if agent == "" || agent == "generic" {
		if configured <= 0 || configured > genericMaxContextTokens {
			return genericMaxContextTokens
		}
	}
	return configured
}

const (
	// compactionTriggerRatio -- once the running conversation crosses this
	// fraction of the context budget, older history is compacted into a
	// summary before the next LLM call, instead of growing until the
	// request fails or gets silently truncated by the model server.
	compactionTriggerRatio = 0.85
	// minMessagesToCompact -- skip compaction on small conversations; there's
	// nothing worth summarizing yet.
	minMessagesToCompact = 8
	// keepRecentMessages -- most recent messages kept verbatim (not
	// summarized) so exact wording survives for what's closest to the
	// current turn.
	keepRecentMessages = 6
)

// compactIfNeeded checks the running token estimate against the agent's
// context budget and, if over the trigger ratio, replaces older messages
// with a single summary produced by the same LLM. The system prompt
// (messages[0]) and the most recent messages are always kept verbatim.
// Returns the original messages unchanged if compaction isn't needed, isn't
// possible (too few messages), or fails -- compaction is a best-effort
// efficiency measure, never a hard requirement for the chat to proceed.
// onStatus, if non-nil, is called with a short activity label exactly when
// compaction is about to run its summarization call -- lets the caller relay
// "Compactando contexto..." to the UI. Never called when compaction is
// skipped (below threshold, too few messages, etc.).
func compactIfNeeded(ctx context.Context, client *openai.Client, model string, messages []openai.ChatCompletionMessage, maxContextTokens int, onStatus func(string)) []openai.ChatCompletionMessage {
	if len(messages) < minMessagesToCompact+1 { // +1 for the system message
		return messages
	}
	if estimateMessagesTokens(messages) < int(float64(maxContextTokens)*compactionTriggerRatio) {
		return messages
	}

	systemMsg := messages[0]
	rest := messages[1:]
	if len(rest) <= keepRecentMessages {
		return messages
	}

	if onStatus != nil {
		onStatus("Compacting conversation context...")
	}

	splitAt := safeSplitIndex(rest, len(rest)-keepRecentMessages)
	older, recent := rest[:splitAt], rest[splitAt:]

	summary, err := summarizeMessages(ctx, client, model, older)
	if err != nil {
		log.DefaultLogger.Warn("context compaction failed, continuing uncompacted", "error", err)
		return messages
	}
	log.DefaultLogger.Info("context compacted", "originalMessages", len(messages), "summarizedMessages", len(older), "keptVerbatim", len(recent)+1)

	compacted := make([]openai.ChatCompletionMessage, 0, 2+len(recent))
	compacted = append(compacted, systemMsg, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: "[Automatic summary of the start of this conversation, compacted due to context limits -- the exact original message text is no longer available, only this summary]\n" + summary,
	})
	compacted = append(compacted, recent...)
	return compacted
}

// safeSplitIndex nudges a naive message-count split point backward so it
// never separates a tool-role message from the assistant message whose
// tool_calls produced it. The OpenAI-style message format requires a tool
// result to immediately follow its originating assistant message; cutting
// between them leaves "recent" starting with an orphaned tool message that
// has no context, which real testing showed causes the model to ignore the
// rest of the conversation and fall back to a generic greeting instead of
// using the tool result it was just given.
func safeSplitIndex(rest []openai.ChatCompletionMessage, splitAt int) int {
	for splitAt > 0 && rest[splitAt].Role == openai.ChatMessageRoleTool {
		splitAt--
	}
	return splitAt
}

// summarizeMessages asks the LLM itself for a compact summary of the older
// portion of the conversation -- no tools, small max_tokens, so this stays
// cheap and fast even on the same model already in use for the chat.
func summarizeMessages(ctx context.Context, client *openai.Client, model string, older []openai.ChatCompletionMessage) (string, error) {
	transcript := ""
	for _, m := range older {
		content := m.Content
		if content == "" && len(m.MultiContent) > 0 {
			// A user message with an image attachment has no plain Content
			// (see buildUserMessage) -- pull the text part back out, if any,
			// so the summary doesn't just show a blank line for that turn.
			for _, part := range m.MultiContent {
				if part.Type == openai.ChatMessagePartTypeText {
					content = part.Text
				}
				if part.Type == openai.ChatMessagePartTypeImageURL {
					content += " [attached image]"
				}
			}
		}
		if content == "" && len(m.ToolCalls) > 0 {
			content = fmt.Sprintf("[called %d tool(s)]", len(m.ToolCalls))
		}
		transcript += fmt.Sprintf("%s: %s\n", m.Role, truncateString(content, 2000))
	}

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				// Real live incident (2026-08-08): a request to dispatch two
				// workers and summarize their findings triggered enough tool
				// rounds to compact -- the summary preserved facts already
				// verified but not clearly enough what the user's original
				// request/goal actually was, and the final answer after
				// compaction ignored the real findings entirely and answered
				// an unrelated generic Grafana question instead. "Concrete
				// technical facts" alone reads as "facts found," not "the
				// still-outstanding thing being worked toward" -- stated as
				// its own explicit bullet so it survives compaction as
				// clearly as any other fact does.
				Content: "Summarize the conversation below, compactly (6-8 sentences max), in the same language the conversation is in. Preserve: (1) the user's original request or question -- what they actually asked for and are still waiting on an answer to, stated explicitly and unambiguously, even if it takes its own sentence; (2) concrete technical facts mentioned (dashboard names, metrics, values, decisions, and what has already been verified via tools). Do not invent anything not present in the text.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: transcript,
			},
		},
		MaxCompletionTokens: 400,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices in summary response")
	}
	return resp.Choices[0].Message.Content, nil
}
