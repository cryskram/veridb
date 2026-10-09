package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cryskram/veridb/internal/audit"
	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/database"
	"github.com/cryskram/veridb/internal/guard"
)

// toggleTestDeps builds Deps over one database with the given services. The
// driver is nil because the toggle path never touches the database.
func toggleTestDeps(t *testing.T, services config.Services) *Deps {
	t.Helper()

	reg := database.NewRegistry()
	err := reg.Add(&database.Entry{
		Config: config.ResolvedDatabase{
			Name:     "app",
			Database: "app_db",
			Policy:   config.Policy{ReadOnly: true, MaxAffectedRows: 10},
			Services: services,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	logger, err := audit.New(config.ResolvedAudit{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	return &Deps{Registry: reg, Audit: logger}
}

func denialCode(t *testing.T, err error) string {
	t.Helper()

	var den *guard.Denial
	if !errors.As(err, &den) {
		t.Fatalf("err = %v (%T), want a *guard.Denial", err, err)
	}
	return den.Code
}

func TestSetWriteModeNeedsAdmin(t *testing.T) {
	d := toggleTestDeps(t, config.Services{Query: true})

	_, err := d.runSetWriteMode(context.Background(), setWriteModeInput{
		Database: "app", Mode: writeModeReadWrite, Confirm: writeModeConfirmPhrase,
	})
	if err == nil {
		t.Fatal("expected a refusal without the admin service")
	}
	if !strings.Contains(err.Error(), `"admin"`) {
		t.Errorf("refusal should name the admin service, got: %v", err)
	}
}

func TestSetWriteModeRejectsBadMode(t *testing.T) {
	d := toggleTestDeps(t, config.Services{Admin: true})

	_, err := d.runSetWriteMode(context.Background(), setWriteModeInput{
		Database: "app", Mode: "read-mostly",
	})
	if got := denialCode(t, err); got != "not_allowed" {
		t.Errorf("code = %q, want not_allowed", got)
	}
}

func TestSetWriteModeEnableNeedsConfirmation(t *testing.T) {
	d := toggleTestDeps(t, config.Services{Admin: true})

	_, err := d.runSetWriteMode(context.Background(), setWriteModeInput{
		Database: "app", Mode: writeModeReadWrite,
	})
	if got := denialCode(t, err); got != "confirm_required" {
		t.Errorf("code = %q, want confirm_required", got)
	}
	if d.Registry.WriteMode("app") {
		t.Error("refused enable must not flip the override")
	}

	_, err = d.runSetWriteMode(context.Background(), setWriteModeInput{
		Database: "app", Mode: writeModeReadWrite, Confirm: "pretty please",
	})
	if got := denialCode(t, err); got != "confirm_required" {
		t.Errorf("code = %q, want confirm_required for a wrong phrase", got)
	}
}

func TestSetWriteModeRoundTrip(t *testing.T) {
	d := toggleTestDeps(t, config.Services{Admin: true})
	ctx := context.Background()

	out, err := d.runSetWriteMode(ctx, setWriteModeInput{
		Database: "app", Mode: writeModeReadWrite, Confirm: writeModeConfirmPhrase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != writeModeReadWrite || out.PreviousMode != writeModeReadOnly {
		t.Errorf("out = %+v, want read-only -> read-write", out)
	}
	if !out.SessionOverride || !d.Registry.WriteMode("app") {
		t.Error("override should be active after enabling")
	}

	// Enabling again is a no-op success, not an error.
	out, err = d.runSetWriteMode(ctx, setWriteModeInput{
		Database: "app", Mode: writeModeReadWrite, Confirm: writeModeConfirmPhrase,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Notice, "already") {
		t.Errorf("second enable should report already-on, got notice %q", out.Notice)
	}

	// Disabling needs no confirmation.
	out, err = d.runSetWriteMode(ctx, setWriteModeInput{
		Database: "app", Mode: writeModeReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.SessionOverride || d.Registry.WriteMode("app") {
		t.Error("override should be cleared after disabling")
	}
	if out.PreviousMode != writeModeReadWrite || out.Mode != writeModeReadOnly {
		t.Errorf("out = %+v, want read-write -> read-only", out)
	}
}

// The toggle opens INSERT/UPDATE and nothing else: DELETE must still be
// refused by the guard under the effective policy.
func TestWriteOverrideKeepsDeleteRefused(t *testing.T) {
	d := toggleTestDeps(t, config.Services{Admin: true})
	ctx := context.Background()

	if _, err := d.runSetWriteMode(ctx, setWriteModeInput{
		Database: "app", Mode: writeModeReadWrite, Confirm: writeModeConfirmPhrase,
	}); err != nil {
		t.Fatal(err)
	}

	eff, err := d.Registry.EffectiveConfig("app")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := guard.Check("INSERT INTO orders (id) VALUES (1)", eff.Policy, "app"); err != nil {
		t.Errorf("INSERT should pass under the override, got: %v", err)
	}
	if _, err := guard.Check("DELETE FROM orders WHERE id = 1", eff.Policy, "app"); err == nil {
		t.Error("DELETE must still be refused under the override")
	} else if got := denialCode(t, err); got != "not_allowed" {
		t.Errorf("DELETE refusal code = %q, want not_allowed", got)
	}
}

func TestWriteToolsRegisterWithAdminOnly(t *testing.T) {
	d := toggleTestDeps(t, config.Services{Admin: true})

	if d.enabledForAny(writeToolsEnabled) {
		t.Error("write service should be off in this fixture")
	}
	if !d.enabledForAny(adminToolsEnabled) {
		t.Error("admin service should be on in this fixture")
	}
	// registerWriteTools registers when write OR admin is on, so the toggle
	// has tools to unlock; the per-call gate still refuses until then.
	if !d.enabledForAny(writeToolsEnabled) && !d.enabledForAny(adminToolsEnabled) {
		t.Error("write tools would not register, leaving the toggle with nothing to unlock")
	}
}
