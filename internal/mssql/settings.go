// Package mssql turns the settings profile the platform ships with every call
// into a live connection pool, and runs read/write T-SQL over it.
//
// The plugin holds no database configuration of its own. A connection travels
// in each request as `body.settings`, the same named profile the runtime folds
// into action bodies — so one running plugin can serve many SQL Server
// instances and rotating a password needs no redeploy.
package mssql

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// ErrNoSettings is returned when a call arrives without a connection. It is
// phrased for the person looking at the node, since that is where the fix is.
var ErrNoSettings = errors.New("this node has no SQL Server connection: pick a settings profile in the node drawer (host, database, user, password) or create one from the plugin's set-up form")

// Config is a resolved SQL Server connection. Either a full DSN is given, or
// the discrete fields are, and the two are merged into one URL the driver
// understands.
type Config struct {
	DSN      string
	Host     string
	Port     string
	Instance string
	Database string
	User     string
	Password string
	// Encrypt is the driver's `encrypt` parameter: disable, false, true or
	// strict. Empty leaves the driver's own default (false — the login packet
	// is encrypted, the rest of the session is not).
	Encrypt string
	// TrustServerCertificate skips certificate validation. It is what a server
	// with a self-signed certificate needs — the common case on a development
	// or on-premises instance once encryption is on.
	TrustServerCertificate bool
	AppName                string
	ConnectTimeout         string
}

// ParseConfig reads a connection out of the settings profile the platform ships
// with every action call as `body.settings`.
//
// The profile is a free-form key/value record: it is filled from the plugin's
// own set-up form when the platform could read it, and hand-typed otherwise. So
// keys are matched leniently — case, spaces, dashes and underscores are ignored,
// and the usual synonyms for each field are accepted.
func ParseConfig(settings map[string]any) (Config, error) {
	if len(settings) == 0 {
		return Config{}, ErrNoSettings
	}

	values := make(map[string]string, len(settings))
	for key, value := range settings {
		if text := toString(value); text != "" {
			values[canonicalKey(key)] = text
		}
	}

	cfg := Config{
		DSN:                    pick(values, dsnKeys),
		Host:                   pick(values, hostKeys),
		Port:                   pick(values, portKeys),
		Instance:               pick(values, instanceKeys),
		Database:               pick(values, databaseKeys),
		User:                   pick(values, userKeys),
		Password:               pick(values, passwordKeys),
		Encrypt:                pick(values, encryptKeys),
		TrustServerCertificate: isTrue(pick(values, trustKeys)),
		AppName:                pick(values, appNameKeys),
		ConnectTimeout:         pick(values, timeoutKeys),
	}

	// A host written the way SQL Server tooling writes it — HOST\INSTANCE, or
	// HOST,1433 — carries fields of its own. Split them out so the URL is built
	// from the pieces rather than from a string the URL form cannot express.
	cfg.splitHost()

	if cfg.DSN == "" && cfg.Host == "" {
		// Something was configured, but none of it is a SQL Server connection —
		// most likely the wrong profile is selected on the node.
		return Config{}, fmt.Errorf("%w (the selected profile has none of these: %s)", ErrNoSettings, strings.Join(sortedKeys(settings), ", "))
	}
	return cfg, nil
}

// splitHost moves an instance name or port written into the host field into its
// own field: `sqlexpress.corp\SQLEXPRESS` and `db.corp,1433` are both ordinary
// ways to write a SQL Server address, and neither belongs in a URL's host.
func (c *Config) splitHost() {
	host := strings.TrimSpace(c.Host)
	if host == "" {
		return
	}
	if name, instance, found := strings.Cut(host, `\`); found {
		host = strings.TrimSpace(name)
		if c.Instance == "" {
			c.Instance = strings.TrimSpace(instance)
		}
	}
	if name, port, found := strings.Cut(host, ","); found {
		host = strings.TrimSpace(name)
		if c.Port == "" {
			c.Port = strings.TrimSpace(port)
		}
	}
	c.Host = host
}

// ConnString renders the config as the `sqlserver://` URL the driver parses. A
// DSN given verbatim wins outright — a user who pasted a full connection string
// means it, and re-deriving one from half-filled fields would only fight them.
//
// The URL form is used rather than the ADO keyword/value form because every
// part of it is percent-escaped: a password holding a `;` or a space travels
// intact instead of splitting the string into extra settings.
func (c Config) ConnString() string {
	if dsn := strings.TrimSpace(c.DSN); dsn != "" {
		return dsn
	}

	query := url.Values{}
	set := func(key, value string) {
		if value = strings.TrimSpace(value); value != "" {
			query.Set(key, value)
		}
	}
	set("database", c.Database)
	set("encrypt", c.Encrypt)
	set("connection timeout", c.ConnectTimeout)
	set("app name", firstNonEmpty(c.AppName, "inflow-mssql-plugin"))
	if c.TrustServerCertificate {
		query.Set("trustservercertificate", "true")
	}

	u := url.URL{Scheme: "sqlserver", Host: c.hostPort(), RawQuery: query.Encode()}
	if user := strings.TrimSpace(c.User); user != "" {
		u.User = url.UserPassword(user, c.Password)
	}
	// A named instance goes in the path, where the driver reads it; the SQL
	// Browser service then resolves the port, which is why an instance and an
	// explicit port are not usually given together.
	if instance := strings.TrimSpace(c.Instance); instance != "" {
		u.Path = "/" + instance
	}
	return u.String()
}

// hostPort joins the host and port, leaving the port off when a named instance
// is in play (the browser service supplies it) or when none was given (the
// driver defaults to 1433).
func (c Config) hostPort() string {
	host := strings.TrimSpace(c.Host)
	port := strings.TrimSpace(c.Port)
	if port == "" || strings.TrimSpace(c.Instance) != "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// isTrue reads a checkbox out of a profile that may store it as a bool, a
// string, or a number — the settings editor keeps whatever the field held.
func isTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "1", "t", "on":
		return true
	}
	return false
}

// Field synonyms, in preference order. Each entry is already canonical (lower
// case, letters and digits only) — see canonicalKey.
var (
	dsnKeys      = []string{"dsn", "connectionstring", "connstring", "connectionurl", "url", "uri", "connuri"}
	hostKeys     = []string{"host", "hostname", "server", "address", "endpoint", "datasource"}
	portKeys     = []string{"port"}
	instanceKeys = []string{"instance", "instancename", "namedinstance"}
	databaseKeys = []string{"database", "dbname", "db", "initialcatalog", "catalog"}
	userKeys     = []string{"user", "username", "userid", "uid", "login", "account"}
	passwordKeys = []string{"password", "pass", "pwd", "secret", "apikey", "token"}
	encryptKeys  = []string{"encrypt", "encryption", "ssl", "tls", "sslmode"}
	trustKeys    = []string{"trustservercertificate", "trustcert", "trustservercert", "insecureskipverify", "trust"}
	appNameKeys  = []string{"appname", "applicationname", "application"}
	timeoutKeys  = []string{"connectiontimeout", "connecttimeout", "timeout", "logintimeout"}
)

func pick(values map[string]string, keys []string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return ""
}

// canonicalKey reduces a settings key to letters and digits, lower case, so
// "User ID", "user_id" and "userId" are one key.
func canonicalKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toString renders a profile value. The settings editor stores anything that
// parses as JSON (numbers, booleans) as that type, so values are not always
// strings — a port arrives as a float64.
func toString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

func sortedKeys(settings map[string]any) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Pool hands out one client per distinct connection, so every job using the
// same settings profile shares a database/sql pool instead of dialling afresh.
// Clients are safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	clients map[string]*Client
}

// maxPooledClients bounds the cache: a handful of profiles is the norm, and the
// pool is drained rather than grown without limit if that ever changes.
const maxPooledClients = 16

func NewPool() *Pool { return &Pool{clients: map[string]*Client{}} }

// Client resolves the settings profile shipped with a call into a client,
// opening (and caching) a connection pool for it on first use.
func (p *Pool) Client(settings map[string]any) (*Client, error) {
	cfg, err := ParseConfig(settings)
	if err != nil {
		return nil, err
	}
	return p.For(cfg)
}

// For returns the client for an already-parsed config.
func (p *Pool) For(cfg Config) (*Client, error) {
	key := cfg.fingerprint()

	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[key]; ok {
		return client, nil
	}
	client, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if len(p.clients) >= maxPooledClients {
		for k, c := range p.clients {
			c.Close()
			delete(p.clients, k)
		}
	}
	p.clients[key] = client
	return client, nil
}

// fingerprint identifies a connection without keeping the password in a map key.
func (c Config) fingerprint() string {
	sum := sha256.Sum256([]byte(c.ConnString()))
	return hex.EncodeToString(sum[:])
}
