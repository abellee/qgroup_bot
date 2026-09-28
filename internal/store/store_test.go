package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want an error")
	}
}

func TestAdminRoundTrip(t *testing.T) {
	st := newStore(t)

	if n, err := st.CountAdmins(); err != nil || n != 0 {
		t.Fatalf("CountAdmins = %d, %v, want 0, nil on a fresh database", n, err)
	}
	if _, err := st.CreateAdmin("operator", "hash-1"); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if _, err := st.CreateAdmin("operator", "hash-2"); err == nil {
		t.Fatal("CreateAdmin accepted a duplicate username")
	}

	a, err := st.AdminByUsername("operator")
	if err != nil {
		t.Fatalf("AdminByUsername: %v", err)
	}
	if a.PassHash != "hash-1" {
		t.Errorf("PassHash = %q, want the stored hash", a.PassHash)
	}
	if _, err := st.AdminByUsername("nobody"); err != ErrNotFound {
		t.Errorf("AdminByUsername(nobody) err = %v, want ErrNotFound", err)
	}

	if err := st.UpdateAdminPassword(a.ID, "hash-9"); err != nil {
		t.Fatalf("UpdateAdminPassword: %v", err)
	}
	a, _ = st.AdminByUsername("operator")
	if a.PassHash != "hash-9" {
		t.Errorf("PassHash after update = %q", a.PassHash)
	}
}

func TestAdminUsernameUpdate(t *testing.T) {
	st := newStore(t)

	id, err := st.CreateAdmin("operator", "hash-1")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if err := st.UpdateAdminUsername(id, "root"); err != nil {
		t.Fatalf("UpdateAdminUsername: %v", err)
	}

	if _, err := st.AdminByUsername("operator"); err != ErrNotFound {
		t.Errorf("AdminByUsername(old) err = %v, want ErrNotFound", err)
	}
	a, err := st.AdminByUsername("root")
	if err != nil {
		t.Fatalf("AdminByUsername(new): %v", err)
	}
	if a.ID != id || a.PassHash != "hash-1" {
		t.Errorf("AdminByUsername(new) = %+v, want the same account with its hash intact", a)
	}
}

func openRow(name, provider string) *ModelConfig {
	return &ModelConfig{
		Name:        name,
		Provider:    provider,
		BaseURL:     "https://api.example.com/v1",
		APIKey:      "sk-secret",
		Model:       "some-model",
		Temperature: 0.7,
	}
}

// A row cannot be stored without the four things a call needs, and the knobs get
// their defaults here rather than at request time.
func TestSaveModelValidationAndClamp(t *testing.T) {
	st := newStore(t)

	bad := &ModelConfig{Persona: "说人话"}
	if err := st.SaveModel(bad); err == nil {
		t.Fatal("SaveModel accepted an empty row")
	} else {
		for _, want := range []string{"name", "provider", "api base url", "api key", "model"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want %q listed as missing", err, want)
			}
		}
	}

	row := openRow("主用", ProviderOpenAI)
	if err := st.SaveModel(row); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}
	if row.MaxTokens != 1024 || row.TimeoutMS != 45000 {
		t.Errorf("defaults after clamp: max_tokens=%d timeout_ms=%d, want 1024 and 45000", row.MaxTokens, row.TimeoutMS)
	}
	if row.ID == 0 {
		t.Error("SaveModel did not fill in the inserted id")
	}
}

// Enabling a row must retire whichever row was active, so the reply path never
// has to break a tie.
func TestOnlyOneRowStaysActive(t *testing.T) {
	st := newStore(t)

	first := openRow("first", ProviderOpenAI)
	first.Enabled = true
	if err := st.SaveModel(first); err != nil {
		t.Fatalf("SaveModel first: %v", err)
	}
	second := openRow("second", ProviderAnthropic)
	if err := st.SaveModel(second); err != nil {
		t.Fatalf("SaveModel second: %v", err)
	}
	if err := st.EnableModel(second.ID); err != nil {
		t.Fatalf("EnableModel: %v", err)
	}

	active, ok, err := st.ActiveModel()
	if err != nil || !ok {
		t.Fatalf("ActiveModel = %v, %v, %v, want the second row", active, ok, err)
	}
	if active.ID != second.ID || active.Provider != ProviderAnthropic {
		t.Errorf("active = %+v, want the row that was enabled last", active)
	}

	rows, err := st.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	var enabled int
	for _, r := range rows {
		if r.Enabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Errorf("enabled rows = %d, want exactly one", enabled)
	}

	if err := st.DisableModel(second.ID); err != nil {
		t.Fatalf("DisableModel: %v", err)
	}
	if _, ok, _ := st.ActiveModel(); ok {
		t.Error("ActiveModel still reports a row after it was disabled")
	}
}

// An edit that does not re-supply the key keeps the stored one: the panel shows
// the key masked and an empty field means "unchanged".
func TestSaveModelUpdateKeepsTheRow(t *testing.T) {
	st := newStore(t)

	row := openRow("主用", ProviderGemini)
	row.Enabled = true
	row.Persona = "你是群助手"
	if err := st.SaveModel(row); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}

	got, err := st.GetModel(row.ID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	got.Name = "改名"
	got.APIKey = "rotated-key"
	if err := st.SaveModel(got); err != nil {
		t.Fatalf("SaveModel update: %v", err)
	}

	again, _ := st.GetModel(row.ID)
	if again.Name != "改名" || again.APIKey != "rotated-key" || again.Persona != "你是群助手" {
		t.Errorf("row after update = %+v, want the edited fields and the untouched persona", again)
	}
	if !again.Enabled {
		t.Error("an edit lost the enabled flag")
	}

	if err := st.DeleteModel(row.ID); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if _, err := st.GetModel(row.ID); err != ErrNotFound {
		t.Errorf("GetModel after delete = %v, want ErrNotFound", err)
	}
	if err := st.DeleteModel(row.ID); err != ErrNotFound {
		t.Errorf("DeleteModel twice = %v, want ErrNotFound", err)
	}
	if err := st.EnableModel(4242); err != ErrNotFound {
		t.Errorf("EnableModel(missing) = %v, want ErrNotFound", err)
	}
}

// A reopening of the same file finds what was written before, which is the whole
// point of not keeping this in memory.
func TestStoreReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "again.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	row := openRow("主用", ProviderOpenAI)
	row.Enabled = true
	if err := st.SaveModel(row); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}
	if _, err := st.CreateAdmin("operator", "hash"); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if _, ok, err := reopened.ActiveModel(); err != nil || !ok {
		t.Errorf("ActiveModel after reopen = %v, %v, want the stored row", ok, err)
	}
	if n, _ := reopened.CountAdmins(); n != 1 {
		t.Errorf("CountAdmins after reopen = %d, want 1", n)
	}
}

// The fallback list rides with the row through save and load, and a database
// made by an older build gains the column on open instead of breaking.
func TestFallbackRepliesRoundTripAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	row := openRow("主用", ProviderOpenAI)
	row.FallbackReplies = "第一条\n第二条\n"
	if err := st.SaveModel(row); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Roll the schema back to what an older build left behind.
	aged, err := Open(path)
	if err != nil {
		t.Fatalf("reopen for aging: %v", err)
	}
	if _, err := aged.db.Exec(`ALTER TABLE model_configs DROP COLUMN fallback_replies`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := aged.db.Exec(`INSERT INTO settings (key, value) VALUES ('keep', 'alive')`); err != nil {
		t.Fatalf("keep the file busy: %v", err)
	}
	aged.Close()

	fresh, err := Open(path)
	if err != nil {
		t.Fatalf("Open after drop: %v", err)
	}
	defer fresh.Close()

	got, err := fresh.GetModel(row.ID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got.FallbackReplies != "" {
		t.Errorf("FallbackReplies after migration = %q, want the column's empty default", got.FallbackReplies)
	}

	got.FallbackReplies = "第一条\n第二条"
	if err := fresh.SaveModel(got); err != nil {
		t.Fatalf("SaveModel after migration: %v", err)
	}
	if again, err := fresh.GetModel(row.ID); err != nil || again.FallbackReplies != "第一条\n第二条" {
		t.Errorf("FallbackReplies = %q, %v, want the saved lines", again.FallbackReplies, err)
	}
}
