# How to use VeriDB

VeriDB sits between your agent and PostgreSQL. It exposes the database through
MCP tools, with every operation checked against the policy in
`configs/veridb.yaml` before it runs.

## 1. Get it running (five minutes)

```bash
git clone <this repo> && cd veridb
direnv allow                  # or: devenv shell
cp .env.example .env && chmod 600 .env
$EDITOR .env                  # fill in your DSN(s)
$EDITOR configs/veridb.yaml   # choose databases, services, policy
make config-check             # connects and reports what was skipped and why
```

`make config-check` starts the server with no client attached and exits. Look
for two things in its output: `[ok]` lines for the databases it reached, and
`[skip]` lines for config entries that do not exist on the host. Skipped
databases are a startup warning, not an error — the server still starts — and
`list_databases` repeats the reasons inside the agent session.

If you are working against your own host rather than the sample, copy the
example instead of editing it in place:

```bash
cp configs/veridb.example.yaml configs/veridb.local.yaml   # gitignored
make config-check CONFIG=configs/veridb.local.yaml
```

## 2. Hand it to your agent

Register the server in `~/.config/mcp/mcp.json` (after `make build`):

```json
{
  "mcpServers": {
    "veridb": {
      "command": "/home/you/Projects/veridb/bin/veridb",
      "args": ["-config", "/home/you/Projects/veridb/configs/veridb.yaml",
               "-env-file", "/home/you/Projects/veridb/.env"]
    }
  }
}
```

Then tell the agent, once, how to work with it. Something like this is enough:

> The `veridb` tools are your database access. Start every database task with
> `list_databases` to see what exists and which services each database allows.
> Explore with `list_tables`, `describe_table` and `sample_rows` before writing
> SQL. Send reads through `query` with `$1, $2` parameters, never with values
> pasted into the statement. If `truncated` is true, narrow the query — do not
> assume you saw everything.

After that, stay out of the way. The policy refuses anything the agent should
not do, and the refusal message says what to change.

## 3. The working loop

A session usually goes in this order, cheapest calls first:

1. **`list_databases`** — which databases exist, their mode (`read-only` vs
   `read-write`), which services each allows, and anything skipped at startup.
2. **`list_tables`** — tables with estimated rows and size. Use the estimates to
   avoid scanning something huge by accident.
3. **`describe_table`** + **`sample_rows`** — column types and a few real rows,
   so the SQL you write matches the actual shapes.
4. **`query`** — one read statement. Use `$1, $2` and `params`; the guard
   rejects string-concatenated filters the same way it rejects everything else.
5. **`explain_query`** — check the plan for anything expensive first.
   The default returns a plan without running the statement; `analyze: true`
   executes it and needs the write service for non-read statements.

`search_columns` answers "which table holds the email column?" across a whole
database, and `veridb_status` reports connectivity plus startup skips. Both are
always registered, like `list_databases`.

## 4. Reading the output

Reads return rows as objects plus a few fields that are worth paying attention
to:

- **`truncated: true`** means more rows matched than `applied_limit` returned.
  Narrow the query; the missing rows are not an error, they are the cap working.
- **`redacted_columns`** names columns whose values were replaced with
  `[redacted]` by policy. The data exists and was deliberately withheld.
- **`notice`** explains adjustments the server made, e.g. a requested limit
  lowered to the database cap.
- **Refusals arrive as errors** with a stable vocabulary: `read_only`,
  `missing_where` (an `UPDATE`/`DELETE` without a `WHERE`), `cap_exceeded`,
  `keyword`, `schema`, `table`. They describe the exact config change needed,
  which is usually a message for the operator, not the agent.

## 5. Letting an agent write

Writes are off everywhere by default and take two deliberate steps for one
database: permit the operation in policy **and** opt into the tool group.

```yaml
databases:
  sandbox:
    connection: primary
    read_only: false
    allow_write: true        # INSERT and UPDATE; DELETE needs allow_delete
    max_affected_rows: 25
    services:
      write: true            # registers execute / run_in_transaction for this DB
```

`execute` runs one statement in a transaction that is rolled back when it would
affect more than `max_affected_rows` — the count is only knowable after the
fact, so committing first would make an over-broad `UPDATE` unrecoverable.
`run_in_transaction` runs several statements all-or-nothing, checking every one
before running any of them.

## 6. Watching what happened

Every call, including refusals, is appended to the JSONL audit trail
(`audit.sinks` → `file`). To browse it:

```bash
make viewer-up    # docker compose up, then prints the public ngrok URL
make viewer-url   # print the URL again later
make viewer-down
```

The viewer ingests the trail into SQLite and serves a token-protected UI plus a
JSON API (`/api/records`, `/api/stats`, `/api/meta`) for scripting.

## 7. When something goes wrong

| Symptom | Meaning | Fix |
|---|---|---|
| `database "x" is not available` | Not in config, disabled, or skipped at startup | `list_databases` → check `skipped`; `make config-check` |
| `refused: database "x" is read-only` | `read_only: true` | Flip `read_only` and set the matching `allow_*` for that DB only |
| `this UPDATE has no WHERE clause` | `require_where_for_mutation` | Add the `WHERE`, or reconsider |
| `would affect N rows, above the cap` | `max_affected_rows` guard | The statement was rolled back; narrow it or raise the cap deliberately |
| `pg_sleep / dblink / COPY TO PROGRAM refused` | `deny_keywords` | Expected; these are escape hatches, not queries |
| `explain … analyze` refused for a write | `explain` needs the write service for mutations | `analyze` on a mutation *executes* it |
| Server starts with zero databases | Connection/DSN/environment problem | Read the `[warn]`/`[skip]` lines; `veridb_status` says the same thing in-session |
