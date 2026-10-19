package mssql

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// Client is a ready-to-use pool for one connection. Query and Exec are the two
// halves of the plugin — reading rows, and writing with a count — and both
// accept positional `@p1…@pn` arguments so user input never lands in the SQL
// text.
type Client struct {
	db       *sql.DB
	connInfo string
}

// Pool limits for one settings profile. SQL Server charges real memory per
// session, and Azure SQL enforces a connection ceiling per tier, so the pool
// stays small; the lifetime cap keeps a failed-over or rebooted instance from
// handing back connections that are no longer good.
const (
	maxOpenConns    = 10
	maxIdleConns    = 2
	connMaxIdleTime = 5 * time.Minute
	connMaxLifetime = 30 * time.Minute
)

// New opens a connection pool for the config. The connector parses the
// connection string eagerly, so a malformed one fails here, at set-up, rather
// than on the first query.
func New(cfg Config) (*Client, error) {
	connString := cfg.ConnString()

	connector, err := mssqldb.NewConnector(connString)
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}

	// sql.OpenDB does not dial until a connection is first needed, which keeps
	// start-up cheap; the Ping in the connection test is what proves the pool.
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	db.SetConnMaxLifetime(connMaxLifetime)

	return &Client{db: db, connInfo: describe(connString)}, nil
}

// describe renders host:port/database — enough to say which server a node is
// talking to, with no credentials in it. The connection string is re-parsed
// rather than read off the Config, so a pasted DSN is described too.
func describe(connString string) string {
	parsed, err := msdsn.Parse(connString)
	if err != nil {
		return "the configured server"
	}
	host := parsed.Host
	if parsed.Instance != "" {
		host += `\` + parsed.Instance
	}
	if parsed.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, parsed.Port)
	}
	if parsed.Database != "" {
		return host + "/" + parsed.Database
	}
	return host
}

// Close releases the pool. It is called when a client is evicted from the cache.
func (c *Client) Close() { _ = c.db.Close() }

// ConnInfo is host:port/database — enough to say which database a node is
// talking to, with no credentials in it.
func (c *Client) ConnInfo() string { return c.connInfo }

// Ping verifies the connection is live, for the "Test connection" button.
func (c *Client) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// Result is what a read returns: the column names in order, and the rows as
// JSON-able maps.
type Result struct {
	Columns  []string         `json:"columns"`
	Rows     []map[string]any `json:"rows"`
	RowCount int              `json:"rowCount"`
}

// Query runs a statement and reads its rows. It is used for reads (SELECT), and
// also for a write whose statement carries OUTPUT — the driver surfaces those
// rows the same way. `maxRows` caps how many are read; 0 means no cap.
//
// T-SQL has no LIMIT clause to push the cap into, so it is applied while the
// rows are read: the server still computes the whole result set. Put a
// `TOP (n)` in the statement to make it do less work.
//
// A batch can produce several result sets (a `SET NOCOUNT`-style preamble, an
// OUTPUT clause followed by a SELECT). The first one that has columns is the
// answer; the rest are drained so the connection goes back to the pool clean.
func (c *Client) Query(ctx context.Context, sql string, args []any, maxRows int) (Result, error) {
	rows, err := c.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()

	for {
		columns, err := rows.Columns()
		if err == nil && len(columns) > 0 {
			return readResultSet(rows, columns, maxRows)
		}
		if !rows.NextResultSet() {
			// No result set carried columns: the statement returned nothing.
			return Result{Columns: []string{}, Rows: []map[string]any{}}, rows.Err()
		}
	}
}

// readResultSet drains one result set into JSON-able maps.
func readResultSet(rows *sql.Rows, columns []string, maxRows int) (Result, error) {
	// The declared type of each column tells a 16-byte uniqueidentifier apart
	// from a 16-byte varbinary, which arrive as the same []byte. See normalize.
	types := make([]string, len(columns))
	if columnTypes, err := rows.ColumnTypes(); err == nil {
		for i, ct := range columnTypes {
			if i < len(types) {
				types[i] = strings.ToUpper(ct.DatabaseTypeName())
			}
		}
	}

	out := make([]map[string]any, 0, 16)
	scanned := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range scanned {
		pointers[i] = &scanned[i]
	}

	for rows.Next() {
		if maxRows > 0 && len(out) >= maxRows {
			break
		}
		if err := rows.Scan(pointers...); err != nil {
			return Result{}, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = normalize(scanned[i], types[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	return Result{Columns: columns, Rows: out, RowCount: len(out)}, nil
}

// ExecResult is what a write returns: the command verb (INSERT, UPDATE, …), the
// number of rows it affected, and any rows an OUTPUT clause produced.
type ExecResult struct {
	Command      string           `json:"command"`
	RowsAffected int64            `json:"rowsAffected"`
	Columns      []string         `json:"columns,omitempty"`
	Rows         []map[string]any `json:"rows,omitempty"`
	RowCount     int              `json:"rowCount"`
}

// Exec runs a write (INSERT / UPDATE / DELETE / MERGE / DDL).
//
// Which of the driver's two paths it takes depends on whether the statement
// returns rows, because database/sql offers the count and the rows on different
// calls: ExecContext reports rows-affected but discards any rows, QueryContext
// yields the rows but no count. A statement with an OUTPUT clause therefore goes
// through Query, where the number of rows OUTPUT reported *is* the number
// affected; everything else goes through Exec and gets the server's own count.
func (c *Client) Exec(ctx context.Context, statement string, args []any) (ExecResult, error) {
	command := CommandWord(statement)

	if ReturnsRows(statement) {
		res, err := c.Query(ctx, statement, args, 0)
		if err != nil {
			return ExecResult{}, err
		}
		out := ExecResult{
			Command:      command,
			RowsAffected: int64(res.RowCount),
			RowCount:     res.RowCount,
		}
		if res.RowCount > 0 {
			out.Columns = res.Columns
			out.Rows = res.Rows
		}
		return out, nil
	}

	result, err := c.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return ExecResult{}, err
	}
	// RowsAffected is not supported by every statement (DDL reports none); an
	// error there is not a failure of the write.
	affected, err := result.RowsAffected()
	if err != nil {
		affected = 0
	}
	return ExecResult{Command: command, RowsAffected: affected}, nil
}

// PrimaryKeyColumns returns the primary-key columns of a table, in key order, so
// a write can turn a clash into an update. `qualified` is a table name,
// optionally schema-qualified (dbo.findings); OBJECT_ID resolves it against the
// connection's default schema and yields NULL for a table that does not exist,
// which comes back as an empty key here — the write then runs as a plain insert
// and fails, if the table really is missing, with the server's own clear
// "Invalid object name".
func (c *Client) PrimaryKeyColumns(ctx context.Context, qualified string) ([]string, error) {
	const statement = `SELECT c.name AS column_name
FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.is_primary_key = 1 AND i.object_id = OBJECT_ID(@p1)
ORDER BY ic.key_ordinal`

	res, err := c.Query(ctx, statement, []any{qualified}, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot read primary key of %s: %w", qualified, err)
	}
	pk := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		if name, ok := row["column_name"].(string); ok {
			pk = append(pk, name)
		}
	}
	return pk, nil
}

// IdentityColumn returns the name of the table's IDENTITY column, or "" when it
// has none. A value cannot be written into one without SET IDENTITY_INSERT, so
// an insert leaves it out and lets the server generate it.
func (c *Client) IdentityColumn(ctx context.Context, qualified string) (string, error) {
	const statement = `SELECT name AS column_name
FROM sys.identity_columns
WHERE object_id = OBJECT_ID(@p1)`

	res, err := c.Query(ctx, statement, []any{qualified}, 0)
	if err != nil {
		return "", fmt.Errorf("cannot read identity column of %s: %w", qualified, err)
	}
	if len(res.Rows) == 0 {
		return "", nil
	}
	name, _ := res.Rows[0]["column_name"].(string)
	return name, nil
}

// normalize turns a driver-decoded value into something that marshals to clean
// JSON. The driver hands back Go values faithful to the column type, and left
// as-is several of those would serialise as byte arrays of numbers:
//
//   - uniqueidentifier arrives as 16 raw bytes in TDS field order, which is not
//     the order a GUID is written in; the driver's own UniqueIdentifier.Scan
//     reorders them, and the canonical text is what a flow can compare and log.
//   - decimal, numeric and money arrive as their decimal text, already exact —
//     they stay text rather than being forced through a float.
//   - varbinary is kept as data: base64 when it is not valid UTF-8.
func normalize(value any, dbType string) any {
	switch v := value.(type) {
	case nil:
		return nil
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case []byte:
		if dbType == "UNIQUEIDENTIFIER" && len(v) == 16 {
			var uid mssqldb.UniqueIdentifier
			if err := uid.Scan(v); err == nil {
				return strings.ToLower(uid.String())
			}
		}
		if utf8.Valid(v) {
			return string(v)
		}
		return base64.StdEncoding.EncodeToString(v)
	default:
		return v
	}
}
