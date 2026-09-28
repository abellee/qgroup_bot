package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"qgroup-bot/internal/store"
)

// handleModels is the 菜单 the operator asked for: every saved configuration,
// the active one marked, with enable/edit/delete next to each.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.newPage(s.session(r), "模型")
	data.Flash = flash(r)

	rows, err := s.st.ListModels()
	if err != nil {
		data.Err = "读取失败：" + err.Error()
	}
	for i := range rows {
		data.Models = append(data.Models, *viewOf(&rows[i]))
	}
	s.render(w, "models", data)
}

// handleForm shows the same page for a new row and an edit; id=0 means new.
func (s *Server) handleForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.newPage(s.session(r), "模型配置")

	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id == 0 {
		data.Form = &modelView{Temperature: "1", MaxTokens: "1024", TimeoutMS: "45000", Provider: store.ProviderOpenAI}
		s.render(w, "form", data)
		return
	}

	cfg, err := s.st.GetModel(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		data.Form = &modelView{}
		data.Err = "读取失败：" + err.Error()
		s.render(w, "form", data)
		return
	}
	data.Form = viewOf(cfg)
	s.render(w, "form", data)
}

// handleSave writes both create and update. An empty key field on an edit means
// "keep the one already stored", so the operator never retypes a secret they can
// no longer see.
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.newPage(s.session(r), "模型配置")
	cfg := s.formConfig(r)

	if cfg.ID != 0 {
		existing, err := s.st.GetModel(cfg.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			data.Form = echoView(&cfg)
			data.Err = "这条配置已经不存在了"
			s.render(w, "form", data)
			return
		case err != nil:
			data.Form = echoView(&cfg)
			data.Err = "保存失败：" + err.Error()
			s.render(w, "form", data)
			return
		}
		if cfg.APIKey == "" {
			cfg.APIKey = existing.APIKey
		}
	}

	if err := s.st.SaveModel(&cfg); err != nil {
		data.Form = echoView(&cfg)
		data.Err = "保存失败：" + err.Error()
		s.render(w, "form", data)
		return
	}
	s.log.Info("model config saved", "id", cfg.ID, "provider", cfg.Provider, "model", cfg.Model, "enabled", cfg.Enabled)
	http.Redirect(w, r, "/admin/models?flash=saved", http.StatusFound)
}

// handleEnable is the switch the reply path reads. Enabling one row disables the
// others inside the same transaction, so there is never a tie to break.
func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "id 无效", http.StatusBadRequest)
		return
	}
	if err := s.st.EnableModel(id); err != nil {
		s.log.Error("enable model failed", "id", id, "error", err)
		http.Redirect(w, r, "/admin/models", http.StatusFound)
		return
	}
	s.log.Info("model config enabled", "id", id)
	http.Redirect(w, r, "/admin/models?flash=enabled", http.StatusFound)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "id 无效", http.StatusBadRequest)
		return
	}
	if err := s.st.DeleteModel(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("delete model failed", "id", id, "error", err)
	}
	s.log.Info("model config deleted", "id", id)
	http.Redirect(w, r, "/admin/models?flash=deleted", http.StatusFound)
}

func (s *Server) formConfig(r *http.Request) store.ModelConfig {
	return store.ModelConfig{
		ID:          int64(parseIntDefault(r.FormValue("id"), 0)),
		Name:        strings.TrimSpace(r.FormValue("name")),
		Provider:    strings.ToLower(strings.TrimSpace(r.FormValue("provider"))),
		BaseURL:     strings.TrimSpace(r.FormValue("base_url")),
		APIKey:      strings.TrimSpace(r.FormValue("api_key")),
		Model:       strings.TrimSpace(r.FormValue("model")),
		Persona:     r.FormValue("persona"),
		Temperature: parseFloatDefault(r.FormValue("temperature"), 1),
		MaxTokens:   parseIntDefault(r.FormValue("max_tokens"), 1024),
		TimeoutMS:   parseIntDefault(r.FormValue("timeout_ms"), 45000),
		Enabled:     r.FormValue("enabled") == "1",
	}
}

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
}
