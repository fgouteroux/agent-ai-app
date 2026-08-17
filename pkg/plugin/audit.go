package plugin

import (
	"context"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// requesterRole returns the Grafana org role (Admin/Editor/Viewer) of the
// user who made this request, or "" if unknown (e.g. a request Grafana's own
// backend initiated, like Alerting, carries no user). This is Grafana's own
// role info, attached to every backend request by the platform itself --
// distinct from (and not a replacement for) whatever permissions the
// configured service account itself has; see effectiveGuardrails' trust-
// boundary note in guardrails.go.
func requesterRole(ctx context.Context) string {
	user := backend.PluginConfigFromContext(ctx).User
	if user == nil {
		return ""
	}
	return user.Role
}

// maxAuditContentChars bounds how much prompt/response text goes into a
// single audit log line when AuditLogFullContent is enabled -- large enough
// to be useful for a real investigation, small enough not to blow up a
// single log line.
const maxAuditContentChars = 4000

func truncateForAudit(s string) string {
	if len(s) <= maxAuditContentChars {
		return s
	}
	return s[:maxAuditContentChars] + "...(truncated)"
}

// auditLogChat records one completed chat exchange to the backend's own
// structured logger -- Grafana's existing log pipeline (stdout, Loki,
// whatever the operator already has configured) IS the audit trail here;
// this plugin doesn't invent a separate store or its own retention job for
// it. By default only metadata is recorded (who, which agent/mode, how
// long, whether it succeeded) -- this is invisible and always-on, no
// user-facing setting. Enabling Settings.AuditLogFullContent additionally
// records the prompt/final response text (truncated), for when an admin
// genuinely needs to review what was asked/answered; the frontend shows a
// discreet notice whenever that's on (see /limits' auditLogFullContent).
func (a *App) auditLogChat(user, role, mode, agent, prompt, response string, err error, durationSeconds float64) {
	fields := []any{
		"user", user,
		"role", role,
		"mode", mode,
		"agent", agent,
		"promptChars", len(prompt),
		"responseChars", len(response),
		"duration_s", durationSeconds,
		"success", err == nil,
	}
	if err != nil {
		fields = append(fields, "error", err.Error())
	}
	if a.settings.AuditLogFullContent {
		fields = append(fields, "prompt", truncateForAudit(prompt), "response", truncateForAudit(response))
	}
	a.logger.Info("chat audit", fields...)
}
