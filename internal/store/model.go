package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Providers the reply path speaks. Each one has its own request shape in
// internal/llm; nothing else in the code branches on these strings.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
)

// ValidProvider reports whether p is one of the three the panel offers.
func ValidProvider(p string) bool {
	switch p {
	case ProviderOpenAI, ProviderAnthropic, ProviderGemini:
		return true
	}
	return false
}

// ModelConfig is one entry of the 模型 menu: the credentials, the persona, and
// the sampling knobs needed to actually call the provider.
type ModelConfig struct {
	ID          int64
	Name        string
	Provider    string
	BaseURL     string
	APIKey      string
	Model       string
	Persona     string
	Temperature float64
	MaxTokens   int
	TimeoutMS   int
	Enabled     bool
	UpdatedAt   time.Time
}

// Clamp defaults the knobs an empty form leaves behind, so a row saved from the
// panel is always callable.
func (m *ModelConfig) Clamp() {
	m.Name = strings.TrimSpace(m.Name)
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	m.BaseURL = strings.TrimSpace(m.BaseURL)
	m.Model = strings.TrimSpace(m.Model)
	if m.MaxTokens <= 0 {
		m.MaxTokens = 1024
	}
	if m.TimeoutMS <= 0 {
		m.TimeoutMS = 45000
	}
	if m.TimeoutMS > 300000 {
		m.TimeoutMS = 300000
	}
	// Negative temperatures are rejected by every one of the three providers.
	if m.Temperature < 0 {
		m.Temperature = 0
	}
	if m.Name == "" {
		m.Name = m.Model
	}
}

// Validate is the last gate before a write: the panel is trivially reachable
// by its own user, so a half-filled row must not become a silent send failure.
func (m *ModelConfig) Validate() error {
	var missing []string
	if m.Name == "" {
		missing = append(missing, "name")
	}
	if !ValidProvider(m.Provider) {
		missing = append(missing, "provider")
	}
	if m.BaseURL == "" {
		missing = append(missing, "api base url")
	}
	if m.APIKey == "" {
		missing = append(missing, "api key")
	}
	if m.Model == "" {
		missing = append(missing, "model")
	}
	if len(missing) > 0 {
		return errors.New("required fields missing: " + strings.Join(missing, ", "))
	}
	return nil
}

func (s *Store) ListModels() ([]ModelConfig, error) {
	rows, err := s.db.Query(`SELECT id, name, provider, base_url, api_key, model, persona,
		temperature, max_tokens, timeout_ms, enabled, updated_at
		FROM model_configs ORDER BY enabled DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelConfig
	for rows.Next() {
		var (
			m         ModelConfig
			enabled   int
			updatedAT string
		)
		if err := rows.Scan(&m.ID, &m.Name, &m.Provider, &m.BaseURL, &m.APIKey, &m.Model,
			&m.Persona, &m.Temperature, &m.MaxTokens, &m.TimeoutMS, &enabled, &updatedAT); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAT)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetModel(id int64) (*ModelConfig, error) {
	var (
		m         ModelConfig
		enabled   int
		updatedAT string
	)
	err := s.db.QueryRow(`SELECT id, name, provider, base_url, api_key, model, persona,
		temperature, max_tokens, timeout_ms, enabled, updated_at
		FROM model_configs WHERE id = ?`, id).
		Scan(&m.ID, &m.Name, &m.Provider, &m.BaseURL, &m.APIKey, &m.Model,
			&m.Persona, &m.Temperature, &m.MaxTokens, &m.TimeoutMS, &enabled, &updatedAT)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Enabled = enabled != 0
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAT)
	return &m, nil
}

// ActiveModel is the configuration the reply path uses. Not found is not an
// error worth failing on: a bot with no model configured simply has nothing to
// answer with.
func (s *Store) ActiveModel() (*ModelConfig, bool, error) {
	var (
		m         ModelConfig
		enabled   int
		updatedAT string
	)
	err := s.db.QueryRow(`SELECT id, name, provider, base_url, api_key, model, persona,
		temperature, max_tokens, timeout_ms, enabled, updated_at
		FROM model_configs WHERE enabled = 1 ORDER BY id LIMIT 1`).
		Scan(&m.ID, &m.Name, &m.Provider, &m.BaseURL, &m.APIKey, &m.Model,
			&m.Persona, &m.Temperature, &m.MaxTokens, &m.TimeoutMS, &enabled, &updatedAT)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m.Enabled = true
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAT)
	return &m, true, nil
}

// SaveModel inserts when ID is zero and updates otherwise. Enabling a row
// disables every other one in the same transaction, because two active
// configurations would make the reply path's choice arbitrary.
func (s *Store) SaveModel(m *ModelConfig) error {
	m.Clamp()
	if err := m.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if m.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO model_configs
			(name, provider, base_url, api_key, model, persona, temperature, max_tokens, timeout_ms, enabled, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.Name, m.Provider, m.BaseURL, m.APIKey, m.Model, m.Persona,
			m.Temperature, m.MaxTokens, m.TimeoutMS, boolInt(m.Enabled), now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		m.ID = id
		if !m.Enabled {
			return nil
		}
		return s.enableOnly(id)
	}

	if _, err := s.db.Exec(`UPDATE model_configs SET
		name = ?, provider = ?, base_url = ?, api_key = ?, model = ?, persona = ?,
		temperature = ?, max_tokens = ?, timeout_ms = ?, updated_at = ?
		WHERE id = ?`,
		m.Name, m.Provider, m.BaseURL, m.APIKey, m.Model, m.Persona,
		m.Temperature, m.MaxTokens, m.TimeoutMS, now, m.ID); err != nil {
		return err
	}
	if m.Enabled {
		return s.enableOnly(m.ID)
	}
	return s.DisableModel(m.ID)
}

// DisableModel clears the active flag on a row that carries it, so an edit that
// unchecks 启用 takes effect even though the enabled column is not part of the
// UPDATE in SaveModel.
func (s *Store) DisableModel(id int64) error {
	_, err := s.db.Exec(`UPDATE model_configs SET enabled = 0 WHERE id = ?`, id)
	return err
}

func (s *Store) EnableModel(id int64) error {
	var found int
	err := s.db.QueryRow(`SELECT count(*) FROM model_configs WHERE id = ?`, id).Scan(&found)
	if err != nil {
		return err
	}
	if found == 0 {
		return ErrNotFound
	}
	return s.enableOnly(id)
}

func (s *Store) DeleteModel(id int64) error {
	res, err := s.db.Exec(`DELETE FROM model_configs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// enableOnly makes one row active in a single statement, so there is no moment
// where two configurations claim the reply path.
func (s *Store) enableOnly(id int64) error {
	_, err := s.db.Exec(`UPDATE model_configs SET enabled = CASE WHEN id = ? THEN 1 ELSE 0 END`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
