package mssql

import "testing"

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"findings", "[findings]"},
		{"my table", "[my table]"},
		{"order", "[order]"},                    // a keyword needs the brackets
		{"we]rd", "[we]]rd]"},                   // a closing bracket is doubled
		{"a]]b", "[a]]]]b]"},                    // already-doubled brackets double again
		{"drop] table [x", "[drop]] table [x]"}, // cannot end the quoting early
	}
	for _, c := range cases {
		if got := QuoteIdent(c.in); got != c.want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQualify(t *testing.T) {
	if got, want := Qualify("dbo", "findings"), "[dbo].[findings]"; got != want {
		t.Errorf("Qualify = %q, want %q", got, want)
	}
	if got, want := Qualify("", "findings"), "[findings]"; got != want {
		t.Errorf("Qualify with no schema = %q, want %q", got, want)
	}
	if got, want := Qualify("  ", "findings"), "[findings]"; got != want {
		t.Errorf("Qualify with blank schema = %q, want %q", got, want)
	}
}

func TestQuoteLiteral(t *testing.T) {
	if got, want := QuoteLiteral("dbo.findings"), "'dbo.findings'"; got != want {
		t.Errorf("QuoteLiteral = %q, want %q", got, want)
	}
	// A quote inside the value is doubled, so it cannot end the literal and
	// continue as SQL.
	if got, want := QuoteLiteral("o'brien"), "'o''brien'"; got != want {
		t.Errorf("QuoteLiteral with a quote = %q, want %q", got, want)
	}
}

func TestValidIdent(t *testing.T) {
	if err := ValidIdent("table", "findings"); err != nil {
		t.Errorf("a plain name should be valid: %v", err)
	}
	if err := ValidIdent("table", "my table"); err != nil {
		t.Errorf("a name with a space is quotable, so valid: %v", err)
	}
	for _, bad := range []string{"", "   ", "a\nb", "a\x00b", "a\rb"} {
		if err := ValidIdent("table", bad); err == nil {
			t.Errorf("ValidIdent(%q) should fail", bad)
		}
	}
}

func TestCommandWord(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SELECT 1", "SELECT"},
		{"  insert into t values (1)", "INSERT"},
		{"\n\tUPDATE t SET a = 1", "UPDATE"},
		{"MERGE t AS target", "MERGE"},
		{"-- a note\nDELETE FROM t", "DELETE"},
		{"/* a block\n comment */ CREATE TABLE t (a int)", "CREATE"},
		{"", ""},
		{";", ""},
	}
	for _, c := range cases {
		if got := CommandWord(c.in); got != c.want {
			t.Errorf("CommandWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReturnsRows(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"SELECT * FROM t", true},
		{"WITH c AS (SELECT 1 AS a) SELECT * FROM c", true},
		{"EXEC sp_who", true},
		{"UPDATE t SET a = 1", false},
		{"DELETE FROM t WHERE id = @p1", false},
		{"INSERT INTO t (a) VALUES (@p1)", false},
		{"INSERT INTO t (a) OUTPUT INSERTED.* VALUES (@p1)", true},
		{"DELETE FROM t OUTPUT DELETED.[id] WHERE id = @p1", true},
		{"SELECT a FROM t FOR JSON PATH", true},
		{"CREATE TABLE t (a int)", false},
		// A word that only looks like the clause must not flip the decision:
		// the count from the server is what the node reports for these.
		{"UPDATE t SET output_path = @p1", false},
		{"UPDATE t SET note = 'send to OUTPUT folder'", false},
		{"UPDATE t SET a = 1 -- then OUTPUT the rows", false},
		{"/* OUTPUT */ UPDATE t SET a = 1", false},
	}
	for _, c := range cases {
		if got := ReturnsRows(c.in); got != c.want {
			t.Errorf("ReturnsRows(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
