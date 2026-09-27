package qqbot

import (
	"encoding/json"
)

// Opcodes of the shared webhook/websocket payload envelope.
const (
	OpDispatch      = 0
	OpCallbackACK   = 12
	OpURLValidation = 13
)

// EventGroupJoinRequest is the envelope `t` value for a user applying to join a group.
const EventGroupJoinRequest = "GROUP_JOIN_REQUEST"

// The two shapes of a pushed group message. `GROUP_AT_MESSAGE_CREATE` fires only
// when a member addresses the bot; `GROUP_MESSAGE_CREATE` is the full-traffic
// mode and needs the app to have 接收所有消息 enabled. Both carry the message id
// that a reply is built on.
const (
	EventGroupAtMessageCreate = "GROUP_AT_MESSAGE_CREATE"
	EventGroupMessageCreate   = "GROUP_MESSAGE_CREATE"
)

type Envelope struct {
	ID string          `json:"id"`
	Op int             `json:"op"`
	S  json.RawMessage `json:"s"`
	T  string          `json:"t"`
	D  json.RawMessage `json:"d"`
}

type ValidationRequest struct {
	PlainToken string `json:"plain_token"`
	EventTs    string `json:"event_ts"`
}

type ValidationResponse struct {
	PlainToken string `json:"plain_token"`
	Signature  string `json:"signature"`
}

type VerifyInfo struct {
	Method        string `json:"method"`
	VerifyMessage string `json:"verify_message"`
	ReviewQAList  []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	} `json:"review_qa_list"`
}

type JoinRequestEvent struct {
	GroupOpenID   string     `json:"group_openid"`
	JoinRequestID string     `json:"join_request_id"`
	MemberOpenID  string     `json:"member_openid"`
	UnionOpenID   string     `json:"union_openid"`
	Username      string     `json:"username"`
	ApplyAt       string     `json:"apply_at"`
	ApplySource   string     `json:"apply_source"`
	InvitedBy     string     `json:"invited_by"`
	Bot           bool       `json:"bot"`
	VerifyInfo    VerifyInfo `json:"verify_info"`
	AutoApproved  *struct {
		StrategyID string `json:"strategy_id"`
	} `json:"auto_approved"`

	// Raw is the untouched event body, filled in by the webhook layer. The
	// documented field list has no plain QQ number, so keeping the original
	// JSON is how an undocumented identity field stays discoverable in logs.
	Raw json.RawMessage `json:"-"`

	// EventID is the dispatch envelope's `id`, filled in by the webhook layer.
	// It is log correlation only: a join event cannot be replied to, which is
	// why the welcome note waits for one of the member's messages instead.
	EventID string `json:"-"`
}

// GroupMessageEvent is the `d` of either pushed group message event.
type GroupMessageEvent struct {
	// ID is the message id the send endpoint accepts as `msg_id`. It is the
	// only reply credential this app can obtain in a group, and it expires
	// five minutes after the message.
	ID          string `json:"id"`
	Timestamp   string `json:"timestamp"`
	GroupOpenID string `json:"group_openid"`
	Content     string `json:"content"`
	MessageType int    `json:"message_type"`
	Author      struct {
		ID           string `json:"id"`
		Username     string `json:"username"`
		MemberOpenID string `json:"member_openid"`
	} `json:"author"`

	// Raw keeps the untouched body, so fields the docs do not list stay
	// discoverable in the logs while the event shape is confirmed live.
	Raw json.RawMessage `json:"-"`
}

const (
	ApplySourceSelf    = "self_apply"
	ApplySourceInvited = "invited"
)
