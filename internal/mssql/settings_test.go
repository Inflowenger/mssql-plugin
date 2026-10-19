package mssql

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParseConfigNoSettings(t *testing.T) {
	if _, err := ParseConfig(nil); !errors.Is(err, ErrNoSettings) {
		t.Errorf("an empty profile should be ErrNoSettings, got %v", err)
	}
	// Something is configured, but none of it is a connection — the wrong
	// profile is bound to the node.
	_, err := ParseConfig(map[string]any{"apiBaseUrl": "https://example.test"})
	if !errors.Is(err, ErrNoSettings) {
		t.Errorf("a profile with no connection keys should be ErrNoSettings, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "apiBaseUrl") {
		t.Errorf("the error should name what the profile does hold, got %v", err)
	}
}

func TestParseConfigLenientKeys(t *testing.T) {
	// Case, spaces, dashes and underscores are ignored, and the usual synonyms
	// are accepted, so a hand-typed profile still resolves.
	cfg, err := ParseConfig(map[string]any{
		"Server":                   "db.internal",
		"Port":                     float64(1433), // the editor stores numbers as numbers
		"Initial Catalog":          "appdb",
		"user_id":                  "sa",
		"PASSWORD":                 "p@ss word",
		"encrypt":                  "true",
		"trust-server-certificate": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.internal" || cfg.Port != "1433" || cfg.Database != "appdb" {
		t.Errorf("host/port/database not resolved: %+v", cfg)
	}
	if cfg.User != "sa" || cfg.Password != "p@ss word" {
		t.Errorf("credentials not resolved: user=%q", cfg.User)
	}
	if cfg.Encrypt != "true" || !cfg.TrustServerCertificate {
		t.Errorf("TLS fields not resolved: %+v", cfg)
	}
}

func TestParseConfigSplitsHost(t *testing.T) {
	// HOST\INSTANCE and HOST,PORT are both ordinary ways to write a SQL Server
	// address, and neither belongs in a URL's host.
	cfg, err := ParseConfig(map[string]any{"host": `sql.corp\SQLEXPRESS`, "database": "appdb"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "sql.corp" || cfg.Instance != "SQLEXPRESS" {
		t.Errorf("instance not split out: %+v", cfg)
	}

	cfg, err = ParseConfig(map[string]any{"host": "sql.corp,14330"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "sql.corp" || cfg.Port != "14330" {
		t.Errorf("port not split out: %+v", cfg)
	}
}

func TestConnStringDSNWins(t *testing.T) {
	const dsn = "sqlserver://sa:secret@elsewhere:1433?database=other"
	cfg := Config{DSN: dsn, Host: "ignored", Database: "ignored"}
	if got := cfg.ConnString(); got != dsn {
		t.Errorf("a pasted DSN should win verbatim:\n got %q\nwant %q", got, dsn)
	}
}

func TestConnStringFromFields(t *testing.T) {
	cfg := Config{
		Host:                   "db.internal",
		Port:                   "1433",
		Database:               "appdb",
		User:                   "sa",
		Password:               "p@ss;word with spaces",
		Encrypt:                "true",
		TrustServerCertificate: true,
	}
	raw := cfg.ConnString()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the built connection string does not parse as a URL: %v (%q)", err, raw)
	}
	if parsed.Scheme != "sqlserver" {
		t.Errorf("scheme = %q, want sqlserver", parsed.Scheme)
	}
	if parsed.Host != "db.internal:1433" {
		t.Errorf("host = %q", parsed.Host)
	}
	// The whole point of the URL form: a password holding a ';' or a space
	// travels intact instead of splitting the string into extra settings.
	password, _ := parsed.User.Password()
	if password != "p@ss;word with spaces" {
		t.Errorf("password did not survive escaping: %q", password)
	}
	query := parsed.Query()
	if query.Get("database") != "appdb" {
		t.Errorf("database = %q", query.Get("database"))
	}
	if query.Get("encrypt") != "true" || query.Get("trustservercertificate") != "true" {
		t.Errorf("TLS params not set: %v", query)
	}
	if query.Get("app name") == "" {
		t.Error("app name should be set so the connection is identifiable on the server")
	}
}

func TestConnStringNamedInstanceDropsPort(t *testing.T) {
	// With a named instance the SQL Browser service resolves the port, so the
	// default port must not be pinned into the URL alongside it.
	cfg := Config{Host: "sql.corp", Port: "1433", Instance: "SQLEXPRESS", Database: "appdb"}
	parsed, err := url.Parse(cfg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "sql.corp" {
		t.Errorf("host = %q, want the bare host", parsed.Host)
	}
	if parsed.Path != "/SQLEXPRESS" {
		t.Errorf("instance path = %q, want /SQLEXPRESS", parsed.Path)
	}
}

func TestConnStringNoUser(t *testing.T) {
	// No login given (a trusted connection, or credentials carried some other
	// way): the URL must not grow an empty userinfo section.
	cfg := Config{Host: "db.internal", Database: "appdb"}
	if got := cfg.ConnString(); strings.Contains(got, "@") {
		t.Errorf("no credentials should mean no userinfo: %q", got)
	}
}

func TestFingerprintHidesPasswordButSeparatesConnections(t *testing.T) {
	a := Config{Host: "db", Database: "x", User: "sa", Password: "one"}
	b := Config{Host: "db", Database: "x", User: "sa", Password: "two"}
	if a.fingerprint() == b.fingerprint() {
		t.Error("a changed password must not reuse the cached client")
	}
	if strings.Contains(a.fingerprint(), "one") {
		t.Error("the fingerprint must not carry the password")
	}
	if a.fingerprint() != (Config{Host: "db", Database: "x", User: "sa", Password: "one"}).fingerprint() {
		t.Error("the same connection must share one cached client")
	}
}

func TestIsTrue(t *testing.T) {
	for _, yes := range []string{"true", "TRUE", "yes", "1", "t", "on", " true "} {
		if !isTrue(yes) {
			t.Errorf("isTrue(%q) should be true", yes)
		}
	}
	for _, no := range []string{"", "false", "0", "no", "maybe"} {
		if isTrue(no) {
			t.Errorf("isTrue(%q) should be false", no)
		}
	}
}
