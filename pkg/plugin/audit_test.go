package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// fakeLogger captures Info() calls so tests can assert on exactly what an
// audit log line contains, without depending on the real logger's output
// format.
type fakeLogger struct {
	log.Logger
	infoArgs []interface{}
}

func (f *fakeLogger) Info(_ string, args ...interface{}) {
	f.infoArgs = args
}

func argsToMap(args []interface{}) map[string]interface{} {
	m := make(map[string]interface{})
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			continue
		}
		m[key] = args[i+1]
	}
	return m
}

func TestAuditLogChat_MetadataOnlyByDefault(t *testing.T) {
	t.Parallel()

	fl := &fakeLogger{}
	a := &App{logger: fl, settings: Settings{}}

	a.auditLogChat("alice", "Viewer", "chat", "generic", "what is my password?", "I can't reveal that.", nil, 1.23)

	fields := argsToMap(fl.infoArgs)
	if fields["user"] != "alice" || fields["role"] != "Viewer" || fields["mode"] != "chat" || fields["agent"] != "generic" {
		t.Errorf("fields = %+v, missing expected metadata", fields)
	}
	if fields["success"] != true {
		t.Errorf("success = %v, want true", fields["success"])
	}
	if _, ok := fields["prompt"]; ok {
		t.Error("prompt should NOT be logged when AuditLogFullContent is false")
	}
	if _, ok := fields["response"]; ok {
		t.Error("response should NOT be logged when AuditLogFullContent is false")
	}
	if fields["promptChars"] != len("what is my password?") {
		t.Errorf("promptChars = %v, want %d", fields["promptChars"], len("what is my password?"))
	}
}

func TestAuditLogChat_FullContentWhenEnabled(t *testing.T) {
	t.Parallel()

	fl := &fakeLogger{}
	a := &App{logger: fl, settings: Settings{AuditLogFullContent: true}}

	a.auditLogChat("bob", "Editor", "chat", "generic", "hello", "hi there", nil, 0.5)

	fields := argsToMap(fl.infoArgs)
	if fields["prompt"] != "hello" {
		t.Errorf("prompt = %v, want %q", fields["prompt"], "hello")
	}
	if fields["response"] != "hi there" {
		t.Errorf("response = %v, want %q", fields["response"], "hi there")
	}
}

func TestAuditLogChat_RecordsError(t *testing.T) {
	t.Parallel()

	fl := &fakeLogger{}
	a := &App{logger: fl, settings: Settings{}}

	a.auditLogChat("carol", "Admin", "chat", "generic", "hi", "", errors.New("boom"), 2.0)

	fields := argsToMap(fl.infoArgs)
	if fields["success"] != false {
		t.Errorf("success = %v, want false", fields["success"])
	}
	if fields["error"] != "boom" {
		t.Errorf("error = %v, want %q", fields["error"], "boom")
	}
}

func TestTruncateForAudit(t *testing.T) {
	t.Parallel()

	short := "hello"
	if got := truncateForAudit(short); got != short {
		t.Errorf("truncateForAudit(short) = %q, want unchanged %q", got, short)
	}

	long := strings.Repeat("a", maxAuditContentChars+500)
	got := truncateForAudit(long)
	if len(got) <= maxAuditContentChars {
		t.Errorf("expected truncated marker appended, got len %d", len(got))
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("expected truncation suffix, got suffix %q", got[len(got)-20:])
	}
}

func TestRequesterRole_ReturnsRoleFromPluginContext(t *testing.T) {
	t.Parallel()

	ctx := backend.WithPluginContext(context.Background(), backend.PluginContext{
		User: &backend.User{Login: "viewer1", Role: "Viewer"},
	})
	if got := requesterRole(ctx); got != "Viewer" {
		t.Errorf("requesterRole() = %q, want %q", got, "Viewer")
	}
}

func TestRequesterRole_EmptyWhenNoUser(t *testing.T) {
	t.Parallel()

	// A request Grafana's own backend initiated (e.g. Alerting) carries no
	// User -- must not panic, must return "".
	ctx := backend.WithPluginContext(context.Background(), backend.PluginContext{})
	if got := requesterRole(ctx); got != "" {
		t.Errorf("requesterRole() = %q, want empty string", got)
	}
}

func TestRequesterRoleLine(t *testing.T) {
	t.Parallel()

	if got := requesterRoleLine(""); got != "" {
		t.Errorf("requesterRoleLine(\"\") = %q, want empty string", got)
	}
	if got := requesterRoleLine("Editor"); !strings.Contains(got, "Editor") {
		t.Errorf("requesterRoleLine(\"Editor\") = %q, want it to mention the role", got)
	}
}
