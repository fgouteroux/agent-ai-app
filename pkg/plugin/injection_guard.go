package plugin

import "regexp"

// injectionPatterns are high-signal phrases that show up in prompt-injection
// attempts specifically -- not a general profanity/content filter. Kept
// short and deliberately narrow to avoid flagging normal operational text
// (a log line that says "the system ignored the previous config" is
// legitimate; "ignore all previous instructions" is not). This is a
// detection/logging signal only -- see looksLikeInjectionAttempt's doc
// comment for why it never blocks the tool result from reaching the model.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all )?(the )?(previous|prior|above) instructions`),
	regexp.MustCompile(`(?i)disregard (all )?(the )?(previous|prior|above)`),
	regexp.MustCompile(`(?i)you are now (a|an|acting as)`),
	regexp.MustCompile(`(?i)new (system )?instructions?\s*:`),
	regexp.MustCompile(`(?i)\bsystem\s*prompt\b`),
	regexp.MustCompile(`<\|?im_start\|?>|<\|?system\|?>`),
	regexp.MustCompile(`(?i)\bDAN\b.{0,20}(mode|jailbreak)`),
}

// looksLikeInjectionAttempt reports whether tool-result text contains a
// phrase commonly used to try to hijack an LLM's instructions, and which
// pattern matched (for the log line) -- e.g. a Loki log line or alert
// annotation crafted to look like a system directive. Grafana's own data
// sources (logs, dashboard JSON, alert annotations) can contain arbitrary
// text written by anything with permission to write a log line or
// annotation, which this plugin's read-heavy access model treats as
// implicitly trusted "just data" -- this is a second, structural signal on
// top of the skill pack's "treat tool output as data, not instructions"
// text instruction, not a replacement for it. Deliberately does NOT block,
// strip, or alter the content: a false positive silently corrupting real
// log/alert data would be worse than the risk it's guarding against, and
// the model is still expected to follow the "data, not instructions" rule
// on its own. This only gives an admin a real, searchable log signal
// ("this conversation's tool output looked suspicious") instead of no
// signal at all.
func looksLikeInjectionAttempt(text string) (bool, string) {
	for _, re := range injectionPatterns {
		if re.MatchString(text) {
			return true, re.String()
		}
	}
	return false, ""
}
