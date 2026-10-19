package mssql

import (
	"fmt"
	"regexp"
	"strings"
)

// QuoteIdent wraps a T-SQL identifier in brackets, which is how SQL Server
// quotes a name that is a keyword, holds a space, or is otherwise not a bare
// identifier. A closing bracket inside the name is doubled, as T-SQL escapes it,
// so a name can never end the quoting early and continue as SQL.
//
// Identifiers — a table, a column — cannot be bound as parameters, so this is
// what keeps a name the flow author supplied out of the statement's grammar.
// Use ValidIdent first on anything that came from a form.
func QuoteIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// ValidIdent rejects a name that cannot be safely bracket-quoted: an empty one,
// or one holding a character that terminates or splits a statement rather than
// naming anything (a NUL, a newline, a bracket-escaping oddity).
func ValidIdent(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("missing %s name", kind)
	}
	if strings.ContainsAny(name, "\x00\n\r") {
		return fmt.Errorf("invalid %s name %q: it contains a line break or a null character", kind, name)
	}
	return nil
}

// Qualify renders a bracket-quoted, optionally schema-qualified table name.
func Qualify(schema, table string) string {
	if strings.TrimSpace(schema) == "" {
		return QuoteIdent(table)
	}
	return QuoteIdent(schema) + "." + QuoteIdent(table)
}

// QuoteLiteral renders a T-SQL string literal, doubling the single quotes
// inside it. It is for the places where a name has to travel as *text* rather
// than as an identifier — OBJECT_ID('dbo.findings') — where a bound parameter
// is not allowed because the value is read at parse time.
func QuoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

var (
	// lineComment, blockComment and stringLiteral are stripped before a
	// statement is classified, so a word inside a comment or a quoted value
	// cannot be mistaken for a clause. See strip.
	lineComment   = regexp.MustCompile(`--[^\n]*`)
	blockComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	stringLiteral = regexp.MustCompile(`'(?:[^']|'')*'`)

	// outputClause recognises the clause that makes a write return rows. The
	// word has to stand alone: a column called `output_path` is not one.
	outputClause = regexp.MustCompile(`(?i)\bOUTPUT\b`)
	// forJSONOrXML recognises the other way a statement yields a result set.
	forJSONOrXML = regexp.MustCompile(`(?i)\bFOR\s+(JSON|XML)\b`)
	// leadingWord is the statement's first bare word, which names the command.
	leadingWord = regexp.MustCompile(`(?i)^[^\pL]*([\pL@#]+)`)
)

// strip removes comments and string literals from a statement, leaving a
// skeleton safe to pattern-match against. It is never run — only inspected — so
// replacing a literal with a blank rather than parsing it is enough.
func strip(statement string) string {
	s := blockComment.ReplaceAllString(statement, " ")
	s = lineComment.ReplaceAllString(s, " ")
	return stringLiteral.ReplaceAllString(s, "''")
}

// CommandWord is the leading verb of a statement — INSERT, UPDATE, MERGE — in
// upper case, or "" when it has none.
//
// Unlike a PostgreSQL command tag, this is read off the statement text rather
// than reported by the server: the TDS protocol answers a write with a count,
// not with the name of what ran. For a CTE the verb is the leading WITH, which
// is all the text says without parsing the whole statement.
func CommandWord(statement string) string {
	match := leadingWord.FindStringSubmatch(strip(statement))
	if len(match) < 2 {
		return ""
	}
	return strings.ToUpper(match[1])
}

// ReturnsRows reports whether a statement is expected to produce a result set,
// which decides whether a write goes through QueryContext or ExecContext — see
// Client.Exec for why that choice has to be made.
//
// A read (SELECT, or a WITH that feeds one) returns rows by definition; a write
// does when it carries an OUTPUT clause or a FOR JSON / FOR XML tail; a stored
// procedure may, so EXEC is treated as if it does.
func ReturnsRows(statement string) bool {
	skeleton := strip(statement)
	switch CommandWord(statement) {
	case "SELECT", "WITH", "EXEC", "EXECUTE", "DECLARE":
		return true
	}
	return outputClause.MatchString(skeleton) || forJSONOrXML.MatchString(skeleton)
}
