package actions

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Inflowenger/mssql-plugin/internal/mssql"
)

// TestIntegration exercises the real database path — the existence-guarded
// create and its no-op re-run, a MERGE upsert that inserts and then updates, an
// insert with typed params, and a read back — against a live SQL Server. It is
// skipped unless MSSQL_TEST_DSN is set, so `go test ./...` stays hermetic.
//
//	MSSQL_TEST_DSN="sqlserver://sa:Your_password123@localhost:1433?database=master&encrypt=disable" \
//	  go test ./internal/actions -run TestIntegration -v
//
// A throwaway server to point it at:
//
//	docker run -d --name mssql-it -p 1433:1433 \
//	  -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Your_password123 \
//	  mcr.microsoft.com/mssql/server:2022-latest
func TestIntegration(t *testing.T) {
	dsn := os.Getenv("MSSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set MSSQL_TEST_DSN to run the live-database integration test")
	}

	client, err := mssql.New(mssql.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Logf("connected to %s", client.ConnInfo())

	const table = "findings_it"
	qualified := mssql.Qualify("", table)

	// Start clean, however a previous run left off.
	if _, err := client.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", qualified), nil); err != nil {
		t.Fatalf("drop: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = client.Exec(cleanup, fmt.Sprintf("DROP TABLE IF EXISTS %s", qualified), nil)
	})

	// ---------------------------------------------------- create, and re-run --

	ddl, err := buildCreateDDL("", table, "finding_id int primary key, severity nvarchar(20) not null, score decimal(10,2) null, seen_at datetime2 null")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec(ctx, ddl, nil); err != nil {
		t.Fatalf("create: %v\n%s", err, ddl)
	}
	// The whole point of the guard: running the same node twice is a no-op, not
	// an "object already exists" failure.
	if _, err := client.Exec(ctx, ddl, nil); err != nil {
		t.Fatalf("re-create should be a no-op: %v", err)
	}

	// --------------------------------------------------------- insert, upsert --

	pk, err := client.PrimaryKeyColumns(ctx, qualified)
	if err != nil {
		t.Fatal(err)
	}
	if len(pk) != 1 || pk[0] != "finding_id" {
		t.Fatalf("primary key = %v, want [finding_id]", pk)
	}

	identity, err := client.IdentityColumn(ctx, qualified)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "" {
		t.Fatalf("identity column = %q, want none on this table", identity)
	}

	cols := []string{"finding_id", "score", "severity"} // sorted, as the action builds them
	upsert, upsertCommand := buildUpsertDDL(qualified, cols, pk, identity)
	if upsertCommand != "MERGE" {
		t.Fatalf("a table with a primary key should upsert via MERGE, got %q", upsertCommand)
	}

	// First write: no such key yet, so the MERGE inserts.
	res, err := client.Exec(ctx, upsert, []any{int64(7), 12.5, "high"})
	if err != nil {
		t.Fatalf("insert via merge: %v\n%s", err, upsert)
	}
	if res.RowsAffected != 1 {
		t.Errorf("insert affected %d rows, want 1", res.RowsAffected)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("OUTPUT should report the written row, got %d rows", len(res.Rows))
	}
	if got := res.Rows[0]["severity"]; got != "high" {
		t.Errorf("written severity = %#v, want high", got)
	}

	// Second write, same key: the MERGE updates instead of failing on the key.
	res, err = client.Exec(ctx, upsert, []any{int64(7), 99.0, "critical"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.RowsAffected != 1 {
		t.Errorf("upsert affected %d rows, want 1", res.RowsAffected)
	}
	if got := res.Rows[0]["severity"]; got != "critical" {
		t.Errorf("upserted severity = %#v, want critical", got)
	}

	// ------------------------------------------------- typed params on a write --

	plain := fmt.Sprintf("INSERT INTO %s (finding_id, severity, score, seen_at) VALUES (@p1, @p2, @p3, @p4)", qualified)
	args, err := parseParams([]string{"8", "low", "1.25", "2026-01-02T03:04:05Z"})
	if err != nil {
		t.Fatal(err)
	}
	// The timestamp is text, which is what a datetime2 column accepts; the
	// others keep the types coerceParam gave them.
	res, err = client.Exec(ctx, plain, args)
	if err != nil {
		t.Fatalf("parameterised insert: %v", err)
	}
	if res.RowsAffected != 1 {
		t.Errorf("affected %d rows, want 1", res.RowsAffected)
	}
	if res.Command != "INSERT" {
		t.Errorf("command = %q, want INSERT", res.Command)
	}

	// ---------------------------------------------------------------- read back --

	read, err := client.Query(ctx, fmt.Sprintf("SELECT finding_id, severity, score, seen_at FROM %s ORDER BY finding_id", qualified), nil, maxRowLimit)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.RowCount != 2 {
		t.Fatalf("read %d rows, want 2", read.RowCount)
	}
	if got := read.Rows[0]["finding_id"]; got != int64(7) {
		t.Errorf("finding_id = %#v (%T), want int64(7)", got, got)
	}
	// decimal arrives as exact decimal text rather than being forced through a
	// float — see normalize.
	if got, ok := read.Rows[0]["score"].(string); !ok {
		t.Errorf("score = %#v (%T), want decimal text", read.Rows[0]["score"], read.Rows[0]["score"])
	} else if got != "99.00" {
		t.Errorf("score = %q, want 99.00", got)
	}
	// A datetime2 comes back as RFC 3339 text, which a flow can compare.
	if got, ok := read.Rows[1]["seen_at"].(string); !ok || got == "" {
		t.Errorf("seen_at = %#v, want an RFC 3339 string", read.Rows[1]["seen_at"])
	}

	// The row cap is applied while reading, since T-SQL has no LIMIT.
	capped, err := client.Query(ctx, fmt.Sprintf("SELECT finding_id FROM %s", qualified), nil, 1)
	if err != nil {
		t.Fatalf("capped read: %v", err)
	}
	if capped.RowCount != 1 {
		t.Errorf("capped read returned %d rows, want 1", capped.RowCount)
	}

	// ------------------------------------------------- a GUID, rendered readably --

	guid, err := client.Query(ctx, "SELECT CAST('6f9619ff-8b86-d011-b42d-00c04fc964ff' AS uniqueidentifier) AS id", nil, 1)
	if err != nil {
		t.Fatalf("guid read: %v", err)
	}
	if got := guid.Rows[0]["id"]; got != "6f9619ff-8b86-d011-b42d-00c04fc964ff" {
		t.Errorf("uniqueidentifier = %#v, want the canonical text", got)
	}

	// -------------------------------------------------------- a write's count --

	del, err := client.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE finding_id = @p1", qualified), []any{int64(8)})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if del.RowsAffected != 1 {
		t.Errorf("delete affected %d rows, want 1", del.RowsAffected)
	}
	if del.Command != "DELETE" {
		t.Errorf("command = %q, want DELETE", del.Command)
	}
}

// TestIntegrationIdentity covers the IDENTITY column rules, which the table in
// TestIntegration deliberately does not have: a key left out is generated by the
// server, a key supplied explicitly is honoured exactly (which SQL Server allows
// only under SET IDENTITY_INSERT), and supplying it twice updates the row rather
// than failing on the key or inserting a second one.
//
// Same switch as TestIntegration: set MSSQL_TEST_DSN to run it.
func TestIntegrationIdentity(t *testing.T) {
	dsn := os.Getenv("MSSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set MSSQL_TEST_DSN to run the live-database integration test")
	}
	client, err := mssql.New(mssql.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const table = "identity_it"
	q := mssql.Qualify("", table)
	if _, err := client.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", q), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = client.Exec(c, fmt.Sprintf("DROP TABLE IF EXISTS %s", q), nil)
	})

	ddl, err := buildCreateDDL("", table, "id int identity(1,1) primary key, name nvarchar(50) not null")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec(ctx, ddl, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	pk, err := client.PrimaryKeyColumns(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.IdentityColumn(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "id" {
		t.Fatalf("identity column = %q, want id", identity)
	}

	// (a) identity left out -> plain insert, server generates the key.
	gen, command := buildUpsertDDL(q, []string{"name"}, pk, identity)
	if command != "INSERT" {
		t.Errorf("an omitted identity key leaves nothing to merge on, so command should be INSERT, got %q", command)
	}
	res, err := client.Exec(ctx, gen, []any{"generated"})
	if err != nil {
		t.Fatalf("generated-key insert: %v", err)
	}
	if got := res.Rows[0]["id"]; got != int64(1) {
		t.Errorf("generated id = %#v, want 1", got)
	}

	// (b) identity supplied -> IDENTITY_INSERT bracketing honours it exactly.
	forced, command := buildUpsertDDL(q, []string{"id", "name"}, pk, identity)
	if command != "MERGE" {
		t.Errorf("command = %q, want MERGE (the batch's own verb is SET)", command)
	}
	res, err = client.Exec(ctx, forced, []any{int64(42), "forced"})
	if err != nil {
		t.Fatalf("explicit identity insert: %v", err)
	}
	if got := res.Rows[0]["id"]; got != int64(42) {
		t.Errorf("forced id = %#v, want 42", got)
	}

	// (c) same key again -> the MERGE updates rather than failing on the key.
	res, err = client.Exec(ctx, forced, []any{int64(42), "updated"})
	if err != nil {
		t.Fatalf("upsert on identity key: %v", err)
	}
	if got := res.Rows[0]["name"]; got != "updated" {
		t.Errorf("name = %#v, want updated", got)
	}

	read, err := client.Query(ctx, fmt.Sprintf("SELECT id, name FROM %s ORDER BY id", q), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if read.RowCount != 2 {
		t.Errorf("row count = %d, want 2 (the update must not have inserted a third)", read.RowCount)
	}
}
