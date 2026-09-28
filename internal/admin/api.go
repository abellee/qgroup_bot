package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"qgroup-bot/internal/guard"
	"qgroup-bot/internal/store"
)

// modelDTO is one row as the app sees it. The stored key is never part of it:
// only the mask and whether one exists, so an edit can leave the field blank to
// mean "keep what is there".
type modelDTO struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	BaseURL         string  `json:"base_url"`
	KeyMasked       string  `json:"key_masked"`
	KeyGiven        bool    `json:"key_given"`
	Model           string  `json:"model"`
	Persona         string  `json:"persona"`
	FallbackReplies string  `json:"fallback_replies"`
	Temperature     float64 `json:"temperature"`
	MaxTokens       int     `json:"max_tokens"`
	TimeoutMS       int     `json:"timeout_ms"`
	Enabled         bool    `json:"enabled"`
	UpdatedAt       string  `json:"updated_at"`
}

func dtoOf(m *store.ModelConfig) modelDTO {
	return modelDTO{
		ID:              m.ID,
		Name:            m.Name,
		Provider:        m.Provider,
		BaseURL:         m.BaseURL,
		KeyMasked:       maskKey(m.APIKey),
		KeyGiven:        strings.TrimSpace(m.APIKey) != "",
		Model:           m.Model,
		Persona:         m.Persona,
		FallbackReplies: m.FallbackReplies,
		Temperature:     m.Temperature,
		MaxTokens:       m.MaxTokens,
		TimeoutMS:       m.TimeoutMS,
		Enabled:         m.Enabled,
		UpdatedAt:       m.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// saveRequest is the edit form. APIKey is the only field where blank means
// something other than "empty": on an existing row it means unchanged.
type saveRequest struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	BaseURL         string  `json:"base_url"`
	APIKey          string  `json:"api_key"`
	Model           string  `json:"model"`
	Persona         string  `json:"persona"`
	FallbackReplies string  `json:"fallback_replies"`
	Temperature     float64 `json:"temperature"`
	MaxTokens       int     `json:"max_tokens"`
	TimeoutMS       int     `json:"timeout_ms"`
	Enabled         bool    `json:"enabled"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var in struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if !decode(w, r, &in) {
		return
	}

	addr := guard.ClientAddr(r)
	if wait := s.lockedFor(addr); wait > 0 {
		writeError(w, http.StatusTooManyRequests, "失败次数过多，请 "+fmtWait(wait)+" 后重试")
		return
	}

	// The human check sits between the throttle and the credentials: a token
	// that does not verify never reaches the password, and a throttled address
	// is not charged turnstile quota.
	if s.human != nil {
		if err := s.human.Verify(r.Context(), in.TurnstileToken, addr); err != nil {
			s.log.Warn("login refused by the human check", "addr", addr, "error", err)
			writeError(w, http.StatusForbidden, "人机验证未通过，请刷新后重试")
			return
		}
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
	writeJSON(w, http.StatusOK, s.sessionDTOOf(sess))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	s.dropSession(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: s.path, MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, s.sessionDTOOf(nil))
}

// sessionDTO answers the one question the app asks before it renders anything:
// is there a session, what token must its requests carry, and - for the login
// page - whether a Turnstile widget is part of the deal.
type sessionDTO struct {
	Authenticated    bool   `json:"authenticated"`
	Username         string `json:"username,omitempty"`
	CSRF             string `json:"csrf,omitempty"`
	TurnstileSiteKey string `json:"turnstile_site_key,omitempty"`
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
		writeJSON(w, http.StatusOK, s.sessionDTOOf(nil))
		return
	}
	writeJSON(w, http.StatusOK, s.sessionDTOOf(sess))
}

func (s *Server) sessionDTOOf(sess *session) sessionDTO {
	out := sessionDTO{}
	if s.human != nil {
		out.TurnstileSiteKey = s.siteKey
	}
	if sess == nil {
		return out
	}
	out.Authenticated = true
	out.Username = sess.username
	out.CSRF = sess.csrf
	return out
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
		ID:              in.ID,
		Name:            strings.TrimSpace(in.Name),
		Provider:        strings.ToLower(strings.TrimSpace(in.Provider)),
		BaseURL:         strings.TrimSpace(in.BaseURL),
		APIKey:          strings.TrimSpace(in.APIKey),
		Model:           strings.TrimSpace(in.Model),
		Persona:         in.Persona,
		FallbackReplies: strings.TrimSpace(in.FallbackReplies),
		Temperature:     in.Temperature,
		MaxTokens:       in.MaxTokens,
		TimeoutMS:       in.TimeoutMS,
		Enabled:         in.Enabled,
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

// remoteRequest is the body of the model catalog call: the form's provider and
// base url, plus whatever key the form holds. A blank key on an existing row
// means "keep the stored one", and the stored key is what the provider should
// be asked with - the form never sees it.
type remoteRequest struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

// handleModelRemote proxies the provider's own model list back to the app, so
// the model field can offer real choices. It never logs the key.
func (s *Server) handleModelRemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	if s.tester == nil {
		writeError(w, http.StatusServiceUnavailable, "模型列表通道未接入")
		return
	}
	var in remoteRequest
	if !decode(w, r, &in) {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if !store.ValidProvider(provider) {
		writeError(w, http.StatusBadRequest, "提供商不对，先在表单里选一个")
		return
	}
	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL == "" {
		writeError(w, http.StatusBadRequest, "先填 API 地址")
		return
	}
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" && in.ID > 0 {
		if stored, err := s.st.GetModel(in.ID); err == nil {
			apiKey = stored.APIKey
		}
	}
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, "先填 API Key")
		return
	}

	// A slow relay should not eat the whole server write deadline; the list
	// call is small, so its budget is fixed and short.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
		s.log.Warn("write deadline not extendable", "error", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	ids, err := s.tester.ListModels(ctx, store.ModelConfig{
		Provider: provider,
		BaseURL:  baseURL,
		APIKey:   apiKey,
	})
	if err != nil {
		s.log.Warn("model list pull failed", "provider", provider, "base_url", baseURL, "error", err)
		writeError(w, http.StatusBadGateway, "拉取模型列表失败："+err.Error())
		return
	}
	s.log.Info("model list pulled", "provider", provider, "base_url", baseURL, "count", len(ids))
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

// accountRequest changes the administrator's own credentials. The old password
// is the second factor here: a stolen tab alone cannot rename the account or
// swap the hash.
type accountRequest struct {
	OldPassword string `json:"old_password"`
	Username    string `json:"username"`
	NewPassword string `json:"new_password"`
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	sess := s.session(r)
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}
	var in accountRequest
	if !decode(w, r, &in) {
		return
	}
	admin, err := s.st.AdminByUsername(sess.username)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "账号已不存在，请重新登录")
		return
	}
	if err != nil {
		s.log.Error("admin lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "读取失败")
		return
	}
	if !passwordMatches(admin, in.OldPassword) {
		writeError(w, http.StatusBadRequest, "旧密码不正确")
		return
	}

	name := strings.TrimSpace(in.Username)
	nameChanged := name != "" && name != admin.Username
	passChanged := in.NewPassword != ""
	if !nameChanged && !passChanged {
		writeError(w, http.StatusBadRequest, "没有要修改的内容")
		return
	}

	// Both writes are validated before either lands, so a refused password
	// never leaves a half-applied change behind.
	var hash string
	if passChanged {
		if hash, err = HashPassword(in.NewPassword); err != nil {
			writeError(w, http.StatusBadRequest, "新密码不可用："+err.Error())
			return
		}
	}
	if nameChanged {
		if err := s.st.UpdateAdminUsername(admin.ID, name); err != nil {
			s.log.Error("admin rename failed", "error", err)
			writeError(w, http.StatusBadRequest, "改用户名失败：这个名字可能已被占用")
			return
		}
	}
	if passChanged {
		if err := s.st.UpdateAdminPassword(admin.ID, hash); err != nil {
			s.log.Error("admin password update failed", "error", err)
			writeError(w, http.StatusInternalServerError, "保存失败")
			return
		}
	}

	// Every session dies with the old credentials, the caller's included: the
	// operator signs back in with what they just set.
	s.dropAllSessions()
	s.log.Info("admin account updated", "renamed", nameChanged, "password_changed", passChanged, "addr", guard.ClientAddr(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// testRequest is the body of the test dialog: one stored row and one prompt,
// the same single turn a group @ would send.
type testRequest struct {
	ID     int64  `json:"id"`
	Prompt string `json:"prompt"`
}

// handleModelTest runs one prompt against one row - enabled or not - so a
// credential or a persona can be checked before anything goes to a group.
func (s *Server) handleModelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	if s.tester == nil {
		writeError(w, http.StatusServiceUnavailable, "测试通道未接入")
		return
	}
	var in testRequest
	if !decode(w, r, &in) {
		return
	}
	if in.ID <= 0 {
		writeError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "先输入要发给模型的话")
		return
	}
	cfg, err := s.st.GetModel(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "这条配置已经不存在了")
		return
	}
	if err != nil {
		s.log.Error("model lookup failed", "id", in.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "读取失败")
		return
	}

	// A model answer can outlast the server's write deadline, and the row's own
	// timeout is the real bound, so the deadline moves out to match.
	horizon := time.Duration(cfg.TimeoutMS)*time.Millisecond + 5*time.Second
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(horizon)); err != nil {
		s.log.Warn("write deadline not extendable", "error", err)
	}

	start := time.Now()
	answer, err := s.tester.TestModel(r.Context(), *cfg, prompt)
	if err != nil {
		s.log.Warn("model test failed", "id", cfg.ID, "name", cfg.Name, "provider", cfg.Provider,
			"error", err, "took", time.Since(start).String())
		writeError(w, http.StatusBadGateway, "模型调用失败："+err.Error())
		return
	}
	s.log.Info("model test", "id", cfg.ID, "name", cfg.Name, "provider", cfg.Provider,
		"took", time.Since(start).String())
	writeJSON(w, http.StatusOK, map[string]any{
		"answer":  answer,
		"took_ms": time.Since(start).Milliseconds(),
	})
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
