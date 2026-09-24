package approval

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"qgroup-bot/internal/qqbot"
)

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

type Directory interface {
	UserExists(ctx context.Context, email string) (bool, error)
}

// Service decides the fate of one join request: the applicant's verification
// message must contain the email of a registered sub2api account.
//
// Every decision is logged; anything that cannot be judged (sub2api unreachable
// or answering in an unexpected way) is left pending so a human still sees it.
type Service struct {
	qq           *qqbot.Client
	dir          Directory
	groups       map[string]struct{}
	rejectReason string
	log          *slog.Logger
}

func NewService(qq *qqbot.Client, dir Directory, groups map[string]struct{}, rejectReason string, log *slog.Logger) *Service {
	return &Service{qq: qq, dir: dir, groups: groups, rejectReason: rejectReason, log: log}
}

func (s *Service) HandleJoin(ctx context.Context, ev *qqbot.JoinRequestEvent) {
	if _, ok := s.groups[ev.GroupOpenID]; !ok {
		s.log.Info("join review skipped: group not managed",
			"group_openid", ev.GroupOpenID, "member_openid", ev.MemberOpenID)
		return
	}
	if ev.ApplySource == qqbot.ApplySourceInvited {
		s.log.Info("join review left to manual: invited by a member",
			"group_openid", ev.GroupOpenID, "member_openid", ev.MemberOpenID, "invited_by", ev.InvitedBy)
		return
	}

	logFields := []any{
		"group_openid", ev.GroupOpenID,
		"member_openid", ev.MemberOpenID,
		"join_request_id", ev.JoinRequestID,
		"username", ev.Username,
		"verify_method", ev.VerifyInfo.Method,
	}
	start := time.Now()

	if ev.Bot {
		s.decide(ctx, ev, "decline", append(logFields, "reason", "robot account")...)
		return
	}
	email, ok := ExtractEmail(ev.VerifyInfo.VerifyMessage)
	if !ok {
		s.decide(ctx, ev, "decline", append(logFields, "reason", "no email in verify message")...)
		return
	}
	logFields = append(logFields, "email", MaskEmail(email))

	found, err := s.dir.UserExists(ctx, email)
	if err != nil {
		s.log.Error("join review deferred: sub2api lookup failed",
			append(logFields, "error", err)...)
		return
	}
	if found {
		s.decide(ctx, ev, "approve", append(logFields, "took", time.Since(start).String())...)
		return
	}
	s.decide(ctx, ev, "decline", append(logFields, "reason", "email not registered")...)
}

func (s *Service) decide(ctx context.Context, ev *qqbot.JoinRequestEvent, action string, fields ...any) {
	var err error
	if action == "approve" {
		err = s.qq.ApproveJoin(ctx, ev)
	} else {
		err = s.qq.DeclineJoin(ctx, ev, s.rejectReason)
	}
	if err != nil {
		s.log.Error("join review failed", append(fields, "action", action, "error", err)...)
		return
	}
	s.log.Info("join review", append(fields, "action", action)...)
}

// ExtractEmail finds the first email-looking token in free text and normalises
// it, since sub2api looks accounts up by their stored (lower-cased) email.
func ExtractEmail(text string) (string, bool) {
	m := emailPattern.FindString(strings.TrimSpace(text))
	if m == "" {
		return "", false
	}
	return strings.ToLower(m), true
}

// MaskEmail keeps logs readable without persisting full applicant addresses.
func MaskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "***"
	}
	local, domain := email[:at], email[at+1:]
	keep := 1
	if len(local) < keep {
		keep = len(local)
	}
	return fmt.Sprintf("%s***@%s", local[:keep], domain)
}
