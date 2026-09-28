package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"qgroup-bot/internal/store"
)

// modelDTO is one row as the app sees it. The stored key is never part of it:
// only the mask and whether one exists, so an edit can leave the field blank to
// mean "keep what is there".
type modelDTO struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	KeyMasked   string  `json:"key_masked"`
	KeyGiven    bool    `json:"key_given"`
	Model       string  `json:"model"`
	Persona     string  `json:"persona"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	TimeoutMS   int     `json:"timeout_ms"`
	Enabled     bool    `json:"enabled"`
	UpdatedAt   string  `json:"updated_at"`
}

func dtoOf(m *store.ModelConfig) modelDTO {
	return modelDTO{
		ID:          m.ID,
		Name:        m.Name,
		Provider:    m.Provider,
		BaseURL:     m.BaseURL,
		KeyMasked:   maskKey(m.APIKey),
		KeyGiven:    strings.TrimSpace(m.APIKey) != "",
		Model:       m.Model,
		Persona:     m.Persona,
		Temperature: m.Temperature,
		MaxTokens:   m.MaxTokens,
		TimeoutMS:   m.TimeoutMS,
		Enabled:     m.Enabled,
		UpdatedAt:   m.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// saveRequest is the edit form. APIKey is the only field where blank means
// something other than "empty": on an existing row it means unchanged.
type saveRequest struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`
	Persona     string  `json:"persona"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	TimeoutMS   int     `json:"timeout_ms"`
	Enabled     bool    `json:"enabled"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}

	addr := clientAddr(r)
	if wait := s.lockedFor(addr); wait > 0 {
		writeError(w, http.StatusTooManyRequests, "失败次数过多，请 "+fmtWait(wait)+" 后重试")
		return
	}

	name := strings.TrimSpace(in.Username)
	admin, err := s.st.AdminByUsername(name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		admin = nil
	case err != nil:
		s.log.Error("admin lookup failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "登录暂时不可用")
		return
	}

	if admin == nil || !passwordMatches(admin, in.Password) {
		s.noteFailure(addr)
		writeError(w, http.StatusUnauthorized, "用户名或密码不正确")
		return
	}

	s.noteSuccess(addr)
	sess := s.startSession(w, r, admin.ID, admin.Username)
	s.log.Info("admin login", "username", admin.Username, "addr", addr)
	writeJSON(w, http.StatusOK, sessionDTO{Authenticated: true, Username: sess.username, CSRF: sess.csrf})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	s.dropSession(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: s.path, MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, sessionDTO{Authenticated: false})
}

// sessionDTO answers the one question the app asks before it renders anything:
// is there a session, and what token must its requests carry.
type sessionDTO struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
	CSRF          string `json:"csrf,omitempty"`
}

// handleSession is deliberately not behind authed: an unauthenticated answer is
// the normal case, not an error worth a 401 in the console.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	sess := s.session(r)
	if sess == nil {
		writeJSON(w, http.StatusOK, sessionDTO{Authenticated: false})
		return
	}
	writeJSON(w, http.StatusOK, sessionDTO{Authenticated: true, Username: sess.username, CSRF: sess.csrf})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.st.ListModels()
		if err != nil {
			s.log.Error("model list failed", "error", err)
			writeError(w, http.StatusInternalServerError, "读取失败")
			return
		}
		out := make([]modelDTO, 0, len(rows))
		for i := range rows {
			out = append(out, dtoOf(&rows[i]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": out})
	case http.MethodPost:
		s.saveModel(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "只接受 GET 或 POST")
	}
}

// saveModel writes both create and update. A rejected row comes back as a 400
// with the store's own complaint, which already names the missing fields.
func (s *Server) saveModel(w http.ResponseWriter, r *http.Request) {
	var in saveRequest
	if !decode(w, r, &in) {
		return
	}

	cfg := store.ModelConfig{
		ID:          in.ID,
		Name:        strings.TrimSpace(in.Name),
		Provider:    strings.ToLower(strings.TrimSpace(in.Provider)),
		BaseURL:     strings.TrimSpace(in.BaseURL),
		APIKey:      strings.TrimSpace(in.APIKey),
		Model:       strings.TrimSpace(in.Model),
		Persona:     in.Persona,
		Temperature: in.Temperature,
		MaxTokens:   in.MaxTokens,
		TimeoutMS:   in.TimeoutMS,
		Enabled:     in.Enabled,
	}

	if cfg.ID != 0 {
		existing, err := s.st.GetModel(cfg.ID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "这条配置已经不存在了")
			return
		}
		if err != nil {
			s.log.Error("model lookup failed", "id", cfg.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "读取失败")
			return
		}
		if cfg.APIKey == "" {
			cfg.APIKey = existing.APIKey
		}
	}

	if err := s.st.SaveModel(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "保存失败："+err.Error())
		return
	}
	s.log.Info("model config saved", "id", cfg.ID, "provider", cfg.Provider, "model", cfg.Model, "enabled", cfg.Enabled)
	writeJSON(w, http.StatusOK, dtoOf(&cfg))
}

// idRequest is the body of the two one-field calls the list page makes.
type idRequest struct {
	ID int64 `json:"id"`
}

func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
	id, ok := s.readID(w, r)
	if !ok {
		return
	}
	if err := s.st.EnableModel(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "这条配置已经不存在了")
			return
		}
		s.log.Error("enable model failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "启用失败")
		return
	}
	s.log.Info("model config enabled", "id", id)
	active, _, _ := s.st.ActiveModel()
	writeJSON(w, http.StatusOK, map[string]any{"active_id": activeID(active)})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.readID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteModel(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "这条配置已经不存在了")
			return
		}
		s.log.Error("delete model failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "删除失败")
		return
	}
	s.log.Info("model config deleted", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted_id": id})
}

func (s *Server) readID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return 0, false
	}
	var in idRequest
	if !decode(w, r, &in) {
		return 0, false
	}
	if in.ID <= 0 {
		writeError(w, http.StatusBadRequest, "缺少 id")
		return 0, false
	}
	return in.ID, true
}

// handleProviders hands the app its own provider list, so the dropdown can never
// offer a value the store would reject.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providerLabels})
}

func activeID(m *store.ModelConfig) int64 {
	if m == nil {
		return 0
	}
	return m.ID
}

func passwordMatches(admin *store.Admin, plain string) bool {
	if len(plain) == 0 || len(plain) > 72 {
		// bcrypt would silently truncate the second case; refuse it instead.
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(admin.PassHash), []byte(plain)) == nil
}

// decode reads a JSON body and answers the request itself when it cannot. The
// driver text is included because this panel only ever has one user, and a
// rejected field name is otherwise invisible from the browser.
func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer body.Close()

	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "请求体为空")
			return false
		}
		writeError(w, http.StatusBadRequest, "请求格式不正确："+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("json encode failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func fmtWait(d time.Duration) string {
	return strconv.Itoa(int(d.Minutes())+1) + " 分钟"
}
