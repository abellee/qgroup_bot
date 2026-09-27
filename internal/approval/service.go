package approval

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"qgroup-bot/internal/qqbot"
)

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// Applicants answer the group question with their QQ number, whose registered
// address is the same digits at @qq.com.
var qqNumberPattern = regexp.MustCompile(`^[0-9]+$`)

type Directory interface {
	UserExists(ctx context.Context, email string) (bool, error)
}

// Service decides the fate of one join request: the answer given to the group's
// verification question must be the email of a registered sub2api account.
//
// Approving is the only judgement made. Every other outcome - no email in the
// answers, an unregistered email, a lookup that fails - leaves the request
// pending for a human, and nothing is ever auto-rejected. A successful approval
// then posts the configured welcome note to the group.
type Service struct {
	qq      *qqbot.Client
	dir     Directory
	groups  map[string]struct{}
	welcome string
	log     *slog.Logger
}

// NewService wires the approver. An empty welcome markdown leaves the group
// announcement disabled, which is the safe default when the copy is not set.
func NewService(qq *qqbot.Client, dir Directory, groups map[string]struct{}, welcome string, log *slog.Logger) *Service {
	return &Service{qq: qq, dir: dir, groups: groups, welcome: welcome, log: log}
}

func (s *Service) HandleJoin(ctx context.Context, ev *qqbot.JoinRequestEvent) {
	// Logged before any filtering so a request from an unmanaged group is still
	// readable in full: that is how the applicant's text and the group openID
	// are recovered during bootstrap.
	s.log.Info("join request received",
		"group_openid", ev.GroupOpenID,
		"event_id", ev.EventID,
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

	// The applicant is already in the group, so a failed announcement is only
	// worth logging; nothing about the approval changes.
	if s.welcome != "" {
		if err := s.qq.SendGroupMarkdown(ctx, ev.GroupOpenID, ev.EventID, s.welcome); err != nil {
			s.log.Error("welcome message send failed",
				append(logFields, "event_id", ev.EventID, "error", err)...)
			return
		}
		s.log.Info("welcome message sent", append(logFields, "event_id", ev.EventID)...)
	}
}

// EmailFromEvent reads the verification answers first, since the group now asks
// for the email through a question; the free-text verify message stays as a
// fallback for groups still using that verification mode.
func EmailFromEvent(ev *qqbot.JoinRequestEvent) (string, bool) {
	for _, qa := range ev.VerifyInfo.ReviewQAList {
		if email, ok := EmailFromAnswer(qa.Answer); ok {
			return email, true
		}
	}
	return EmailFromAnswer(ev.VerifyInfo.VerifyMessage)
}

// EmailFromAnswer turns one piece of applicant text into an address: the first
// email-looking token wins, and a bare QQ number stands for its @qq.com
// address. A number mixed into prose is not a QQ number answer, so only text
// that is nothing but digits gets the suffix.
//
// Whitespace is collapsed only after the text is read as typed, otherwise
// "my email is a@b.co" would glue its words into one long local part.
func EmailFromAnswer(text string) (string, bool) {
	if email, ok := ExtractEmail(text); ok {
		return email, true
	}
	collapsed := dropSpace(text)
	if email, ok := ExtractEmail(collapsed); ok {
		return email, true
	}
	if qqNumberPattern.MatchString(collapsed) {
		return collapsed + "@qq.com", true
	}
	return "", false
}

// dropSpace removes every space character, including the full-width and
// no-break ones an IME or a paste leaves inside an address.
func dropSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
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
