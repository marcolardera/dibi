// Package config stores connections, saved queries and small application
// state in the user's config directory (see Dir).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Type identifies the kind of data source of a connection.
type Type string

const (
	Postgres Type = "postgres"
	MySQL    Type = "mysql"
	MariaDB  Type = "mariadb"
	Oracle   Type = "oracle"
	MSSQL    Type = "sqlserver"
	SQLite   Type = "sqlite"
	DuckDB   Type = "duckdb"
	CSV      Type = "csv"
	Parquet  Type = "parquet"
)

// Types lists every supported connection type, in display order.
var Types = []Type{Postgres, MySQL, MariaDB, Oracle, MSSQL, SQLite, DuckDB, CSV, Parquet}

// Label returns the human-readable name of a connection type.
func (t Type) Label() string {
	switch t {
	case Postgres:
		return "PostgreSQL"
	case MySQL:
		return "MySQL"
	case MariaDB:
		return "MariaDB"
	case Oracle:
		return "Oracle"
	case MSSQL:
		return "SQL Server"
	case SQLite:
		return "SQLite"
	case DuckDB:
		return "DuckDB"
	case CSV:
		return "CSV"
	case Parquet:
		return "Parquet"
	}
	return string(t)
}

// IsFile reports whether the connection points to a local file rather than a server.
func (t Type) IsFile() bool {
	switch t {
	case SQLite, DuckDB, CSV, Parquet:
		return true
	}
	return false
}

// DefaultPort returns the usual port of a server type, or 0 for file types.
func (t Type) DefaultPort() int {
	switch t {
	case Postgres:
		return 5432
	case MySQL, MariaDB:
		return 3306
	case Oracle:
		return 1521
	case MSSQL:
		return 1433
	}
	return 0
}

// SSLMode says whether a server connection uses TLS and how much of the
// server certificate is checked. The names follow libpq's sslmode.
type SSLMode string

const (
	SSLDisable    SSLMode = "disable"     // never use TLS
	SSLPrefer     SSLMode = "prefer"      // use TLS if the server supports it, without checks
	SSLRequire    SSLMode = "require"     // always use TLS, without checks
	SSLVerifyCA   SSLMode = "verify-ca"   // also check that a trusted CA signed the certificate
	SSLVerifyFull SSLMode = "verify-full" // also check that the certificate names the host
)

// Label returns the human-readable name of an SSL mode.
func (m SSLMode) Label() string {
	switch m {
	case SSLDisable:
		return "Disable"
	case SSLPrefer:
		return "Prefer"
	case SSLRequire:
		return "Require"
	case SSLVerifyCA:
		return "Verify CA"
	case SSLVerifyFull:
		return "Verify full"
	}
	return string(m)
}

// Verifies reports whether the mode checks the server certificate.
func (m SSLMode) Verifies() bool { return m == SSLVerifyCA || m == SSLVerifyFull }

// SSLModes returns the SSL modes a server type supports; the first is the
// default. Oracle has no "prefer": TLS (TCPS) is served by its own listener.
func (t Type) SSLModes() []SSLMode {
	switch {
	case t.IsFile():
		return nil
	case t == Oracle:
		return []SSLMode{SSLDisable, SSLRequire, SSLVerifyCA, SSLVerifyFull}
	}
	return []SSLMode{SSLPrefer, SSLDisable, SSLRequire, SSLVerifyCA, SSLVerifyFull}
}

// Connection holds the parameters of a saved connection.
//
// For Oracle, Database is the service name. For SQL Server, Host may name
// an instance (host\instance, the port is then ignored) and an empty User
// means integrated (Windows/Kerberos) authentication. In v1 the password is
// stored in plaintext.
//
// SSLMode sets TLS (empty means the type's default). SSLRootCert is a PEM
// file of trusted CAs, checked instead of the system ones by the verify
// modes. SSLCert and SSLKey are a client certificate and its key (PEM).
// Wallet is an Oracle wallet folder: its cwallet.sso provides both the
// trusted CAs and the client certificate.
type Connection struct {
	Name     string `yaml:"name"`
	Type     Type   `yaml:"type"`
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Database string `yaml:"database,omitempty"`
	User     string `yaml:"user,omitempty"`
	Password string `yaml:"password,omitempty"`
	Path     string `yaml:"path,omitempty"`

	SSLMode     SSLMode `yaml:"ssl_mode,omitempty"`
	SSLRootCert string  `yaml:"ssl_root_cert,omitempty"`
	SSLCert     string  `yaml:"ssl_cert,omitempty"`
	SSLKey      string  `yaml:"ssl_key,omitempty"`
	Wallet      string  `yaml:"wallet,omitempty"`
}

// TLSMode returns the SSL mode in effect: SSLMode, or the type's default.
func (c *Connection) TLSMode() SSLMode {
	if c.SSLMode != "" {
		return c.SSLMode
	}
	if modes := c.Type.SSLModes(); len(modes) > 0 {
		return modes[0]
	}
	return SSLDisable
}

// Validate checks that the fields required by the connection type are set.
func (c *Connection) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	if !validType(c.Type) {
		return fmt.Errorf("unknown connection type %q", c.Type)
	}
	if c.Type.IsFile() {
		if c.Path == "" {
			return errors.New("file path is required")
		}
		return nil
	}
	if c.Host == "" {
		return errors.New("host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if c.Type == Oracle && c.Database == "" {
		return errors.New("service name is required")
	}
	if c.SSLMode != "" && !slices.Contains(c.Type.SSLModes(), c.SSLMode) {
		return fmt.Errorf("SSL mode %q is not supported by %s", c.SSLMode, c.Type.Label())
	}
	if (c.SSLCert == "") != (c.SSLKey == "") {
		return errors.New("the client certificate and its key must be given together")
	}
	return nil
}

func validType(t Type) bool {
	for _, x := range Types {
		if x == t {
			return true
		}
	}
	return false
}

// Store reads and writes configuration files under a root directory.
type Store struct {
	Root string
}

// Dir returns the default config directory for Dibi.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "dibi"), nil
}

// Open returns a Store rooted at dir, creating its subfolders if needed.
func Open(dir string) (*Store, error) {
	s := &Store{Root: dir}
	for _, d := range []string{s.connectionsDir(), s.queriesDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) connectionsDir() string { return filepath.Join(s.Root, "connections") }
func (s *Store) queriesDir() string     { return filepath.Join(s.Root, "queries") }
func (s *Store) statePath() string      { return filepath.Join(s.Root, "state.yaml") }

// fileName turns a user-provided name into a safe file name (without extension).
func fileName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' ||
			r == '<' || r == '>' || r == '|' || r < 32:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	n := b.String()
	if n == "" || n == "." || n == ".." {
		n = "_"
	}
	return n
}

// Connections returns all saved connections, sorted by name.
func (s *Store) Connections() ([]Connection, error) {
	entries, err := os.ReadDir(s.connectionsDir())
	if err != nil {
		return nil, err
	}
	var conns []Connection
	var errs []error
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.connectionsDir(), e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var c legacyConnection
		if err := yaml.Unmarshal(data, &c); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		conns = append(conns, c.upgrade())
	}
	sort.Slice(conns, func(i, j int) bool {
		return strings.ToLower(conns[i].Name) < strings.ToLower(conns[j].Name)
	})
	return conns, errors.Join(errs...)
}

// legacyConnection reads the SQL Server TLS options that preceded SSLMode.
type legacyConnection struct {
	Connection      `yaml:",inline"`
	Encrypt         bool `yaml:"encrypt,omitempty"`
	TrustServerCert bool `yaml:"trust_server_certificate,omitempty"`
}

func (l legacyConnection) upgrade() Connection {
	c := l.Connection
	if c.SSLMode == "" && l.Encrypt {
		c.SSLMode = SSLVerifyFull
		if l.TrustServerCert {
			c.SSLMode = SSLRequire
		}
	}
	return c
}

// SaveConnection writes a connection. If oldName is set and differs from the
// new name (a rename), the old file is removed. Names must be unique.
func (s *Store) SaveConnection(c Connection, oldName string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	path := filepath.Join(s.connectionsDir(), fileName(c.Name)+".yaml")
	renamed := oldName != "" && fileName(oldName) != fileName(c.Name)
	if oldName == "" || renamed {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("a connection named %q already exists", c.Name)
		}
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if renamed {
		return s.DeleteConnection(oldName)
	}
	return nil
}

// DeleteConnection removes a saved connection.
func (s *Store) DeleteConnection(name string) error {
	err := os.Remove(filepath.Join(s.connectionsDir(), fileName(name)+".yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Queries returns the names of the saved queries, sorted.
func (s *Store) Queries() ([]string, error) {
	entries, err := os.ReadDir(s.queriesDir())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			names = append(names, strings.TrimSuffix(e.Name(), ".sql"))
		}
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	return names, nil
}

// QueryExists reports whether a saved query with this name exists.
func (s *Store) QueryExists(name string) bool {
	_, err := os.Stat(filepath.Join(s.queriesDir(), fileName(name)+".sql"))
	return err == nil
}

// LoadQuery returns the text of a saved query.
func (s *Store) LoadQuery(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.queriesDir(), fileName(name)+".sql"))
	return string(data), err
}

// SaveQuery writes a query, replacing any existing one with the same name.
// It returns the name actually used on disk.
func (s *Store) SaveQuery(name, text string) (string, error) {
	name = fileName(name)
	return name, os.WriteFile(filepath.Join(s.queriesDir(), name+".sql"), []byte(text), 0o600)
}

// DeleteQuery removes a saved query.
func (s *Store) DeleteQuery(name string) error {
	return os.Remove(filepath.Join(s.queriesDir(), fileName(name)+".sql"))
}

// State is small application state persisted between sessions.
type State struct {
	LastConnection string `yaml:"last_connection,omitempty"`
	LastDatabase   string `yaml:"last_database,omitempty"`
}

// LoadState reads the state file; a missing file yields an empty State.
func (s *Store) LoadState() State {
	var st State
	if data, err := os.ReadFile(s.statePath()); err == nil {
		yaml.Unmarshal(data, &st)
	}
	return st
}

// SaveState writes the state file.
func (s *Store) SaveState(st State) error {
	data, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(s.statePath(), data, 0o600)
}
