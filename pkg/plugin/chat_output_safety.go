package plugin

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	maxAssistantChatMessageChars = 40_000
	maxAssistantStreamChunkChars = 4_000
	maxMarkdownLineChars         = 1_200
	maxMarkdownTableRows         = 60
	maxMarkdownListItems         = 80
	maxMarkdownLinks             = 20
	maxMarkdownCodeBlocks        = 6
)

// leakedModelMarkerTokenPattern strips known tokenizer-artifact marker words
// that qwen2.5:14b-instruct emits around a pseudo-tool-call (see
// extractPseudoToolCalls' doc comment and
// TestExtractPseudoToolCalls_JSONBlobWithStrayMarkerWords) from the START of
// a chunk of content. extractPseudoToolCalls only strips the JSON/tag block
// itself and deliberately leaves these words as "cosmetic residue" in
// cleanText -- fine when that residue is the ONLY thing wrong, but two real,
// separate leaks compound into a visible bug: (1) streaming.go sends that
// residue to the user as its own chunk when a tool call fires, and (2) on a
// later round the same marker can precede the model's real final answer with
// NO JSON attached this time, so extractPseudoToolCalls never even runs and
// the raw marker reaches the user unfiltered. Live-observed result (real
// screenshot, agent-ai-app, 2026-08-06): the two leaks land as consecutive
// stream chunks with no separator, rendering as
// "CallCheckCallCheckThere are no currently firing or pending alerts...".
// Anchored to the start of each chunk (not \b-bounded) because the second
// leak is directly adjacent to the first with no whitespace between them.
var leakedModelMarkerTokenPattern = regexp.MustCompile(`^\s*(?:(?:CallCheck|_icall_|_ical_)\s*)+`)

var unsafeChatMarkupPattern = regexp.MustCompile(`(?is)<\s*/?\s*(script|iframe|object|embed|style|link|meta|svg|math|form|input|button|video|audio|source|img)\b|javascript\s*:|data\s*:\s*(text/html|image/|application/)`)
var markdownImagePattern = regexp.MustCompile(`!\[[^\]]{0,200}\]\([^)]+\)`)
var mermaidFenceStartPattern = regexp.MustCompile("(?i)^```\\s*(mermaid|html|svg)\\s*$")
var orderedListItemPattern = regexp.MustCompile(`^\d+\.\s+`)
var markdownLinkPattern = regexp.MustCompile(`\[([^\]]{1,160})\]\([^)]+\)`)

// chatOutputBudget bounds a genuinely incremental stream of assistant content
// chunks (see streamFinalResponse) across the whole message, on top of
// sanitizeAssistantChatOutput's per-call cleanup -- distinct from a single
// already-complete string, where sanitizeAssistantChatOutput's own
// maxAssistantChatMessageChars cap alone is enough.
type chatOutputBudget struct {
	remaining int
	truncated bool
}

func newChatOutputBudget() *chatOutputBudget {
	return &chatOutputBudget{remaining: maxAssistantChatMessageChars}
}

// Apply sanitizes raw and returns the piece of it that still fits within
// this budget's remaining allowance, appending a friendly truncation notice
// exactly once when the budget first runs out. Safe to call repeatedly with
// successive chunks of the same streamed message.
func (b *chatOutputBudget) Apply(raw string) string {
	if b == nil {
		return sanitizeAssistantChatOutput(raw)
	}
	if b.remaining <= 0 {
		if b.truncated {
			return ""
		}
		b.truncated = true
		return "\n\n[Output truncated to keep the chat responsive.]"
	}
	clean := sanitizeAssistantChatOutput(raw)
	clean = truncateRunes(clean, min(b.remaining, maxAssistantStreamChunkChars))
	b.remaining -= len([]rune(clean))
	if b.remaining <= 0 && !b.truncated {
		b.truncated = true
		clean += "\n\n[Output truncated to keep the chat responsive.]"
	}
	return clean
}

// sanitizeAssistantChatOutput is the backend-side defense-in-depth pass every
// piece of assistant-authored content goes through before reaching the chat
// UI -- blocks HTML active markup / javascript: / data: URLs, drops remote
// markdown images, clamps expensive markdown constructs (huge tables, huge
// lists, runaway code blocks, giant lines), and caps total length. This does
// NOT replace search_web's own allowlist/authorization filtering (content
// that never passed that never reaches here to begin with) -- it protects
// against every OTHER path content can take into the chat (the model's own
// free-text, a tool result quoted back, provider error text, etc.).
func sanitizeAssistantChatOutput(raw string) string {
	if raw == "" {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
	s = leakedModelMarkerTokenPattern.ReplaceAllString(s, "")
	s = unsafeChatMarkupPattern.ReplaceAllString(s, "[blocked unsafe markup]")
	s = markdownImagePattern.ReplaceAllString(s, "[blocked remote image]")
	s = clampExpensiveMarkdown(s)
	return truncateRunes(s, maxAssistantChatMessageChars)
}

// clampExpensiveMarkdown limits per-line length, ordered/unordered list
// items, markdown links, table rows, and fenced code blocks -- and renders
// mermaid/html/svg fences as inert text instead of leaving them for the
// frontend to interpret as diagram/rich content. None of this alters the
// substance of a normal, reasonably-sized answer; it only kicks in once a
// response tries to flood the chat.
func clampExpensiveMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	tableRows := 0
	listItems := 0
	links := 0
	codeBlocks := 0
	inUnsafeFence := false

	for _, line := range lines {
		line = truncateRunes(line, maxMarkdownLineChars)

		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if mermaidFenceStartPattern.MatchString(strings.TrimSpace(line)) {
				inUnsafeFence = true
				out = append(out, "```text")
				out = append(out, "[diagram/html/svg rendering disabled for chat safety]")
				continue
			}
			if inUnsafeFence {
				inUnsafeFence = false
				out = append(out, "```")
				continue
			}
			codeBlocks++
			if codeBlocks > maxMarkdownCodeBlocks {
				out = append(out, "```text")
				out = append(out, "[additional code blocks truncated for chat safety]")
				out = append(out, "```")
				inUnsafeFence = true
				continue
			}
		}
		if inUnsafeFence {
			continue
		}

		if isMarkdownListItem(line) {
			listItems++
			if listItems > maxMarkdownListItems {
				if listItems == maxMarkdownListItems+1 {
					out = append(out, "- [list truncated for chat safety]")
				}
				continue
			}
		}

		links += strings.Count(line, "](")
		if links > maxMarkdownLinks {
			line = stripMarkdownLinks(line)
		}

		if strings.Count(line, "|") >= 2 {
			tableRows++
			if tableRows > maxMarkdownTableRows {
				if tableRows == maxMarkdownTableRows+1 {
					out = append(out, "| ... | ... |")
					out = append(out, "| table truncated for chat safety | |")
				}
				continue
			}
		} else {
			tableRows = 0
		}

		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isMarkdownListItem(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "* ") ||
		orderedListItemPattern.MatchString(trimmed)
}

func stripMarkdownLinks(line string) string {
	return markdownLinkPattern.ReplaceAllString(line, "$1")
}
