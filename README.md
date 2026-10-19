# Inflowenger Microsoft SQL Server plugin

A SQL Server node for the Inflowenger workflow canvas, built on the
`go-plugin-sdk` (`sdkv1`) under the **inflowv1** protocol. It runs as a
long-running Go process and is called by the Fractal runtime over NATS.

It talks to any TDS server the `microsoft/go-mssqldb` driver supports —
SQL Server 2012 and later, Azure SQL Database, Azure SQL Managed Instance, and
SQL Server on Linux or in a container.

The plugin holds **no** database configuration. A connection travels in every
call as a settings profile (`body.settings`), so one running plugin serves many
databases and rotating a password needs no redeploy.

## Actions

| Action | Method | What it does |
| --- | --- | --- |
| **Run Query (read)** | `mssql.query` | Runs a SELECT (or any row-returning statement) and returns the rows, their column order, and a count. Pull values from the flow inline with `{{$.path}}`. Returns **at most 100 rows**. |
| **Execute (write)** | `mssql.execute` | Runs an INSERT / UPDATE / DELETE / MERGE and returns the command verb and affected count. Add an `OUTPUT` clause to also read rows back. |
| **Insert / Upsert record** | `mssql.record.insert` | Writes a row to a table from per-column fields or a JSON record. On a primary-key clash the existing row is **updated** (a `MERGE` upsert). |
| **Create Table (if not exists)** | `mssql.table.create` | Creates a table only when it does not already exist; existing tables are left untouched. |

## Run Query (read)

Just a **SQL** field and a **Max rows** field — no positional parameters. Pull
values from the flow inline with `{{$.path}}`:

```sql
SELECT id, email FROM users WHERE org_id = '{{$.trigger.orgId}}' AND active = 1
```

**Max rows** is capped at **100**: leave it blank for the default of 100, and any
larger number (or a `{{$.path}}` token that resolves to one) is treated as 100.

T-SQL has no `LIMIT` clause to push that cap into, so it is applied as the rows
are read — the server still computes the whole result set. Add a `TOP (n)` to
the statement when you want the server to do less work:

```sql
SELECT TOP (50) id, email FROM users ORDER BY created_at DESC
```

## Insert / Upsert record

Write one row to a table, given one of two ways:

- **Per-column fields.** The **Table** field has a single ↻ *Tables / columns*
  button: press it with the box empty to list the tables, pick one, then press it
  again to load that table's columns as value fields. Fill the ones you want; each
  accepts a literal or a `{{$.path}}` token, and a blank column is omitted so its
  default (or `IDENTITY`) applies.
- **A JSON record.** Paste a `{ "column": value }` object into **JSON record**; it
  wins over the column fields when present. Values may be `{{$.path}}` tokens:
  ```json
  { "finding_id": 7, "severity": "high", "name": "{{$.event.title}}" }
  ```

The primary key is discovered automatically. When every key column is supplied,
a clash becomes a `MERGE … WHEN MATCHED THEN UPDATE` of the other columns (a true
upsert); when only the key is supplied, a match is left alone; with no usable key
(none defined, or an `IDENTITY` one left out to be generated) it is a plain
insert. Either way the written row comes back via `OUTPUT INSERTED.*`.

The `MERGE` is taken out under `WITH (HOLDLOCK)`, which is what makes it safe to
run concurrently — without it, two flows can both miss the row and both try to
insert it.

**An explicit value for an `IDENTITY` column is allowed.** SQL Server normally
refuses one, but supplying the key of the row you want to upsert is the ordinary
way to use this action, so the statement is bracketed with
`SET IDENTITY_INSERT … ON/OFF` when (and only when) a value for that column is
given. That needs `ALTER` permission on the table. Leave the column blank and
the server generates the key as usual.

## Create Table

The definition field accepts **T-SQL** in either shape:

- **A whole `CREATE TABLE` statement** (what most people paste):
  ```sql
  CREATE TABLE findings (
      finding_id   INT PRIMARY KEY,
      finding_name NVARCHAR(255) NOT NULL,
      severity     NVARCHAR(20) NOT NULL,
      created_at   DATETIME2 NOT NULL
  );
  ```
  Run as given; the table-name field is not needed.
- **Just the column definitions** — the text inside the parentheses — with the
  table name supplied separately:
  ```
  finding_id int primary key, finding_name nvarchar(255) not null, severity nvarchar(20) not null
  ```

T-SQL has **no `IF NOT EXISTS` on `CREATE TABLE`** — the server rejects the
statement outright. So either shape is wrapped in the existence check T-SQL uses
instead, and re-running a flow is a no-op rather than an "object already exists"
failure:

```sql
IF OBJECT_ID(N'[dbo].[findings]', N'U') IS NULL
BEGIN
CREATE TABLE [dbo].[findings] (...)
END
```

If you paste a `CREATE TABLE IF NOT EXISTS …` out of habit from the Postgres or
MySQL sibling, the clause is stripped for you rather than sent to a server that
would refuse it. Creating a table makes it **empty** — inserting rows is the
Execute action's job.

## Parameters, safely

Never string-concatenate user input into SQL. Bind values with `@p1, @p2, …` and
supply them positionally in the **Parameters** list:

```sql
SELECT id, email FROM users WHERE org_id = @p1 AND active = @p2
```

```
params: ["42", "true"]
```

Each parameter is read as a JSON scalar so it keeps its type:

| You type | Bound as |
| --- | --- |
| `42` | integer |
| `42.5` | float |
| `true` | bit |
| `null` | SQL NULL |
| `{"k": 1}` | the JSON **text** `{"k":1}` — see below |
| `alice`, `user@x.com`, `007` | string (not valid JSON → left as text) |
| `"42"` | string (quoted → forced text) |

SQL Server has no native JSON column type: JSON lives in an `NVARCHAR` column and
is read with `JSON_VALUE` / `OPENJSON`. So a JSON object or array is bound as its
compact JSON text, which is exactly what such a column takes — rather than being
refused as a parameter type the driver cannot send.

## `{{JSONPATH}}` — pull values from the flow

Any string input — the SQL text and every parameter — is resolved against the
flow scope before it runs. Write `{{$.path}}` and the runtime substitutes the
value from upstream nodes:

```sql
UPDATE orders SET status = @p1 WHERE id = @p2
```

```
params: ["shipped", "{{$.trigger.orderId}}"]
```

A token the scope can't supply is left verbatim rather than silently dropped.
Each distinct path is fetched from the runtime once per call, however many
fields reference it.

## What a write reports

`rowsAffected` comes from the server for a statement that returns no rows. For a
statement that **does** return rows — one carrying an `OUTPUT` clause — it is the
number of rows `OUTPUT` reported, which for `OUTPUT` is the same number.

The reason for the split is that TDS answers a write with a count and a result
set on different calls: `ExecContext` gives the count but discards rows,
`QueryContext` gives the rows but no count. The plugin picks the path by looking
at the statement (comments and string literals are ignored, so a column called
`output_path` does not fool it).

`command` is the statement's leading verb — `INSERT`, `UPDATE`, `MERGE`. Unlike a
PostgreSQL command tag it is read off the SQL text, because the protocol never
sends back the name of what ran. A statement that opens with a CTE therefore
reports `WITH`.

## Column types, in JSON

| SQL Server type | Comes back as |
| --- | --- |
| `int`, `bigint`, `smallint`, `tinyint` | number |
| `bit` | boolean |
| `decimal`, `numeric`, `money` | **string** — the exact decimal text, not forced through a float |
| `datetime2`, `datetime`, `date`, `time`, `datetimeoffset` | RFC 3339 string |
| `uniqueidentifier` | the canonical lower-case GUID string |
| `nvarchar`, `varchar`, `text`, `xml` | string |
| `varbinary`, `binary`, `image` | the text if it is valid UTF-8, otherwise base64 |

## Connection (settings profile)

The plugin's set-up form asks for either a full connection string **or** the
discrete fields:

- **Connection string** — `sqlserver://user:pass@host:1433?database=mydb&encrypt=true`,
  or the ADO form `server=host;user id=sa;password=…;database=mydb`. Given, it wins.
- **Host / Port / Named instance / Database / User / Password / Encrypt /
  Trust server certificate** — used when no connection string is set.

A host may be written the way SQL Server tooling writes it — `HOST\INSTANCE` or
`HOST,1433` — and the instance or port is split out for you. When a named
instance is given, the port is left off so the SQL Browser service can resolve it.

**Encrypt** is the driver's own parameter:

| Value | Meaning |
| --- | --- |
| `false` | (default) negotiate; the login packet is encrypted, the session is not |
| `true` | encrypt the whole session — **required by Azure SQL** |
| `strict` | demand TDS 8.0 |
| `disable` | no encryption at all |

**Trust server certificate** skips certificate validation. A development or
on-premises server with a self-signed certificate needs it once Encrypt is `true`;
a managed service with a real certificate does not.

Press **Test connection** (↻) to check the values against the live server before
saving. Keys are matched leniently (case, spaces, dashes and underscores are
ignored, plus the usual synonyms — `server`, `user id`, `initial catalog`), so a
hand-typed or imported profile still resolves.

The connection string is built in the URL form rather than the ADO
keyword/value form because every part of a URL is percent-escaped: a password
holding a `;` or a space travels intact instead of splitting the string into
extra settings.

## Running

Provisioning is a prerequisite: the plugin must be defined in a space to get
`PLUGIN_ID`, `INFRA_CRED` (base64) and `INFRA_URL`. Put them in `.env.inflow`
(see `.env.inflow.example`) — **SQL Server credentials never go here**, they are
a settings profile.

```sh
go build ./...
go run .
```

The SDK logs each subscribed subject on startup. Add the node to a flow, bind a
settings profile, and run it.

## Tests

```sh
go test ./...
```

The unit tests are hermetic — they cover the generated SQL, the parameter typing,
the connection-string building, and the statement classification without a
server.

The two integration tests need a live SQL Server and are skipped without one.
They cover what only a real server can answer: the `OBJECT_ID` guard and its
no-op re-run, a `MERGE` that inserts and then updates, the `IDENTITY` rules
(generated key, forced key under `IDENTITY_INSERT`, and an upsert on that key),
typed parameters, the `decimal` / `uniqueidentifier` / `datetime2` rendering, and
the row cap.

```sh
docker run -d --name mssql-it -p 1433:1433 \
  -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Your_password123 \
  mcr.microsoft.com/mssql/server:2022-latest

MSSQL_TEST_DSN="sqlserver://sa:Your_password123@localhost:1433?database=master&encrypt=disable" \
  go test ./internal/actions -run TestIntegration -v
```

## Layout

```
main.go                     wiring: intro, settings, actions, block after Start()
internal/mssql/             connection profile → database/sql pool → Query / Exec / Ping
  settings.go               the profile → Config → sqlserver:// URL, and the client cache
  client.go                 the pool, the two write paths, column-type normalisation
  sqltext.go                identifier quoting, and classifying a statement
internal/actions/           the four actions, the {{JSONPATH}} resolver, forms, the table/column + connection-test metas
```

## Siblings

The same four actions, same forms, same `{{$.path}}` tokens, against other
engines: [`postgres`](../postgres) (Go), [`mysql`](../mysql) (TypeScript),
[`clickhouse`](../clickhouse) (TypeScript), [`mongo`](../mongo) (Go). The
dialect-specific differences in this one are the `@p1` placeholders, the `MERGE`
upsert, the `OBJECT_ID` existence check, and the row cap being applied on read
rather than as a `LIMIT`.
