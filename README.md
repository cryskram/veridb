# VeriDB

A policy-controlled database MCP for AI coding agents.

VeriDB gives an agent real database access — schema discovery, SQL, query plans,
and optionally writes — while the operator keeps control from a single YAML file:
which databases are reachable, which tools exist, what each database permits,
what gets redacted, and where every call is recorded.

It runs as a stdio MCP server, so it plugs straight into pi, Claude Code, or any
other MCP client. PostgreSQL is supported today; the driver interface is the only
thing a new engine has to implement.

```
                    ┌──────────────────────── configs/veridb.yaml ────────────────────────┐
                    │  connections · databases · services · policy · audit               │
                    └───────────────────────────────┬────────────────────────────────────┘
                                                    │
   pi / any MCP client  ──stdio──▶  veridb  ────────┼──▶  PostgreSQL
                                        │           │
                                        │           └──▶  one pool per database,
                                        │                read-only enforced server-side
                                        │
                                        └──▶  audit JSONL ──▶ veridb-viewer ──▶ ngrok
                                                              (SQLite + web UI)
```

## What it looks like

```
> list the tables in the app database and show me a couple of orders

list_tables(database="app")
  orders             ~48,120 rows   36.2 MB
  order_items       ~212,004 rows   58.9 MB
  products            ~1,940 rows    3.1 MB
  ... 41 more

query(database="app", sql="SELECT id, customer_id, status FROM orders LIMIT 2")

  columns: id, customer_id, status   row_count: 2   applied_limit: 100
  rows: [{id: 8812, customer_id: 41903, status: "shipped"},
         {id: 8813, customer_id: 10942, status: "pending"}]
```

And if the agent tries something the operator did not allow:

```
query(database="identity", sql="SELECT id, email, pan FROM profiles")

  columns: id, email, pan            row_count: 2   applied_limit: 50
  redacted_columns: [email, pan]
  rows: [{id: 41903, email: "[redacted]", pan: "[redacted]"},
         {id: 10942, email: "[redacted]", pan: "[redacted]"}]
```

## Quick start

Requires [devenv](https://devenv.sh) and [direnv](https://direnv.net), or a Go
1.26 toolchain.

```bash
git clone <this repo> && cd veridb
direnv allow                 # or: devenv shell
cp .env.example .env
chmod 600 .env               # credentials must not be group/world readable
$EDITOR .env                 # fill in your DSN(s)
$EDITOR configs/veridb.yaml  # choose databases, services, policy
make check                   # fmt, vet, test
make config-check            # connect, and report what was skipped and why
```

`make` on its own lists every target.

### Where credentials live

`.env` is the only place they exist, and it is gitignored. Two deliberate
choices keep it that way:

- **direnv/devenv do not export `.env` into the shell.** devenv's dotenv
  integration writes the whole environment into `.devenv/shell-*.sh`, so a
  secret in the shell environment becomes a secret in a plaintext file. VeriDB
  reads `.env` itself at startup, and `docker compose` reads it natively, so
  nothing needs it exported. For a manual command:
  `set -a; . ./.env; set +a`
- **The pre-commit hook refuses `.env` outright**, whatever it contains. See
  [Secret scanning](#secret-scanning).

## Registering with pi

pi reads user-level MCP servers from `~/.pi/agent/mcp.json` (validate with
`pi mcp list`; a project-scoped server goes in `.pi/mcp.json` instead — see
[docs/usage.md](docs/usage.md)):

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

`-env-file` (or `VERIDB_ENV_FILE`) matters because pi may launch the server from
a different working directory, where a relative `.env` would not be found.
Alternatively drop `-env-file` and pass the DSN through the server's `env` block.

Build the binary first:

```bash
make build     # produces bin/veridb and bin/veridb-viewer
```

## How to use

The full walkthrough lives in **[docs/usage.md](docs/usage.md)**. The short
version:

1. `make config-check` — connect, and see exactly which databases were reached
   and which were skipped, before any agent is involved.
2. Tell the agent once: start with `list_databases`, explore with
   `list_tables` / `describe_table` / `sample_rows`, read through `query` with
   `$1, $2` parameters, and treat `truncated: true` as "narrow the query".
3. Let the policy do the rest. Refusals (`read_only`, `missing_where`,
   `cap_exceeded`, …) name the config change needed, and the audit trail
   records every call including the refused ones — browse it with
   `make viewer-up`.

Writes take two deliberate steps for one database: permit the operation in
policy (`read_only: false` plus the matching `allow_*`) **and** opt into the
`write` service. Nothing mutates until both are set. For databases the agent
should only *sometimes* write to, grant `services.admin` instead and toggle
with `set_write_mode` — session-only, INSERT/UPDATE only, and confirmed
explicitly. Details in [docs/usage.md](docs/usage.md).

## Tools

Tools are grouped into **services**, and each service can be enabled globally and
overridden per database. A service that no database enables is not registered at
all, so an agent never sees a tool it cannot use.

| Tool | Service | What it does |
|---|---|---|
| `list_databases` | always | Databases, their mode, their enabled services, and anything skipped |
| `veridb_status` | always | Which databases connected, which were skipped and why, optional live probe |
| `list_schemas` | `schema` | User-visible schemas |
| `list_tables` | `schema` | Tables, views, matviews with estimated rows and size |
| `describe_table` | `schema` | Columns, types, nullability, defaults, comments, primary key |
| `search_columns` | `schema` | Find columns by name across a whole database |
| `query` | `query` | One read statement, bounded and truncation-aware |
| `sample_rows` | `sample` | A few rows from a table, to learn its shape |
| `explain_query` | `explain` | Query plan; `analyze=true` executes it |
| `execute` | `write` | One mutation, inside a transaction, rolled back if over the row cap |
| `run_in_transaction` | `write` | Several statements, all or nothing |
| `set_write_mode` | `admin` | Toggle one database read-only/read-write for this session |

`list_databases` is the intended entry point: it says which databases exist and
which services each one allows, so an agent learns the terrain before acting.

## The safety model

The design assumption is that the agent will eventually try something it should
not. Every layer is therefore enforced on the server, not requested of the agent.

1. **Config is the allowlist.** A database only exists for the agent if it is
   named in `databases` and enabled. A name that does not exist on its host is
   skipped with a warning, never guessed.
2. **Services gate tools.** Disabled means the tool is not registered, and each
   call re-checks the service for the specific database.
3. **The SQL guard classifies before executing.** A hand-written lexer handles
   comments, quoted identifiers, `E''` strings and dollar quoting, so a rule
   cannot be evaded by hiding a keyword in a literal. Statements are judged at
   the highest risk found anywhere in them, which stops a data-modifying CTE from
   hiding behind an outer `SELECT`, and `EXPLAIN ANALYZE` is judged by the
   statement it actually runs.
4. **Policy decides.** `read_only` refuses every mutation. Otherwise
   `allow_write`, `allow_delete`, `allow_ddl` and `allow_truncate` are separate:
   enabling inserts does not enable deletes. `require_where_for_mutation` refuses
   an `UPDATE` or `DELETE` with no `WHERE` at its own nesting depth.
5. **The database agrees.** A read-only database sets
   `default_transaction_read_only` on every pooled connection, so even a guard
   bypass cannot write.
6. **Blast radius is capped.** Reads are wrapped in a limiting subquery and
   report truncation. Mutations run in a transaction that is rolled back when
   they exceed `max_affected_rows` — the count is only knowable after the fact,
   so committing without that check would make an over-broad `UPDATE`
   unrecoverable.
7. **Sensitive values are masked** before they reach the agent, by column pattern.
8. **Everything is recorded**, including refusals, which are usually the
   interesting part.

## Configuration

Everything lives in one file. See **[docs/configuration.md](docs/configuration.md)**
for the full reference; this is the shape:

```yaml
connections:                 # how to reach a host; credentials from the environment
  primary:
    driver: postgres
    dsn_env: VERIDB_PRIMARY_DSN           # postgres://USER:PASSWORD@HOST:PORT/{database}
    maintenance_database: postgres        # used only to verify a database exists

services:                    # which tool groups exist at all
  schema: true
  query: true
  write: false

defaults:                    # policy every database inherits
  read_only: true
  max_rows: 100
  statement_timeout: 15s

databases:                   # the explicit allowlist
  app:
    connection: primary
    description: Application data (users, orders, products)
  identity:
    connection: primary
    max_rows: 50                          # only what differs from defaults
    redact_columns: ["*.email", "*.pan"]

audit:
  sinks: [stderr, file]
  file: tmp/audit/veridb-audit.jsonl
```

A database states only what differs from `defaults`. One connection can back many
databases by putting `{database}` in the DSN.

## Audit trail and viewer

`veridb` appends JSON Lines — append-only, crash-safe, one record per call. If the
process dies mid-write, the sink closes the dangling line on restart so the next
record is not corrupted.

The viewer tail-loads that file into SQLite and serves a small UI:

```bash
make viewer-up      # docker compose up, then prints the public ngrok URL
make viewer-url     # print the URL again later
make viewer-logs
make viewer-down
```

Add to `.env`:

```bash
VIEWER_TOKEN=$(openssl rand -hex 24)
NGROK_AUTHTOKEN=...        # https://dashboard.ngrok.com/get-started/your-authtoken
```

Then open `https://<something>.ngrok-free.app/?token=<VIEWER_TOKEN>`.

The stack runs two containers: the viewer, and ngrok. The MCP server is
*not* containerised — it must stay a stdio child of your agent — so the audit
directory is bind-mounted read-only into the viewer.

Viewer properties worth knowing:

- **Incremental ingest.** The byte offset of the last complete record is stored,
  so restarts resume rather than re-read. A truncated or rotated file is re-read
  from the start.
- **Resilient to corruption.** An unparseable line is counted, logged and
  skipped; it never blocks the rest of the trail.
- **Token auth.** Via `X-Viewer-Token`, `?token=`, or a cookie that is set after
  the first authenticated visit so links keep working.
- **Cached.** Responses carry `Cache-Control` and are served from an in-process
  TTL cache (default 5s) whose key excludes the token, so the cache is shared
  between visitors. New records flush it.
- **JSON API** for scripting: `/api/records`, `/api/stats`, `/api/meta`.
- **Runs as your user** (uid/gid from `id -u`) so it can read the `0640` trail
  and write SQLite into `tmp/viewer`.

## Adding a database

1. Add or reuse a `connection`.
2. Add an entry under `databases` with only the overrides you need.
3. Put the credentials in `.env`.
4. `make config-check` — it prints every database it connected to and every one
   it skipped, with the reason.

## Layout

```
cmd/veridb/            MCP server entry point
cmd/veridb-viewer/     audit viewer entry point
internal/config/       YAML schema, defaults merging, DSN resolution, validation
internal/guard/        SQL lexer, classification, policy enforcement, read bounding
internal/database/     Driver interface, PostgreSQL adapter, registry, value normalisation
internal/mcp/          Tool definitions, service gating, audit wiring
internal/schema/       Catalog inspection queries
internal/audit/        Audit record model and sinks
internal/redact/       Column masking
internal/glob/         Pattern matching for deny lists and redaction
internal/viewer/       SQLite ingest and the web UI
```

## Development

```bash
make test          # unit tests
make test-race
make check         # fmt, vet, test
make run           # run the MCP server on stdio
make viewer        # run the viewer locally against tmp/audit/veridb-audit.jsonl
```

Config files under `configs/` are plain YAML; `devenv.nix` provides Go,
PostgreSQL and the rest of the toolchain.

## Secret scanning

A sample credential was committed to this repository once. The response is an
automatic check rather than a resolution to be careful.

`.githooks/pre-commit` runs three layers:

1. **Refuse files that hold secrets by definition** — `.env`, `.env.*`, `*.pem`,
   `*.key`, `.pgpass`, `.netrc`, `auth.json` — whatever they contain.
   `.env.example` is the one documented exception and must hold placeholders.
2. **gitleaks** over the staged diff, narrowed by `.gitleaks.toml`.
3. **A built-in pattern check** that still runs when gitleaks is absent, so a
   missing tool degrades the scan instead of disabling it. Its DSN rule extracts
   each candidate and reports it only when neither the password nor the host is a
   recognisable placeholder, because a scanner that flags documentation examples
   gets bypassed with `--no-verify`, and then it protects nothing.

devenv points git at these hooks on shell entry, so this is automatic. If you
work outside the devenv shell:

```bash
make hooks        # git config core.hooksPath .githooks
make secrets      # scan the working tree and all history
make verify       # fmt, vet, test, and the secret scan
```

If a credential is ever committed, **rotate it**. Rewriting history does not
un-publish it: it was public from the moment it was pushed.

## Security notes

- Credentials are never read from the config file, only from the environment.
  `.env` is gitignored and kept at `0600`; `.env.example` holds placeholders.
- Never expose `veridb` itself over a network: it is a stdio server, and its
  policy is the only thing between an agent and your data.
- The viewer's token grants read access to your audit trail, which contains SQL
  text and database and table names. Treat it as a password, and note that an
  ngrok URL is public unless protected.
- `include_params: false` by default, because bind parameters are where PII
  usually hides.
- A config naming internal hosts and databases describes your architecture. Keep
  such a repository private unless the sample has been genericised.
