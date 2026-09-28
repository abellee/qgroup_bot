// Package store keeps the two things this service cannot hold in memory: the
// administrator account and the model configurations the back-end edits. SQLite
// in a single file on a mounted volume is enough for a personal admin panel, and
// it survives restarts the way an env var cannot.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound marks a row that the caller asked for and the database does not
// have, so handlers can answer 404 without matching on driver text.
var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

// Open prepares the database file. The pragmas matter here: the admin panel and
// the callback worker touch the same file, and without a busy timeout a
// concurrent write fails instead of waiting.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		strings.ReplaceAll(path, `\`, `/`))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection: SQLite allows a single writer, so a pool only turns
	// contention into SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(time.Hour)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
	id         INTEGER PRIMARY KEY,
	username   TEXT NOT NULL UNIQUE,
	pass_hash  TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS model_configs (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL,
	provider    TEXT NOT NULL,
	base_url    TEXT NOT NULL,
	api_key     TEXT NOT NULL,
	model       TEXT NOT NULL,
	persona     TEXT NOT NULL DEFAULT '',
	temperature REAL NOT NULL DEFAULT 1,
	max_tokens  INTEGER NOT NULL DEFAULT 1024,
	timeout_ms  INTEGER NOT NULL DEFAULT 45000,
	enabled     INTEGER NOT NULL DEFAULT 0,
	updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS model_configs_enabled ON model_configs (enabled);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate sqlite schema: %w", err)
	}
	return nil
}

// Setting reads one row of the small key/value table. A missing key reads as an
// empty value, so callers can treat "unset" as the normal first-run case.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting writes one row over whatever was there.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Admin is one administrator. PassHash is never handed to a template; it is
// only used to verify a login.
type Admin struct {
	ID       int64
	Username string
	PassHash string
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM admin_users`).Scan(&n)
	return n, err
}

// CreateAdmin stores the first account, built from the env at startup. A
// duplicate username is refused rather than silently overwriting a hash.
func (s *Store) CreateAdmin(username, passHash string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO admin_users (username, pass_hash, created_at) VALUES (?, ?, ?)`,
		username, passHash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) AdminByUsername(username string) (*Admin, error) {
	var a Admin
	err := s.db.QueryRow(`SELECT id, username, pass_hash FROM admin_users WHERE username = ?`, username).
		Scan(&a.ID, &a.Username, &a.PassHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) UpdateAdminPassword(id int64, passHash string) error {
	_, err := s.db.Exec(`UPDATE admin_users SET pass_hash = ? WHERE id = ?`, passHash, id)
	return err
}
