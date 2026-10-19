package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/mssql-plugin/internal/mssql"
)

// ------------------------------------------------------------------- read --

type queryInput struct {
	SQL   string `json:"sql"`
	Limit string `json:"limit"`
}

// maxRowLimit is the ceiling on how many rows a read returns. A larger request
// is clamped to it rather than refused, and it is also the default.
const maxRowLimit = 100

func (r *Registry) query() sdkv1.Action {
	return sdkv1.Action{
		Method:      "mssql.query",
		Title:       "Run Query (read)",
		Description: "Run a SELECT (or any row-returning statement) and get the rows back. Pull values from the flow inline with {{$.path}}. At most 100 rows are returned.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-search"},
		Form:        queryForm,
		RequestHandler: run(r, "query", func(ctx context.Context, job *sdkv1.Job, client *mssql.Client, in queryInput) (map[string]any, error) {
			if strings.TrimSpace(in.SQL) == "" {
				return nil, fmt.Errorf("missing required input: sql")
			}
			limit, err := resolveLimit(in.Limit)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "query", Content: preview(in.SQL)})

			res, err := client.Query(ctx, in.SQL, nil, limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"columns":  res.Columns,
				"rows":     res.Rows,
				"rowCount": res.RowCount,
			}, nil
		}),
	}
}

// resolveLimit turns the (already {{$.path}}-resolved) limit field into the row
// cap to apply. Empty means the default; anything over the ceiling is clamped to
// it, so a read never returns more than maxRowLimit rows. A non-number is an
// error, since it is almost always an unresolved token the user meant to fill.
func resolveLimit(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return maxRowLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("max rows must be a whole number up to %d: %q", maxRowLimit, s)
	}
	if n <= 0 || n > maxRowLimit {
		return maxRowLimit, nil
	}
	return n, nil
}

// ------------------------------------------------------------------ write --

type executeInput struct {
	SQL    string   `json:"sql"`
	Params []string `json:"params"`
}

func (r *Registry) execute() sdkv1.Action {
	return sdkv1.Action{
		Method:      "mssql.execute",
		Title:       "Execute (write)",
		Description: "Run an INSERT / UPDATE / DELETE / MERGE and get the affected count. Add an OUTPUT clause to also read back rows. Use @p1, @p2 … and {{$.path}} for values.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-edit"},
		Form:        executeForm,
		RequestHandler: run(r, "execute", func(ctx context.Context, job *sdkv1.Job, client *mssql.Client, in executeInput) (map[string]any, error) {
			if strings.TrimSpace(in.SQL) == "" {
				return nil, fmt.Errorf("missing required input: sql")
			}
			args, err := parseParams(in.Params)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "execute", Content: preview(in.SQL)})

			res, err := client.Exec(ctx, in.SQL, args)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"command":      res.Command,
				"rowsAffected": res.RowsAffected,
				"rowCount":     res.RowCount,
			}
			if len(res.Rows) > 0 {
				out["columns"] = res.Columns
				out["rows"] = res.Rows
			}
			return out, nil
		}),
	}
}

// --------------------------------------------------------- insert / upsert --

// insertInput is a row to write, given one of two ways. The column fields loaded
// into the form arrive under `values`; a pasted JSON object arrives as `record`
// and, when present, wins.
type insertInput struct {
	Schema string         `json:"schema"`
	Table  string         `json:"table"`
	Values map[string]any `json:"values"`
	Record string         `json:"record"`
}

func (r *Registry) insert() sdkv1.Action {
	return sdkv1.Action{
		Method:      "mssql.record.insert",
		Title:       "Insert / Upsert record",
		Description: "Write a row to a table from column fields or a JSON record. On a primary-key clash the existing row is updated instead of inserted (a MERGE upsert). Values accept {{$.path}} tokens.",
		Icon:        sdkv1.Icon{Icon: "mdi-table-row-plus-after"},
		Form:        insertForm,
		RequestHandler: run(r, "insert", func(ctx context.Context, job *sdkv1.Job, client *mssql.Client, in insertInput) (map[string]any, error) {
			table := strings.TrimSpace(in.Table)
			schema := strings.TrimSpace(in.Schema)
			if err := mssql.ValidIdent("table", table); err != nil {
				return nil, err
			}
			if schema != "" {
				if err := mssql.ValidIdent("schema", schema); err != nil {
					return nil, err
				}
			}

			data, err := insertData(job, in)
			if err != nil {
				return nil, err
			}
			if len(data) == 0 {
				return nil, fmt.Errorf("no column values: fill in at least one column field, or a JSON record")
			}

			cols := sortedMapKeys(data)
			for _, col := range cols {
				if err := mssql.ValidIdent("column", col); err != nil {
					return nil, err
				}
			}

			qualified := mssql.Qualify(schema, table)

			// Discover the primary key so a clash can be turned into an update,
			// and the identity column so an explicit value for it is allowed
			// through rather than rejected by the server.
			pk, err := client.PrimaryKeyColumns(ctx, qualified)
			if err != nil {
				return nil, err
			}
			identity, err := client.IdentityColumn(ctx, qualified)
			if err != nil {
				return nil, err
			}

			args := make([]any, len(cols))
			for i, c := range cols {
				args[i] = data[c]
			}
			upsert := len(pk) > 0 && subset(pk, cols)
			statement, command := buildUpsertDDL(qualified, cols, pk, identity)

			job.Progress(50, sdkv1.Frame{Title: "insert", Content: preview(statement)})

			res, err := client.Exec(ctx, statement, args)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				// The command is the one buildUpsertDDL named, not the leading
				// verb of what was sent: an explicit identity value wraps the
				// write in SET IDENTITY_INSERT, and reporting "SET" would say
				// nothing about what the node did.
				"command":      command,
				"rowsAffected": res.RowsAffected,
				"table":        table,
				"upsert":       upsert,
			}
			if len(res.Rows) > 0 {
				out["columns"] = res.Columns
				out["rows"] = res.Rows
				out["row"] = res.Rows[0]
			}
			return out, nil
		}),
	}
}

// insertData turns the action's two input shapes into the column → value map to
// write. A JSON record wins when present; otherwise the loaded column fields are
// used, with each value resolved and typed the way a positional param is.
func insertData(job *sdkv1.Job, in insertInput) (map[string]any, error) {
	// A pasted record. Its {{$.path}} tokens were resolved by run() before it
	// reached here, since Record is a plain string field, so it now parses as
	// ordinary JSON. UseNumber keeps whole numbers off float64.
	if rec := strings.TrimSpace(in.Record); rec != "" {
		dec := json.NewDecoder(strings.NewReader(rec))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("JSON record does not parse as an object: %w", err)
		}
		data := make(map[string]any, len(obj))
		for col, value := range obj {
			data[col] = bindable(normalizeNumbers(value))
		}
		return data, nil
	}

	// Column fields. run()'s reflect walker rewrites strings and []string on the
	// input, but not the values inside this map, so resolve them here. A blank
	// column is omitted so the column's default (or IDENTITY) applies.
	vr := newVarResolver(job)
	data := make(map[string]any, len(in.Values))
	for col, raw := range in.Values {
		s, ok := raw.(string)
		if !ok {
			if raw != nil {
				data[col] = bindable(raw)
			}
			continue
		}
		if s = strings.TrimSpace(vr.resolve(s)); s == "" {
			continue
		}
		data[col] = coerceParam(s)
	}
	return data, nil
}

// buildUpsertDDL renders the write. Values are bound as @p1…@pn, never spliced
// in; only the identifiers — the table and the column names — are put into the
// text, each bracket-quoted so a name cannot continue as SQL.
//
// When every primary-key column is among the values supplied, a clash becomes an
// UPDATE of the other columns (an upsert), expressed as the MERGE that T-SQL
// gives for the job; when only the key is supplied, the clash is a no-op. With no
// usable key — none defined, or an IDENTITY one left out to be generated — there
// is nothing to merge on, so it is a plain insert.
//
// HOLDLOCK on the target is what makes the MERGE safe to run concurrently:
// without it, two callers can both miss the row and both try to insert it.
//
// An explicit value for an IDENTITY column is normally refused by the server. It
// is allowed here, bracketed by SET IDENTITY_INSERT, because supplying the key
// of the row to upsert is the ordinary way to use this action — and silently
// dropping the value the user typed would write a different row than they asked
// for. It needs ALTER permission on the table.
//
// It returns the command the write performs alongside the statement, because the
// IDENTITY_INSERT bracketing makes the batch's own leading verb ("SET") say
// nothing about what the node did.
func buildUpsertDDL(qualified string, cols, pk []string, identity string) (statement, command string) {
	statement = mergeStatement(qualified, cols, pk)
	command = "INSERT"
	if len(pk) > 0 && subset(pk, cols) {
		command = "MERGE"
	}

	if identity != "" && slicesContains(cols, identity) {
		statement = fmt.Sprintf("SET IDENTITY_INSERT %s ON;\n%s\nSET IDENTITY_INSERT %s OFF;",
			qualified, statement, qualified)
	}
	return statement, command
}

// mergeStatement is the upsert (or plain insert) itself, without the
// IDENTITY_INSERT bracketing buildUpsertDDL may add around it.
func mergeStatement(qualified string, cols, pk []string) string {
	quoted := make([]string, len(cols))
	placeholders := make([]string, len(cols))
	sourceCols := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = mssql.QuoteIdent(c)
		placeholders[i] = fmt.Sprintf("@p%d", i+1)
		sourceCols[i] = "source." + quoted[i]
	}

	// No key to conflict on: a plain insert. OUTPUT comes before VALUES in a
	// T-SQL INSERT, and reports the row as the server stored it — defaults and
	// generated keys filled in.
	if len(pk) == 0 || !subset(pk, cols) {
		return fmt.Sprintf("INSERT INTO %s (%s) OUTPUT INSERTED.* VALUES (%s);",
			qualified, strings.Join(quoted, ", "), strings.Join(placeholders, ", "))
	}

	pkSet := make(map[string]bool, len(pk))
	match := make([]string, len(pk))
	for i, c := range pk {
		q := mssql.QuoteIdent(c)
		match[i] = fmt.Sprintf("target.%s = source.%s", q, q)
		pkSet[c] = true
	}

	assigns := make([]string, 0, len(cols))
	for _, c := range cols {
		if pkSet[c] {
			continue // never overwrite the key with itself
		}
		q := mssql.QuoteIdent(c)
		assigns = append(assigns, fmt.Sprintf("target.%s = source.%s", q, q))
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "MERGE %s WITH (HOLDLOCK) AS target\n", qualified)
	fmt.Fprintf(&builder, "USING (VALUES (%s)) AS source (%s)\n",
		strings.Join(placeholders, ", "), strings.Join(quoted, ", "))
	fmt.Fprintf(&builder, "ON %s\n", strings.Join(match, " AND "))
	// Only the key was supplied: there is nothing to update, so a match is left
	// alone — the MERGE carries no WHEN MATCHED clause at all.
	if len(assigns) > 0 {
		fmt.Fprintf(&builder, "WHEN MATCHED THEN UPDATE SET %s\n", strings.Join(assigns, ", "))
	}
	fmt.Fprintf(&builder, "WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)\n",
		strings.Join(quoted, ", "), strings.Join(sourceCols, ", "))
	// A MERGE must be terminated with a semicolon.
	builder.WriteString("OUTPUT INSERTED.*;")
	return builder.String()
}

// subset reports whether every element of sub is in super.
func subset(sub, super []string) bool {
	set := make(map[string]bool, len(super))
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// sortedMapKeys orders a value map's keys, so the generated column list and the
// bound arguments line up deterministically.
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ----------------------------------------------------------- create table --

type createTableInput struct {
	Schema  string `json:"schema"`
	Table   string `json:"table"`
	Columns string `json:"columns"`
}

func (r *Registry) createTable() sdkv1.Action {
	return sdkv1.Action{
		Method:      "mssql.table.create",
		Title:       "Create Table (if not exists)",
		Description: "Create a table only if it does not already exist. Give it either T-SQL column definitions (with a table name) or a whole CREATE TABLE statement; existing tables are left untouched.",
		Icon:        sdkv1.Icon{Icon: "mdi-table-plus"},
		Form:        createTableForm,
		RequestHandler: run(r, "create table", func(ctx context.Context, job *sdkv1.Job, client *mssql.Client, in createTableInput) (map[string]any, error) {
			table := strings.TrimSpace(in.Table)
			columns := strings.TrimSpace(in.Columns)
			if columns == "" {
				return nil, fmt.Errorf("missing required input: columns")
			}

			ddl, err := buildCreateDDL(strings.TrimSpace(in.Schema), table, columns)
			if err != nil {
				return nil, err
			}

			job.Progress(50, sdkv1.Frame{Title: "create table", Content: preview(ddl)})

			if _, err := client.Exec(ctx, ddl, nil); err != nil {
				return nil, err
			}
			// The statement actually sent leads with the existence check, so its
			// own verb is "IF"; the action's verb is what the node did. Whether
			// the table was created or already there is deliberately not
			// reported — the guard means the server does not say, and finding
			// out would cost a second round trip nothing downstream asked for.
			out := map[string]any{"command": "CREATE TABLE", "ddl": ddl}
			if table != "" {
				out["table"] = table
			}
			return out, nil
		}),
	}
}

var (
	// createTableRe matches the head of a CREATE TABLE statement and captures
	// the table name — so a whole statement can be told apart from a bare column
	// list, and the name can be put into the existence check. A Postgres-style
	// IF NOT EXISTS is captured too, in order to drop it: T-SQL has no such
	// clause and rejects the statement outright.
	createTableRe = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(` + identPattern + `)`)
	// ifNotExistsRe recognises the Postgres spelling, so it can be removed.
	ifNotExistsRe = regexp.MustCompile(`(?is)^(\s*CREATE\s+TABLE\s+)IF\s+NOT\s+EXISTS\s+`)
)

// identPattern matches a (possibly quoted, possibly qualified) T-SQL object
// name: findings, dbo.findings, [my schema].[my table], #temp.
const identPattern = `(?:\[[^\]]+\]|"[^"]+"|[\pL_@#][\w$@#]*)(?:\s*\.\s*(?:\[[^\]]+\]|"[^"]+"|[\pL_@#][\w$@#]*)){0,2}`

// buildCreateDDL turns the form input into the statement to run. Two shapes are
// accepted:
//
//   - A whole CREATE TABLE statement (what most people paste). It is run as
//     given — full control over names, types, constraints — with a trailing
//     semicolon trimmed and a Postgres-style IF NOT EXISTS removed, since T-SQL
//     has no such clause.
//   - Bare column definitions, the text inside the parentheses. These are
//     wrapped as CREATE TABLE <table> (...), so a table name is required.
//
// Either way the result is guarded by an OBJECT_ID existence check, which is how
// T-SQL expresses "if not exists" for a table: re-running a flow is then a
// no-op rather than an error.
//
// The table name is an identifier, not a value, so it cannot be a bound
// parameter — a name this plugin builds is bracket-quoted, and one the user
// wrote inside their own statement is left as they wrote it and only quoted
// where it travels as the text OBJECT_ID reads.
func buildCreateDDL(schema, table, columns string) (string, error) {
	if match := createTableRe.FindStringSubmatch(columns); match != nil {
		statement := ifNotExistsRe.ReplaceAllString(columns, "$1")
		statement = strings.TrimRight(strings.TrimSpace(statement), " \t\r\n;")
		return guardCreate(strings.TrimSpace(match[1]), statement), nil
	}

	if table == "" {
		return "", fmt.Errorf("missing required input: table (needed when the definition is only columns, not a full CREATE TABLE statement)")
	}
	if err := mssql.ValidIdent("table", table); err != nil {
		return "", err
	}
	if schema != "" {
		if err := mssql.ValidIdent("schema", schema); err != nil {
			return "", err
		}
	}

	qualified := mssql.Qualify(schema, table)
	return guardCreate(qualified, fmt.Sprintf("CREATE TABLE %s (%s)", qualified, columns)), nil
}

// guardCreate wraps a CREATE TABLE in the existence check T-SQL uses in place of
// IF NOT EXISTS. OBJECT_ID reads the name as text — it is resolved at parse
// time, so it cannot be a parameter — and the 'U' class restricts the check to
// user tables, so a view or a procedure of the same name is not mistaken for one.
func guardCreate(name, statement string) string {
	return fmt.Sprintf("IF OBJECT_ID(N%s, N'U') IS NULL\nBEGIN\n%s\nEND",
		mssql.QuoteLiteral(name), statement)
}

// ---------------------------------------------------------------- helpers --

// parseParams turns the positional param strings the form collects into the
// typed `@p1…@pn` arguments the driver binds.
//
// Each entry is read as a JSON scalar so a value carries its type: 42 is an
// integer, true a bit, null the SQL NULL. Anything that is not valid JSON — a
// bare word, an e-mail, a zero-padded code like 007 — is a plain string, which
// is almost always what a hand-typed value is. To force a value to stay a
// string, quote it: "42".
func parseParams(params []string) ([]any, error) {
	if len(params) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(params))
	for _, p := range params {
		args = append(args, coerceParam(p))
	}
	return args, nil
}

// coerceParam reads one param string. A clean JSON value wins; otherwise the raw
// string is the value.
func coerceParam(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	// A trailing token means it was not a single JSON value (e.g. `1 2`), so it
	// is really a string.
	if dec.More() {
		return s
	}
	return bindable(normalizeNumbers(v))
}

// bindable makes a decoded JSON value something the driver can bind. SQL Server
// has no native JSON type — JSON is stored as nvarchar and read with the JSON
// functions — so an object or an array is bound as its JSON text rather than
// refused as an unsupported parameter type. Scalars pass straight through.
func bindable(value any) any {
	switch value.(type) {
	case map[string]any, []any:
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return value
}

// normalizeNumbers walks a decoded JSON value and turns json.Number into int64
// (when integral) or float64, so a whole number does not reach the driver as a
// float and get bound to an int column as 42.0.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case []any:
		for i := range t {
			t[i] = normalizeNumbers(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = normalizeNumbers(t[k])
		}
		return t
	default:
		return v
	}
}

// preview trims SQL to one short line for a progress frame.
func preview(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > 120 {
		return sql[:117] + "…"
	}
	return sql
}
