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

// MessageHandler consumes a decoded group message, which is where a reply to a
// member is anchored.
type MessageHandler func(ctx context.Context, ev *GroupMessageEvent)

type Handler struct {
	keys      *KeyPair
	appID     string
	maxSkew   time.Duration
	onJoin    JoinHandler
	onMessage MessageHandler
	log       *slog.Logger
	queue     chan *JoinRequestEvent
	msgQueue  chan *GroupMessageEvent
}

func NewHandler(appID, botSecret string, maxSkew time.Duration, queueSize int, onJoin JoinHandler, onMessage MessageHandler, log *slog.Logger) (*Handler, error) {
	keys, err := NewKeyPair(botSecret)
	if err != nil {
		return nil, err
	}
	if queueSize <= 0 {
		queueSize = 128
	}
	return &Handler{
		keys:      keys,
		appID:     appID,
		maxSkew:   maxSkew,
		onJoin:    onJoin,
		onMessage: onMessage,
		log:       log,
		queue:     make(chan *JoinRequestEvent, queueSize),
		msgQueue:  make(chan *GroupMessageEvent, queueSize),
	}, nil
}

// Start runs one worker per queue. They are separate because a model-backed
// reply can take tens of seconds, and a join request that arrived behind it
// should still be reviewed while the answer is in flight. Within each queue the
// order the platform sent is kept, and a message is handled one at a time so a
// burst of mentions cannot turn into a stampede of provider calls.
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
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-h.msgQueue:
				h.onMessage(ctx, ev)
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

	switch env.T {
	case EventGroupJoinRequest:
		var ev JoinRequestEvent
		if err := json.Unmarshal(env.D, &ev); err != nil {
			h.log.Warn("bad join request event", "id", env.ID, "error", err)
			return
		}
		ev.Raw = env.D
		ev.EventID = env.ID
		select {
		case h.queue <- &ev:
		default:
			h.log.Error("join queue full, leaving request for manual review",
				"join_request_id", ev.JoinRequestID, "group_openid", ev.GroupOpenID)
		}

	case EventGroupAtMessageCreate, EventGroupMessageCreate:
		var ev GroupMessageEvent
		if err := json.Unmarshal(env.D, &ev); err != nil {
			h.log.Warn("bad group message event", "t", env.T, "id", env.ID, "error", err)
			return
		}
		ev.Raw = env.D
		ev.Kind = env.T
		select {
		case h.msgQueue <- &ev:
		default:
			h.log.Error("message queue full, dropping group message",
				"t", env.T, "msg_id", ev.ID, "group_openid", ev.GroupOpenID)
		}

	default:
		// Logged at info rather than debug because which event types the app is
		// actually subscribed to is only visible on the wire.
		h.log.Info("ignored event", "t", env.T, "id", env.ID)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response failed", "error", err)
	}
}
