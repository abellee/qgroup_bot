package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const maxBodyBytes = 1 << 20

// JoinHandler consumes a decoded join request away from the HTTP path, so the
// platform gets its ACK inside the callback timeout even when sub2api is slow.
type JoinHandler func(ctx context.Context, ev *JoinRequestEvent)

type Handler struct {
	keys    *KeyPair
	appID   string
	maxSkew time.Duration
	onJoin  JoinHandler
	log     *slog.Logger
	queue   chan *JoinRequestEvent
}

func NewHandler(appID, botSecret string, maxSkew time.Duration, queueSize int, onJoin JoinHandler, log *slog.Logger) (*Handler, error) {
	keys, err := NewKeyPair(botSecret)
	if err != nil {
		return nil, err
	}
	if queueSize <= 0 {
		queueSize = 128
	}
	return &Handler{
		keys:    keys,
		appID:   appID,
		maxSkew: maxSkew,
		onJoin:  onJoin,
		log:     log,
		queue:   make(chan *JoinRequestEvent, queueSize),
	}, nil
}

// Start runs the single worker draining join requests. One worker keeps
// approvals ordered per process and is plenty for a 60 QPM endpoint.
func (h *Handler) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-h.queue:
				h.onJoin(ctx, ev)
			}
		}
	}()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if appid := r.Header.Get("X-Bot-Appid"); appid != "" && appid != h.appID {
		h.log.Warn("callback for another app", "header_appid", appid)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		h.log.Warn("unparseable callback", "error", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	if err := h.keys.Verify(r.Header.Get("X-Signature-Timestamp"), r.Header.Get("X-Signature-Ed25519"), body, h.maxSkew); err != nil {
		unsignedValidation := env.Op == OpURLValidation && errors.Is(err, ErrMissingSignature)
		if !unsignedValidation {
			h.log.Warn("rejected callback", "op", env.Op, "error", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.log.Info("address verification arriving without signature headers")
	}

	switch env.Op {
	case OpURLValidation:
		h.handleValidation(w, env)
	case OpDispatch:
		h.handleDispatch(w, env)
	default:
		h.log.Info("ignored callback op", "op", env.Op, "t", env.T)
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

func (h *Handler) handleValidation(w http.ResponseWriter, env Envelope) {
	var req ValidationRequest
	if err := json.Unmarshal(env.D, &req); err != nil {
		h.log.Warn("bad validation payload", "error", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if req.PlainToken == "" || req.EventTs == "" {
		h.log.Warn("validation payload missing fields", "event_ts", req.EventTs)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	sig := h.keys.Sign([]byte(req.EventTs + req.PlainToken))
	h.log.Info("callback address verified", "event_ts", req.EventTs)
	writeJSON(w, http.StatusOK, ValidationResponse{PlainToken: req.PlainToken, Signature: sig})
}

func (h *Handler) handleDispatch(w http.ResponseWriter, env Envelope) {
	writeJSON(w, http.StatusOK, map[string]any{"op": OpCallbackACK})

	if env.T != EventGroupJoinRequest {
		h.log.Debug("unhandled event", "t", env.T, "id", env.ID)
		return
	}
	var ev JoinRequestEvent
	if err := json.Unmarshal(env.D, &ev); err != nil {
		h.log.Warn("bad join request event", "id", env.ID, "error", err)
		return
	}
	select {
	case h.queue <- &ev:
	default:
		h.log.Error("join queue full, leaving request for manual review",
			"join_request_id", ev.JoinRequestID, "group_openid", ev.GroupOpenID)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response failed", "error", err)
	}
}
