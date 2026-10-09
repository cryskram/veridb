package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/cryskram/veridb/internal/audit"
	"github.com/cryskram/veridb/internal/config"
	"github.com/cryskram/veridb/internal/database"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// adminToolsEnabled reports whether any database exposes the admin service,
// which exists for session management rather than data access.
func adminToolsEnabled(s config.Services) bool { return s.Admin }

// writeModeConfirmPhrase is the exact confirm value that enables write mode.
// It is deliberately boring: its purpose is to force the agent to state intent
// explicitly, not to act as a secret. Turning write mode off needs no
// confirmation — refusing to disarm would be absurd.
const writeModeConfirmPhrase = "enable-writes"

const (
	writeModeReadOnly  = "read-only"
	writeModeReadWrite = "read-write"
)

type setWriteModeInput struct {
	Database string `json:"database" jsonschema:"database name from list_databases"`
	Mode     string `json:"mode" jsonschema:"\"read-only\" or \"read-write\""`
	Confirm  string `json:"confirm,omitempty" jsonschema:"required to enable: pass exactly \"enable-writes\"; turning write mode off needs no confirmation"`
}

type setWriteModeOutput struct {
	Database        string `json:"database"`
	Mode            string `json:"mode"`
	PreviousMode    string `json:"previous_mode"`
	SessionOverride bool   `json:"session_override"`
	Notice          string `json:"notice,omitempty"`
}

func registerAdminTools(server *mcp.Server, deps *Deps) {
	if !deps.enabledForAny(adminToolsEnabled) {
		return
	}

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "set_write_mode",
			Description: "Turn write mode on or off for one database for the rest of this " +
				"session. Enabling exposes the execute and run_in_transaction tools for that " +
				"database and permits INSERT and UPDATE; DELETE, DDL and TRUNCATE keep " +
				"following the config, and caps and timeouts are unchanged. The override is " +
				"kept in memory and a server restart drops it — to make a mode permanent, " +
				"set read_only, allow_write and services.write in the VeriDB config. " +
				"Enabling requires confirm \"enable-writes\"; disabling needs no confirmation.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, in setWriteModeInput) (*mcp.CallToolResult, setWriteModeOutput, error) {
			out, err := deps.runSetWriteMode(ctx, in)
			return nil, out, err
		},
	)
}

func (d *Deps) runSetWriteMode(ctx context.Context, in setWriteModeInput) (setWriteModeOutput, error) {
	var out setWriteModeOutput
	_ = ctx

	if in.Mode != writeModeReadOnly && in.Mode != writeModeReadWrite {
		return out, denial(
			"not_allowed",
			"mode must be %q or %q, got %q",
			writeModeReadOnly, writeModeReadWrite, in.Mode,
		)
	}

	_, eff, err := d.entry(in.Database, "admin", adminToolsEnabled)
	if err != nil {
		return out, err
	}

	start := time.Now()
	rec := audit.Record{Tool: "set_write_mode", Database: in.Database}

	previous := eff.Policy.Summarize()
	wantWrite := in.Mode == writeModeReadWrite
	out = setWriteModeOutput{
		Database:     in.Database,
		Mode:         in.Mode,
		PreviousMode: previous,
	}

	if wantWrite && in.Confirm != writeModeConfirmPhrase {
		return out, d.finish(rec, start, denial(
			"confirm_required",
			"enabling write mode for database %q needs explicit confirmation: "+
				"pass confirm %q. Turning write mode off needs no confirmation",
			in.Database, writeModeConfirmPhrase,
		))
	}

	if (!eff.Policy.ReadOnly) == wantWrite && d.Registry.WriteMode(in.Database) == wantWrite {
		out.SessionOverride = d.Registry.WriteMode(in.Database)
		out.Notice = fmt.Sprintf("database %q is already %s; no change", in.Database, in.Mode)
		rec.Note = fmt.Sprintf("%s -> %s (no change)", previous, in.Mode)
		return out, d.finish(rec, start, nil)
	}

	if err := d.Registry.SetWriteMode(in.Database, wantWrite); err != nil {
		return out, d.finish(rec, start, err)
	}

	out.SessionOverride = wantWrite
	if wantWrite {
		out.Notice = fmt.Sprintf(
			"write mode is on for database %q until the server restarts: execute and "+
				"run_in_transaction now accept INSERT and UPDATE, still capped by "+
				"max_affected_rows. DELETE, DDL and TRUNCATE still follow the config. "+
				"To make this permanent, set read_only: false, allow_write: true and "+
				"services.write: true for it in the VeriDB config",
			in.Database,
		)
	} else {
		out.Notice = fmt.Sprintf(
			"write mode is off for database %q; the config policy applies again",
			in.Database,
		)
	}
	rec.Note = fmt.Sprintf("%s -> %s (session override; resets on restart)", previous, in.Mode)

	return out, d.finish(rec, start, nil)
}

// declareWritable marks a just-begun transaction read-write. A database whose
// pool was opened read-only (default_transaction_read_only) refuses writes
// itself, so a session write override must say so explicitly — and it must say
// so before the first statement. On an already-writable pool this is a
// harmless no-op.
func declareWritable(ctx context.Context, tx database.Tx) error {
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ WRITE"); err != nil {
		return fmt.Errorf("declare writable transaction: %w", err)
	}
	return nil
}
