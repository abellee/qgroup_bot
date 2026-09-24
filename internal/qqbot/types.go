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
}

const (
	ApplySourceSelf    = "self_apply"
	ApplySourceInvited = "invited"
)
