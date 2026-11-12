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

// Directory looks up an exact email or username and reports account status and activity.
type Directory interface {
	FindUser(ctx context.Context, identifier string, byEmail bool) (exists, active, activity bool, err error)
}

// WelcomeSink remembers members whose approval just landed, so the group
// greeting can fire on their first message. It is optional: a nil sink means
// approvals happen without any welcome bookkeeping.
type WelcomeSink interface {
	Remember(memberOpenID, groupOpenID string)
}

// Service approves only active sub2api accounts with non-announcement activity
// since registration, identified by email or username.
//
// Every other outcome leaves the request pending for a human, and nothing is
// ever auto-rejected.
type Service struct {
	qq      *qqbot.Client
	dir     Directory
	groups  map[string]struct{}
	welcome WelcomeSink
	log     *slog.Logger
}

// NewService wires the approver. A nil welcome sink leaves the greeting
// bookkeeping out, which is the default when no welcome copy is configured.
func NewService(qq *qqbot.Client, dir Directory, groups map[string]struct{}, welcome WelcomeSink, log *slog.Logger) *Service {
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

	identifier, byEmail, ok := IdentifierFromEvent(ev)
	if !ok {
		s.log.Info("join review left to manual: no email or username in the answers", append(logFields, "reason", "no identifier")...)
		return
	}
	lookupType := "username"
	if byEmail {
		lookupType = "email"
		logFields = append(logFields, "email", MaskEmail(identifier))
	} else {
		logFields = append(logFields, "identifier_type", lookupType)
	}

	found, active, activity, err := s.dir.FindUser(ctx, identifier, byEmail)
	if err != nil {
		s.log.Error("join review deferred: sub2api lookup failed",
			append(logFields, "error", err)...)
		return
	}
	if !found {
		s.log.Info("join review left to manual: account not found",
			append(logFields, "reason", "account not found", "lookup_type", lookupType)...)
		return
	}
	if !active {
		s.log.Info("join review left to manual: user is not active",
			append(logFields, "reason", "user not active", "lookup_type", lookupType)...)
		return
	}
	if !activity {
		s.log.Info("join review left to manual: user has no non-announcement activity since registration",
			append(logFields, "reason", "no non-announcement activity since registration", "lookup_type", lookupType)...)
		return
	}

	if err := s.qq.ApproveJoin(ctx, ev); err != nil {
		s.log.Error("join approval failed", append(logFields, "action", "approve", "error", err)...)
		return
	}
	// The platform refuses a reply to a join event and refuses active
	// messages, so the greeting cannot leave now; the sink remembers the
	// member and the group greeting fires on their first own message.
	if s.welcome != nil {
		s.welcome.Remember(ev.MemberOpenID, ev.GroupOpenID)
	}
	s.log.Info("join review", append(logFields, "action", "approve", "took", time.Since(start).String())...)
}

// IdentifierFromEvent prefers an email wherever it appears, then treats the
// first non-empty answer as a username. The free-text field remains supported.
func IdentifierFromEvent(ev *qqbot.JoinRequestEvent) (string, bool, bool) {
	for _, qa := range ev.VerifyInfo.ReviewQAList {
		if email, ok := EmailFromAnswer(qa.Answer); ok {
			return email, true, true
		}
	}
	if email, ok := EmailFromAnswer(ev.VerifyInfo.VerifyMessage); ok {
		return email, true, true
	}
	for _, qa := range ev.VerifyInfo.ReviewQAList {
		if username := strings.TrimSpace(qa.Answer); username != "" {
			return username, false, true
		}
	}
	username := strings.TrimSpace(ev.VerifyInfo.VerifyMessage)
	return username, false, username != ""
}

// EmailFromAnswer accepts an email embedded in prose and tolerates spaces
// accidentally inserted inside the address. Other answers remain usernames.
func EmailFromAnswer(text string) (string, bool) {
	if email, ok := ExtractEmail(text); ok {
		return email, true
	}
	collapsed := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
	return ExtractEmail(collapsed)
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
