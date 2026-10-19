package actions

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFormsBuild fails if any package-level form is malformed: formkit.Build
// panics on an invalid form, and merely loading this test package evaluates
// every form var, so a broken form cannot reach a user as a dialog that will
// not open.
func TestFormsBuild(t *testing.T) {
	r := New()
	if got := len(r.All()); got != 4 {
		t.Fatalf("want 4 actions, got %d", got)
	}
	for _, a := range r.All() {
		if a.Method == "" || a.RequestHandler == nil {
			t.Fatalf("action %q is incomplete", a.Title)
		}
		if a.Form.Jsonschema == "" {
			t.Fatalf("action %q has no schema", a.Method)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(a.Form.Jsonschema), &doc); err != nil {
			t.Fatalf("action %q schema does not parse: %v", a.Method, err)
		}
	}
	if r.SettingsForm().Jsonschema == "" {
		t.Fatal("settings form has no schema")
	}
}

// TestMetasAreRegistered guards the silent failure mode of a form button: the
// method a Lookup names must be a meta the plugin actually serves, or pressing
// the button does nothing at all.
func TestMetasAreRegistered(t *testing.T) {
	r := New()
	served := make(map[string]bool)
	for _, m := range r.Metas() {
		if m.RequestHandler == nil {
			t.Fatalf("meta %q has no handler", m.Method)
		}
		served[m.Method] = true
	}

	forms := []string{r.SettingsForm().Jsonui, insertForm.Jsonui}
	for _, ui := range forms {
		for _, fn := range lookupFns(t, ui) {
			if !served[fn] {
				t.Errorf("a form button calls %q, which is not a registered meta", fn)
			}
		}
	}
	if submit := r.SettingsForm().SubmitTo; submit != "" && !served[submit] {
		t.Errorf("the settings form submits to %q, which is not a registered meta", submit)
	}
}

// lookupFns pulls every x-inflow-ui action function name out of a UI schema.
func lookupFns(t *testing.T, ui string) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(ui), &doc); err != nil {
		t.Fatalf("UI schema does not parse: %v", err)
	}

	var found []string
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if hints, ok := typed["x-inflow-ui"].(map[string]any); ok {
				if action, ok := hints["action"].(map[string]any); ok {
					if fn, ok := action["fn"].(string); ok {
						found = append(found, fn)
					}
					// action.name is always the literal "pluginFn" — the host's
					// only action. Anything else fails silently.
					if name, ok := action["name"].(string); ok && name != "pluginFn" {
						t.Errorf("action.name is %q, want pluginFn", name)
					}
				}
			}
			for _, v := range typed {
				walk(v)
			}
		case []any:
			for _, v := range typed {
				walk(v)
			}
		}
	}
	walk(doc)
	return found
}

func TestCoerceParam(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"42", int64(42)},
		{"42.5", 42.5},
		{"true", true},
		{"null", nil},
		{"alice", "alice"},
		{"007", "007"}, // invalid JSON (leading zero) → stays a string
		{`"42"`, "42"}, // quoted → forced string
		{"", ""},       // empty stays empty string
		{"1 2", "1 2"}, // trailing token → string
		{"user@x.com", "user@x.com"},
	}
	for _, c := range cases {
		if got := coerceParam(c.in); got != c.want {
			t.Errorf("coerceParam(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// TestCoerceParamJSONDocument covers the one place SQL Server differs from
// Postgres here: it has no native JSON column type, so an object or an array is
// bound as its JSON text (which is what an nvarchar column holds) rather than
// handed to the driver as a map it cannot bind.
func TestCoerceParamJSONDocument(t *testing.T) {
	got := coerceParam(`{"k": 1, "s": "v"}`)
	text, ok := got.(string)
	if !ok {
		t.Fatalf("want a bindable string, got %T", got)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(text), &round); err != nil {
		t.Fatalf("the bound text is not JSON: %v (%q)", err, text)
	}
	if round["k"] != float64(1) || round["s"] != "v" {
		t.Errorf("the document did not survive: %q", text)
	}

	if got := coerceParam(`[1, 2, 3]`); got != "[1,2,3]" {
		t.Errorf("an array should bind as its JSON text, got %#v", got)
	}
}

func TestParseParams(t *testing.T) {
	args, err := parseParams([]string{"shipped", "42", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 {
		t.Fatalf("want 3 args, got %d", len(args))
	}
	if args[0] != "shipped" || args[1] != int64(42) || args[2] != true {
		t.Errorf("params did not keep their types: %#v", args)
	}
	if args, err := parseParams(nil); err != nil || args != nil {
		t.Errorf("no params should bind nothing, got %#v (%v)", args, err)
	}
}

func TestResolveLimit(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 100, false},     // empty → default/ceiling
		{"25", 25, false},    // within range
		{"100", 100, false},  // the ceiling itself
		{"5000", 100, false}, // clamped down to the ceiling
		{"0", 100, false},    // non-positive → ceiling, never unlimited
		{"-3", 100, false},   // negative → ceiling
		{"  10 ", 10, false}, // surrounding space tolerated
		{"{{$.n}}", 0, true}, // unresolved token is an error, not silently 0
		{"lots", 0, true},    // non-numeric is an error
	}
	for _, c := range cases {
		got, err := resolveLimit(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("resolveLimit(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveLimit(%q) errored: %v", c.in, err)
		} else if got != c.want {
			t.Errorf("resolveLimit(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBuildUpsertDDL(t *testing.T) {
	// Every PK column present → a clash updates the non-key columns. HOLDLOCK is
	// what makes the MERGE safe to run concurrently, so it is asserted here.
	got, command := buildUpsertDDL("[dbo].[findings]", []string{"finding_id", "name", "severity"}, []string{"finding_id"}, "")
	want := "MERGE [dbo].[findings] WITH (HOLDLOCK) AS target\n" +
		"USING (VALUES (@p1, @p2, @p3)) AS source ([finding_id], [name], [severity])\n" +
		"ON target.[finding_id] = source.[finding_id]\n" +
		"WHEN MATCHED THEN UPDATE SET target.[name] = source.[name], target.[severity] = source.[severity]\n" +
		"WHEN NOT MATCHED THEN INSERT ([finding_id], [name], [severity]) VALUES (source.[finding_id], source.[name], source.[severity])\n" +
		"OUTPUT INSERTED.*;"
	if got != want {
		t.Errorf("upsert:\n got %q\nwant %q", got, want)
	}
	if command != "MERGE" {
		t.Errorf("command = %q, want MERGE", command)
	}

	// A composite key, and only the key columns supplied → there is nothing to
	// update, so a match is left alone: no WHEN MATCHED clause at all.
	got, _ = buildUpsertDDL("[membership]", []string{"org_id", "user_id"}, []string{"org_id", "user_id"}, "")
	want = "MERGE [membership] WITH (HOLDLOCK) AS target\n" +
		"USING (VALUES (@p1, @p2)) AS source ([org_id], [user_id])\n" +
		"ON target.[org_id] = source.[org_id] AND target.[user_id] = source.[user_id]\n" +
		"WHEN NOT MATCHED THEN INSERT ([org_id], [user_id]) VALUES (source.[org_id], source.[user_id])\n" +
		"OUTPUT INSERTED.*;"
	if got != want {
		t.Errorf("key-only upsert:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "WHEN MATCHED") {
		t.Error("a key-only upsert must not update the key with itself")
	}

	// No primary key → plain insert. OUTPUT comes before VALUES in T-SQL.
	got, plainCommand := buildUpsertDDL("[logs]", []string{"message"}, nil, "")
	if want := "INSERT INTO [logs] ([message]) OUTPUT INSERTED.* VALUES (@p1);"; got != want {
		t.Errorf("no-pk insert:\n got %q\nwant %q", got, want)
	}
	if plainCommand != "INSERT" {
		t.Errorf("command = %q, want INSERT", plainCommand)
	}

	// An IDENTITY key left out (not among the values) → nothing to merge on, so
	// a plain insert, and no IDENTITY_INSERT since no value is being forced.
	got, _ = buildUpsertDDL("[events]", []string{"name"}, []string{"id"}, "id")
	if want := "INSERT INTO [events] ([name]) OUTPUT INSERTED.* VALUES (@p1);"; got != want {
		t.Errorf("identity-key insert:\n got %q\nwant %q", got, want)
	}
}

// TestBuildUpsertDDLIdentityInsert covers the SQL Server rule that an explicit
// value for an IDENTITY column is refused unless IDENTITY_INSERT is on. Giving
// the key of the row to upsert is the ordinary way to use this action, so the
// value is allowed through rather than dropped.
func TestBuildUpsertDDLIdentityInsert(t *testing.T) {
	got, command := buildUpsertDDL("[dbo].[findings]", []string{"id", "name"}, []string{"id"}, "id")

	if !strings.HasPrefix(got, "SET IDENTITY_INSERT [dbo].[findings] ON;\n") {
		t.Errorf("an explicit identity value should be bracketed by IDENTITY_INSERT:\n%s", got)
	}
	if !strings.HasSuffix(got, "\nSET IDENTITY_INSERT [dbo].[findings] OFF;") {
		t.Errorf("IDENTITY_INSERT must be turned back off:\n%s", got)
	}
	if !strings.Contains(got, "MERGE [dbo].[findings] WITH (HOLDLOCK) AS target") {
		t.Errorf("the upsert itself should still be a MERGE:\n%s", got)
	}
	// The batch leads with SET, which says nothing about what the node did.
	if command != "MERGE" {
		t.Errorf("command = %q, want MERGE", command)
	}

	// The identity column is not among the values → no bracketing.
	plain, _ := buildUpsertDDL("[dbo].[findings]", []string{"name"}, []string{"id"}, "id")
	if strings.Contains(plain, "IDENTITY_INSERT") {
		t.Errorf("no explicit identity value should mean no IDENTITY_INSERT:\n%s", plain)
	}
}

func TestBuildCreateDDL(t *testing.T) {
	// Bare column definitions are wrapped with the quoted, schema-qualified
	// name, inside the existence check T-SQL uses in place of IF NOT EXISTS.
	got, err := buildCreateDDL("dbo", "findings", "id int primary key, name nvarchar(255)")
	if err != nil {
		t.Fatal(err)
	}
	want := "IF OBJECT_ID(N'[dbo].[findings]', N'U') IS NULL\n" +
		"BEGIN\n" +
		"CREATE TABLE [dbo].[findings] (id int primary key, name nvarchar(255))\n" +
		"END"
	if got != want {
		t.Errorf("columns mode:\n got %q\nwant %q", got, want)
	}

	// A full statement runs as given, gains the guard, and loses its ';'.
	full := "CREATE TABLE findings (\n  id int primary key,\n  name nvarchar(255) not null\n);"
	got, err = buildCreateDDL("", "", full)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "IF OBJECT_ID(N'findings', N'U') IS NULL\nBEGIN\nCREATE TABLE findings (") {
		t.Errorf("the pasted name should be read out of the statement and guarded, got %q", got)
	}
	if !strings.HasSuffix(got, "\nEND") {
		t.Errorf("the guard should be closed, got %q", got)
	}
	if strings.Contains(got, ");\n") {
		t.Errorf("the trailing semicolon should be trimmed, got %q", got)
	}
	if !strings.Contains(got, "nvarchar(255) not null") {
		t.Errorf("the body should be preserved, got %q", got)
	}

	// A quoted, qualified name in a pasted statement is read whole.
	got, err = buildCreateDDL("", "", `CREATE TABLE [my schema].[my table] (id int)`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "IF OBJECT_ID(N'[my schema].[my table]', N'U') IS NULL") {
		t.Errorf("a quoted qualified name should be read whole, got %q", got)
	}

	// Column-only definitions without a table name are an error: there is
	// nothing to name the table being created.
	if _, err := buildCreateDDL("", "", "id int"); err == nil {
		t.Error("columns without a table name should error")
	}

	// A name that cannot be safely quoted is refused rather than spliced in.
	if _, err := buildCreateDDL("", "find\nings", "id int"); err == nil {
		t.Error("a table name with a line break should be refused")
	}
}

// TestBuildCreateDDLStripsPostgresClause covers the habit a user brings from the
// sibling plugins: T-SQL has no IF NOT EXISTS on CREATE TABLE and rejects the
// statement outright, so the clause is removed and expressed as the guard.
func TestBuildCreateDDLStripsPostgresClause(t *testing.T) {
	got, err := buildCreateDDL("", "", "create table if not exists findings (id int)")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(got), "IF NOT EXISTS") {
		t.Errorf("the Postgres clause should be gone, got %q", got)
	}
	want := "IF OBJECT_ID(N'findings', N'U') IS NULL\n" +
		"BEGIN\n" +
		"create table findings (id int)\n" +
		"END"
	if got != want {
		t.Errorf("stripped statement:\n got %q\nwant %q", got, want)
	}
}

func TestSortedMapKeysAndSubset(t *testing.T) {
	// The column list and the bound arguments must line up, which is why the
	// keys are ordered rather than taken in map order.
	keys := sortedMapKeys(map[string]any{"c": 3, "a": 1, "b": 2})
	if strings.Join(keys, ",") != "a,b,c" {
		t.Errorf("keys not sorted: %v", keys)
	}

	if !subset([]string{"a"}, []string{"a", "b"}) {
		t.Error("a is a subset of a,b")
	}
	if subset([]string{"a", "z"}, []string{"a", "b"}) {
		t.Error("a,z is not a subset of a,b")
	}
	if !subset(nil, []string{"a"}) {
		t.Error("an empty key list is a subset of anything")
	}
}

func TestPreview(t *testing.T) {
	// A progress frame gets one short line, whatever the statement looked like.
	if got := preview("SELECT\n  a,\n  b\nFROM t"); got != "SELECT a, b FROM t" {
		t.Errorf("preview did not flatten: %q", got)
	}
	long := preview(strings.Repeat("SELECT a FROM t ", 40))
	if len([]rune(long)) > 118 {
		t.Errorf("preview should be trimmed, got %d runes", len([]rune(long)))
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("a trimmed preview should say so, got %q", long)
	}
}
