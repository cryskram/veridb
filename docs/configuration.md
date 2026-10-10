# Configuration reference

VeriDB is driven by one YAML file, `configs/veridb.yaml` by default, overridable
with `-config` or `VERIDB_CONFIG`.

The file is read top to bottom as a single control plane:

| Section | Purpose |
|---|---|
| `server` | Identity advertised over MCP |
| `connections` | How to reach each host |
| `services` | Which tool groups exist at all, globally |
| `defaults` | The policy every database inherits |
| `databases` | The explicit allowlist, with per-database overrides |
| `audit` | Where tool calls are recorded |

Everything is validated at startup, and validation reports *every* problem it
finds rather than stopping at the first, so a new config can be fixed in one pass.

## Environment and secrets

Credentials never appear in this file.

- A field named `*_env` holds an **environment variable name**, not a value.
- A `${VAR}` written inside a resolved field is substituted from the environment.
  Only connection fields, `databases.<name>.database` and `audit.file` are
  expanded. Descriptions, instructions and comments are never rewritten, so
  documentation can mention `${VAR}` freely.
- A reference to an unset variable is a **startup error**, not an empty string,
  so a typo fails loudly instead of producing a confusing connection failure.

```yaml
connections:
  primary:
    dsn_env: VERIDB_PRIMARY_DSN
  reporting:
    host: ${REPORTING_HOST}
    user: ${REPORTING_USER}
    password_env: REPORTING_PASSWORD
```

## `server`

```yaml
server:
  name: veridb
  version: 0.2.0
  instructions: |
    Free-form guidance shown to the agent when it connects.
```

| Key | Default | Meaning |
|---|---|---|
| `name` | `veridb` | MCP server name |
| `version` | `0.0.0` | MCP server version |
| `instructions` | built-in text | Text handed to the client on connect; use it for house rules |

## `connections`

A connection describes how to reach a server. Databases reference connections by
name, so one connection can back an arbitrary number of databases.

```yaml
connections:
  primary:
    driver: postgres
    dsn_env: VERIDB_PRIMARY_DSN       # postgres://USER:PASSWORD@HOST:PORT/{database}
    maintenance_database: postgres
    pool:
      max_conns: 5
      min_conns: 0
      max_conn_lifetime: 30m
      max_conn_idle_time: 5m
      health_check_period: 1m
```

| Key | Default | Meaning |
|---|---|---|
| `driver` | required | Only `postgres` today |
| `dsn` | — | Inline DSN; may contain `{database}` |
| `dsn_env` | — | Environment variable holding the DSN; takes precedence over `host`/`port`/`user` |
| `host`, `port`, `user` | `5432` for port | Used to build a DSN when no DSN is supplied |
| `password_env` | — | Environment variable holding the password |
| `ssl_mode` | driver default | `sslmode` connection parameter |
| `maintenance_database` | `postgres` | Database used only to verify which databases exist |
| `pool.*` | see above | Bounded connection pool |

### The `{database}` placeholder

One connection entry can serve many databases:

```bash
VERIDB_PRIMARY_DSN=postgres://USER:PASSWORD@HOST:PORT/{database}
```

Each database gets its own pool whose DSN has `{database}` replaced with its
physical name. If a connection without a placeholder is shared by more than one
database, startup **fails** with an explanation, rather than silently pointing
several entries at the same database.

When a DSN is built from `host`/`port`/`user`, the database name is appended
automatically; no placeholder is needed.

### Missing databases are skipped, not fatal

At startup VeriDB lists the databases on each connection and compares that with
your config. A configured database that does not exist is **skipped with a
warning**, because one config is often shared across environments where a
database legitimately exists in only some of them. The server still starts, and
`list_databases` / `veridb_status` report what was dropped and why:

```
[skip] database "does_not_exist" does not exist on connection "primary" (host lookup succeeded); skipping
[ok]   app (app, read-only, 5 service(s) enabled)
```

If the listing itself fails (host down, bad credentials) the existence check is
skipped with a warning and VeriDB attempts a direct connection instead, so a
transient failure never hides a working database.

## `services`

Services are tool groups. A service disabled globally is disabled everywhere
unless a database turns it back on; a service that no database enables is not
registered with the MCP client at all.

```yaml
services:
  schema: true
  query: true
  explain: true
  sample: true
  health: true
  write: false
  admin: false
```

| Service | Default | Tools |
|---|---|---|
| `schema` | `true` | `list_schemas`, `list_tables`, `describe_table`, `search_columns` |
| `query` | `true` | `query` |
| `explain` | `true` | `explain_query` |
| `sample` | `true` | `sample_rows` |
| `health` | `true` | Live ping and version probe inside `veridb_status` |
| `write` | `false` | `execute`, `run_in_transaction` |
| `admin` | `false` | `set_write_mode` — the session write-mode toggle |

`list_databases` and `veridb_status` are always available: an agent must be able
to discover what it has and why something is missing.

Enabling `write` is not enough on its own. The database's **policy** must also
permit the specific operation. Both gates exist so that a service can be turned
on globally for a deployment while individual databases stay read-only.

```yaml
databases:
  sandbox:
    connection: primary
    services:
      write: true          # this database may use the write tools
```

## `defaults` and per-database policy

`defaults` is the policy every database inherits. A database overrides only the
fields it names — unset means "inherit", not "reset to zero".

```yaml
defaults:
  read_only: true
  allow_write: false
  allow_delete: false
  allow_ddl: false
  allow_truncate: false
  require_where_for_mutation: true
  max_rows: 100
  statement_timeout: 15s
  max_affected_rows: 1000
  allow_schemas: [public]
  deny_schemas: [pg_catalog, information_schema, pg_toast]
  deny_tables: []
  deny_keywords: [pg_read_file, pg_sleep, dblink, ...]
  redact_columns: []
```

### Permissions

| Key | Default | Effect |
|---|---|---|
| `read_only` | `true` | Master switch. Refuses every mutation regardless of the `allow_*` flags, and additionally sets `default_transaction_read_only` on every pooled connection so PostgreSQL itself rejects writes |
| `allow_write` | `false` | Permits `INSERT`, `UPDATE`, `MERGE` |
| `allow_delete` | `false` | Permits `DELETE`. Separate from `allow_write` because deleting rows is usually the riskiest thing an agent can do |
| `allow_ddl` | `false` | Permits `CREATE`, `ALTER`, `DROP`, `GRANT`, `COMMENT`, and administrative statements |
| `allow_truncate` | `false` | Permits `TRUNCATE`, which needs this **and** `allow_ddl` |
| `require_where_for_mutation` | `true` | Refuses an `UPDATE`/`DELETE` with no `WHERE` at its own nesting depth |

Both `read_only: false` and the matching `allow_*` flag are required, so opening a
database for writes is always a deliberate two-part act.

### Limits

| Key | Default | Effect |
|---|---|---|
| `max_rows` | `100` | Caps rows returned by a read. `0` means unlimited, and the response is still bounded at 1000 with a notice |
| `statement_timeout` | `15s` | Applied server-side as `statement_timeout` and `idle_in_transaction_session_timeout`. Accepts `15s` or `15000` (milliseconds) |
| `max_affected_rows` | `1000` | Caps rows touched by a mutation. Enforced by running the statement in a transaction and rolling it back when the count exceeds the cap, then explaining what happened |

A read is executed as `SELECT * FROM (<your query>) AS veridb_limited LIMIT
max_rows+1`. Fetching one extra row is what makes `truncated` provable rather
than guessed. Statements that cannot be wrapped — `SHOW`, and reads with
`FOR UPDATE` — are run as-is and bounded while reading instead.

### Scope

| Key | Default | Effect |
|---|---|---|
| `allow_schemas` | `[]` (anything not denied) | Allowlist of readable schemas |
| `deny_schemas` | `pg_catalog`, `information_schema`, `pg_toast` | Always subtracted, even from the allowlist |
| `deny_tables` | `[]` | Glob patterns of relations that are never exposed |

`deny_schemas` blocks ordinary queries against the catalog schemas; the schema
tools still cover that ground with dedicated queries.

An unqualified table name is assumed to resolve to `public` (the usual
`search_path`), which makes `allow_schemas` fail closed: if `public` is not
allowed, unqualified names are refused too.

### Keyword rules

`deny_keywords` are matched against a *skeleton* of the statement in which
comments and string literals have been replaced by placeholders. A keyword cannot
be smuggled inside a literal to trip a rule, and cannot be hidden behind a
comment to evade one. The default list blocks filesystem access (`pg_read_file`,
`pg_ls_dir`, `lo_import`), cross-database links (`dblink`), session disruption
(`pg_terminate_backend`, `pg_cancel_backend`), `pg_sleep`, and
`COPY ... TO/FROM PROGRAM`.

Replacing the list replaces it entirely — restate the defaults if you want to
keep them.

### Redaction

`redact_columns` holds glob patterns matched against columns; a match has its
value replaced with `[redacted]` before the response. The response lists which
columns were masked, so an agent knows the data exists but was withheld.

```yaml
redact_columns:
  - "*.pan"          # a column called pan, in any table
  - "*password*"     # anything with password in the name
  - "kyc.aadhaar"
```

`*` matches any characters including dots, and `?` matches one. A leading `*.` is
additionally tried against the bare column name, so `*.pan` reads the way it
looks like it should.

### Glob syntax

`deny_tables` and `redact_columns` use the same pattern language:

| Pattern | Matches |
|---|---|
| `*` | Any sequence of characters, including dots |
| `?` | Exactly one character |

Patterns are case-insensitive, and are tested against the bare name and the
qualified name (`table.column`, `schema.table`).

## `databases`

The allowlist. A database is reachable only if it appears here and is not
disabled.

```yaml
databases:
  app:
    connection: primary
    description: Application data
    tags: [prod, core]

  identity:
    connection: primary
    description: Accounts and KYC documents, contains PII
    tags: [prod, pii]
    max_rows: 50
    redact_columns: ["*.pan", "*.email", "*.phone"]

  reserved:
    connection: primary
    enabled: false
```

| Key | Default | Meaning |
|---|---|---|
| `connection` | required | Name from `connections` |
| `database` | the config key | Physical database name, when it differs from the logical name |
| `enabled` | `true` | `false` documents an entry without exposing it |
| `description` | — | Shown by `list_databases`; helps an agent choose |
| `tags` | — | Free-form labels, reported but not enforced |
| `services` | inherits | Per-database service overrides |
| policy keys | inherit | Any key from `defaults`, applied on top |

### Enabling writes for one database

```yaml
databases:
  sandbox:
    connection: primary
    read_only: false
    allow_write: true        # INSERT and UPDATE
    allow_delete: false      # still no DELETE
    allow_ddl: false
    statement_timeout: 5s
    max_affected_rows: 50
    services:
      write: true            # opt into the write tool group
```

### Toggling write mode at runtime

For a database the agent should only *sometimes* write to, skip the YAML and
grant the `admin` service instead. `set_write_mode` then flips that database
between `read-only` and `read-write` for the rest of the session:

```yaml
databases:
  sandbox:
    connection: primary
    services:
      admin: true            # this database's write mode is toggleable
```

Enabling requires `confirm: "enable-writes"`; disabling needs no
confirmation. The override opens INSERT and UPDATE only — DELETE, DDL,
TRUNCATE, caps and timeouts are untouched — lives in memory (a restart drops
it), is shown as `session_override` by `list_databases` and `veridb_status`,
and is audit-logged with the before/after mode. See
[docs/usage.md](usage.md#toggling-write-mode-from-pi) for the pi-side flow.

### Keeping noisy tables out of view

```yaml
    deny_tables:
      - "*.migrations"
      - "databasechangelog*"
      - "_prisma_migrations"
      - "alembic_version"
```

## `audit`

```yaml
audit:
  enabled: true
  sinks: [stderr, file]
  file: tmp/audit/veridb-audit.jsonl
  include_sql: true
  include_params: false
  max_value_len: 512
```

A relative `file` resolves against the config file's directory, not the
process working directory — an MCP client may launch the server from any
session folder, and the trail must still land in one place (where the viewer
looks for it) rather than scattering `tmp/audit/` across projects.
Absolute paths pass through untouched.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Master switch |
| `sinks` | `[stderr]` | Any of `stderr`, `file` |
| `file` | — | Destination for the `file` sink; required when that sink is used |
| `include_sql` | `true` | Store the statement text |
| `include_params` | `false` | Store bind parameters. Off by default because parameters are where PII usually hides |
| `max_value_len` | `512` | Truncate stored strings |

The `file` sink writes JSON Lines, one record per call, with an `O_APPEND` write
so concurrent appends cannot interleave. If a previous process died mid-write,
the dangling final line is closed on the next start, so the following record is
not corrupted. Records cover refusals too, which is usually the interesting part:

```json
{"ts":"2026-01-02T03:04:05Z","tool":"execute","database":"identity","status":"denied",
 "sql":"DELETE FROM profiles","reason":"read_only","error":"refused: database \"identity\" is read-only..."}
```

`status` is `ok`, `denied` (refused before running) or `error` (ran and failed),
and `reason` carries a stable code: `read_only`, `not_allowed`, `missing_where`,
`cap_exceeded`, `keyword`, `schema`, `table`, `multi_statement`, `empty`,
`syntax`, `unsupported`, `identifier`.

## Validation

Startup fails, listing every problem, for: no connections, no databases, an
unknown or missing driver, a connection with neither a DSN nor a host, a missing
`user` when building a DSN from parts, an out-of-range port, a database
referencing an unknown connection, a negative `statement_timeout` or `max_rows`,
an unknown audit sink, or a `file` sink with no path.

Everything else — a database that does not exist, a host that is unreachable — is
a warning, so one shared config can serve several environments.
