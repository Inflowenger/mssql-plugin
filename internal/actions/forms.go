package actions

import (
	"github.com/Inflowenger/go-plugin-sdk/formkit"
)

// Every form this plugin serves is declared here with the SDK's formkit builder,
// which generates the JSON Schema and the JSON Forms UI schema from one
// statement per field. A malformed form panics at start-up (Build calls
// Validate), where it is a compile-time-shaped mistake rather than a dialog that
// will not open.

const (
	descSQL    = `T-SQL to run. Bind values with @p1, @p2 … (never string-concatenate them). Pull values from the flow with {{$.path}}, e.g. WHERE id = @p1 with the first parameter set to {{$.trigger.userId}}.`
	descParams = `Values for the @p1, @p2 … placeholders in the SQL above, in order: the first row is @p1, the second @p2, and so on. They are bound to the prepared statement, never pasted into the SQL text, so a value can never alter the query. Each is read as JSON so it keeps its type — 42 an integer, true a bit, null the SQL NULL — and anything that is not JSON is a plain string. A JSON object or array is bound as its JSON text, which is how SQL Server stores JSON (in an nvarchar column). A value may be a {{$.path}} token; to force text, quote it: "42".`

	descQuerySQL = `SELECT (or any row-returning statement) to run. Pull values straight from the flow with {{$.path}}, e.g. WHERE tenant = '{{$.trigger.tenantId}}'.`
	descLimit    = `Most rows to return. Capped at 100 — a larger number is treated as 100. T-SQL has no LIMIT, so this is applied as the rows are read: add a TOP (n) to the statement to make the server do less work. Accepts a {{$.path}} token that resolves to a number.`

	descInsertRecord = `Optional JSON object of column → value, e.g. {"finding_id": 7, "severity": "high"}. When given, it wins over the column fields above. Values may be {{$.path}} tokens: {"name": "{{$.user.name}}"}. On a primary-key clash the row is updated instead (upsert).`
)

// ---------------------------------------------------------------- settings --

// settingsForm is stored by the platform as a reusable settings profile and
// shipped with every call as body.settings. The plugin keeps nothing.
//
// A whole DSN or the discrete fields — either works; a DSN, when given, wins.
var settingsForm = formkit.New("SQL Server connection").
	Describe("Stored by the platform as a reusable settings profile and shipped with every call as body.settings. The plugin keeps nothing. Give a full connection string, or fill in the fields below.").
	SubmitTo("mssql.meta.ping.check").
	Add(
		formkit.Text("dsn", "Connection string").
			Describe(`sqlserver://user:pass@host:1433?database=mydb&encrypt=true — or the ADO form, server=host;user id=sa;password=…;database=mydb. Given, it wins; leave empty to use the fields below.`),
		formkit.Text("host", "Host").
			Describe(`e.g. localhost, sql.internal, or myserver.database.windows.net. A named instance may be written here as HOST\INSTANCE.`),
		formkit.Integer("port", "Port").Default(1433).
			Describe("1433 unless the instance was moved. Leave the default when a named instance is given below — the SQL Browser service resolves the port."),
		formkit.Text("instance", "Named instance").
			Describe("Optional, e.g. SQLEXPRESS. Only for an instance that is not the default one on the host."),
		formkit.Text("database", "Database").
			Describe("e.g. master, or the application's own database. Empty connects to the login's default database."),
		formkit.Text("user", "User").
			Describe("A SQL Server login, e.g. sa — or user@tenant for Azure SQL. Leave empty only for a connection string that carries its own credentials."),
		formkit.Secret("password", "Password"),
		formkit.Enum("encrypt", "Encrypt", "false", "true", "strict", "disable").
			Default("false").
			Describe("false negotiates encryption and encrypts the login only (the driver's default). true encrypts the whole session and is required by Azure SQL. strict demands TDS 8.0. disable turns it off entirely."),
		formkit.Bool("trustServerCertificate", "Trust server certificate").
			Default(false).
			Describe("Skip certificate validation. A development or on-premises server with a self-signed certificate needs this once Encrypt is true; a managed service with a real certificate does not."),
		// The button hangs off the last field and its answer appears under it, so
		// a bad connection is reported while it is being entered rather than by
		// every node that later fails. It writes no value — saying what it found
		// is the whole point.
		formkit.Text("test", "Test connection").
			Describe("Press ↻ to check these values against the live server before saving.").
			Lookup("mssql.meta.ping.check", "Test connection"),
	).
	Build()

// ------------------------------------------------------------------ actions --

var queryForm = formkit.New("Run query (read)").Add(
	formkit.TextArea("sql", "SQL").Required().
		Describe(descQuerySQL).
		Help("Read-only intent: use this for SELECT. Pull values from the flow inline with {{$.path}}. Writes belong on the Execute or Insert action."),
	formkit.Text("limit", "Max rows").Default("100").
		Describe(descLimit),
).Build()

var executeForm = formkit.New("Execute (write)").Add(
	formkit.TextArea("sql", "SQL").Required().
		Describe(descSQL).
		Help("INSERT / UPDATE / DELETE / MERGE. Add an OUTPUT clause to read rows back alongside the affected count. Example: UPDATE orders SET status = @p1 WHERE id = @p2 — with Parameters row 1 = shipped (→ @p1) and row 2 = 42 (→ @p2)."),
	formkit.List("params", "Parameters (@p1, @p2 …)").Describe(descParams).
		Help("One row per placeholder in the SQL above, in order: row 1 fills @p1, row 2 fills @p2, … For the example query: row 1 = shipped, row 2 = 42."),
).Build()

// insertFormDef is kept as the builder (not just its Built form) because the
// columns-load meta rebuilds the dialog from it — SchemaMap / UIMap give it a
// fresh copy to splice the discovered column fields into. See columnsEnvelope.
var insertFormDef = formkit.New("Insert / upsert record").
	Describe("Add a row to a table. Pick the table, press ↻ to load its columns as value fields, and fill the ones you want — or paste a JSON record below. If a row with the same primary key already exists, it is updated instead of inserted.").
	Add(
		formkit.Text("schema", "Schema").
			Describe("Optional, e.g. dbo. Empty uses the login's default schema."),
		// One button does both steps: with the box empty it lists the tables to
		// pick from; once a table is set, pressing it again loads that table's
		// columns as the value fields below. See metaTablePick.
		formkit.Text("table", "Table").Required().
			Describe("The table to write to. Press ↻ with the box empty to list tables; pick one, then press ↻ again to load its columns as value fields below.").
			Lookup("mssql.meta.table.pick", "Tables / columns"),
		formkit.TextArea("record", "JSON record (optional)").
			Describe(descInsertRecord),
	)

var insertForm = insertFormDef.Build()

var createTableForm = formkit.New("Create table (if not exists)").Add(
	formkit.Text("schema", "Schema").Describe("Optional, e.g. dbo. Empty uses the login's default schema. Used only with column definitions; ignored when you paste a full statement."),
	formkit.Text("table", "Table name").
		Describe("Identifier only, e.g. findings. Required when the definition below is only columns; not needed when you paste a full CREATE TABLE statement."),
	formkit.TextArea("columns", "Column definitions or full statement").Required().
		Describe(`Either a whole CREATE TABLE statement — CREATE TABLE findings (finding_id int primary key, severity nvarchar(20) not null, ...) — or just the column definitions that go inside the parentheses — finding_id int primary key, severity nvarchar(20) not null. This is T-SQL, not a JSON record. T-SQL has no IF NOT EXISTS on CREATE TABLE, so the statement is wrapped in an existence check for you.`),
).Build()
