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

// Service decides the fate of one join request: the answer given to the group's
// verification question must be the email of a registered sub2api account.
//
// Approving is the only action taken. Every other outcome - no email in the
// answers, an unregistered email, a lookup that fails - leaves the request
// pending for a human, and nothing is ever auto-rejected.
type Service struct {
	qq     *qqbot.Client
	dir    Directory
	groups map[string]struct{}
	log    *slog.Logger
}

func NewService(qq *qqbot.Client, dir Directory, groups map[string]struct{}, log *slog.Logger) *Service {
	return &Service{qq: qq, dir: dir, groups: groups, log: log}
}

func (s *Service) HandleJoin(ctx context.Context, ev *qqbot.JoinRequestEvent) {
	// Logged before any filtering so a request from an unmanaged group is still
	// readable in full: that is how the applicant's text and the group openID
	// are recovered during bootstrap.
	s.log.Info("join request received",
		"group_openid", ev.GroupOpenID,
		"member_openid", ev.MemberOpenID,
		"union_openid", ev.UnionOpenID,
		"username", ev.Username,
		"apply_source", ev.ApplySource,
		"verify_method", ev.VerifyInfo.Method,
		"verify_message", ev.VerifyInfo.VerifyMessage,
		"review_qa", ev.VerifyInfo.ReviewQAList,
		"raw", string(ev.Raw),
	)

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

	email, ok := EmailFromEvent(ev)
	if !ok {
		s.log.Info("join review ignored: no email in the answers", append(logFields, "reason", "no email")...)
		return
	}
	logFields = append(logFields, "email", MaskEmail(email))

	found, err := s.dir.UserExists(ctx, email)
	if err != nil {
		s.log.Error("join review deferred: sub2api lookup failed",
			append(logFields, "error", err)...)
		return
	}
	if !found {
		s.log.Info("join review ignored: email not registered",
			append(logFields, "reason", "email not registered")...)
		return
	}

	if err := s.qq.ApproveJoin(ctx, ev); err != nil {
		s.log.Error("join approval failed", append(logFields, "action", "approve", "error", err)...)
		return
	}
	s.log.Info("join review", append(logFields, "action", "approve", "took", time.Since(start).String())...)
}

// EmailFromEvent reads the verification answers first, since the group now asks
// for the email through a question; the free-text verify message stays as a
// fallback for groups still using that verification mode.
func EmailFromEvent(ev *qqbot.JoinRequestEvent) (string, bool) {
	for _, qa := range ev.VerifyInfo.ReviewQAList {
		if email, ok := ExtractEmail(qa.Answer); ok {
			return email, true
		}
	}
	return ExtractEmail(ev.VerifyInfo.VerifyMessage)
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
