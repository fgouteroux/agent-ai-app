package plugin

import "regexp"

// secretPatterns matches well-known credential formats that can show up in
// Grafana data this plugin reads on a user's behalf -- a log line, a
// dashboard annotation, a panel's own JSON -- none of which are meant to be
// seen by anyone, let alone forwarded to an external LLM provider
// (security-audit finding H-02). Unlike injectionPatterns (injection_guard.go),
// which deliberately never alters content because a false positive there
// would silently corrupt real data, a false positive here just replaces a
// string that merely resembled a secret with a placeholder -- a minor,
// visible inconvenience. A false negative here is a real credential
// leaving this Grafana instance for a third-party API. That asymmetry is
// why this list actually redacts.
var secretPatterns = []*regexp.Regexp{
	// Grafana's own service account tokens.
	regexp.MustCompile(`glsa_[A-Za-z0-9_]{20,}`),
	// AWS access key IDs (the secret access key itself isn't a fixed,
	// recognizable format without prohibitive false positives, so it isn't
	// matched here).
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	// GitHub personal access tokens / OAuth / app / refresh tokens.
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	// OpenAI/Groq/many OpenAI-compatible providers' key format.
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`),
	// Slack tokens.
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	// Bearer tokens in an Authorization-header shape, wherever they appear.
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-_.=]{16,}`),
	// PEM private key blocks, any type.
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	// JWTs -- three dot-separated base64url segments; real JWTs always
	// start with the base64 of {" (header), i.e. "eyJ".
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	// Generic key=value / key: "value" secrets -- the riskiest pattern here
	// for false positives (e.g. a log line legitimately saying
	// "password validation failed"), so it only matches when followed by
	// something that actually looks like a credential value (8+ chars, no
	// spaces), not just the word appearing on its own.
	regexp.MustCompile(`(?i)(api[_-]?key|secret|password|passwd|access[_-]?token)\s*[:=]\s*["']?([A-Za-z0-9\-_/+.]{8,})["']?`),
}

const redactionPlaceholder = "[REDACTED]"

// redactSecrets replaces any recognizable credential in text with a
// placeholder before it's sent to an external LLM provider. Best-effort,
// same as the PII detection elsewhere in this codebase -- it catches common,
// high-signal formats, not every possible secret shape.
func redactSecrets(text string) string {
	for _, re := range secretPatterns {
		text = re.ReplaceAllString(text, redactionPlaceholder)
	}
	return text
}
