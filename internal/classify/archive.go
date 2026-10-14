package classify

import (
	"fmt"
	"strings"
	"time"
)

// ArchivePolicy decides whether an inbox message can be filed away. The goal is
// an inbox that only holds mail the owner has to deal with personally.
type ArchivePolicy struct {
	// ArchiveBelow: archive when P(needs me) is below this.
	ArchiveBelow float64
	// TimeBoundAbove: treat as an OTP / magic link when P(time bound) is above this.
	TimeBoundAbove float64
	// ColdOutreachAbove: archive unsolicited pitches when P(cold outreach) is above this.
	ColdOutreachAbove float64
	// AuthHold: how long an unread OTP stays in the inbox before it counts as expired.
	AuthHold time.Duration
	// CompanyDomains: mail from these sender domains is never archived.
	CompanyDomains []string
	// AlwaysKeep / AlwaysArchive: explicit sender rules that override Jev.
	AlwaysKeep    []SenderRule
	AlwaysArchive []SenderRule
}

// SenderRule matches a message by sender and, optionally, subject.
// From is a full address ("it@corp.com") or a domain ("@corp.com", which
// also matches subdomains). Subject is a case-insensitive substring.
type SenderRule struct {
	From    string `yaml:"from"`
	Subject string `yaml:"subject"`
}

func (r SenderRule) Matches(from, subject string) bool {
	from, pattern := strings.ToLower(from), strings.ToLower(r.From)
	switch {
	case pattern == "":
		return false
	case strings.HasPrefix(pattern, "@"):
		if !strings.HasSuffix(from, pattern) && !strings.HasSuffix(from, "."+pattern[1:]) {
			return false
		}
	case from != pattern:
		return false
	}
	return r.Subject == "" || strings.Contains(strings.ToLower(subject), strings.ToLower(r.Subject))
}

func matchesAny(rules []SenderRule, from, subject string) (SenderRule, bool) {
	for _, r := range rules {
		if r.Matches(from, subject) {
			return r, true
		}
	}
	return SenderRule{}, false
}

// MessageState is the live Outlook state the policy needs alongside triage.
type MessageState struct {
	From       string
	Subject    string
	IsRead     bool
	Flagged    bool
	Importance string
	ReceivedAt time.Time
}

type ArchiveDecision struct {
	Archive bool
	Reason  string
}

func keep(reason string) ArchiveDecision    { return ArchiveDecision{Archive: false, Reason: reason} }
func archive(reason string) ArchiveDecision { return ArchiveDecision{Archive: true, Reason: reason} }

// Decide applies flags and explicit sender rules first, holds OTPs until they
// are read or expired, then applies the broad company-domain keep rule. This
// lets short-lived login codes from company domains age out without filing
// ordinary company mail away.
func (p ArchivePolicy) Decide(t *Triage, m MessageState, now time.Time) ArchiveDecision {
	if m.Flagged {
		return keep("flagged")
	}
	if strings.EqualFold(m.Importance, "high") {
		return keep("high importance")
	}
	if r, ok := matchesAny(p.AlwaysKeep, m.From, m.Subject); ok {
		return keep("always_keep " + r.From)
	}
	if r, ok := matchesAny(p.AlwaysArchive, m.From, m.Subject); ok {
		return archive("always_archive " + r.From)
	}
	if t == nil {
		return keep("no triage")
	}

	if t.TimeBound > p.TimeBoundAbove {
		if m.IsRead {
			return archive(fmt.Sprintf("one-time auth email, read (time_bound %.2f)", t.TimeBound))
		}
		if age := now.Sub(m.ReceivedAt); age >= p.AuthHold {
			return archive(fmt.Sprintf("one-time auth email, unread for %s (time_bound %.2f)", age.Round(time.Minute), t.TimeBound))
		}
		return keep(fmt.Sprintf("one-time auth email waiting to be used (time_bound %.2f)", t.TimeBound))
	}

	if domain := extractDomain(m.From); domain != "" {
		for _, d := range p.CompanyDomains {
			if strings.EqualFold(domain, d) {
				return keep("company domain")
			}
		}
	}

	if t.ColdOutreach > p.ColdOutreachAbove {
		return archive(fmt.Sprintf("cold outreach %.2f", t.ColdOutreach))
	}

	if t.NeedsMe < p.ArchiveBelow {
		return archive(fmt.Sprintf("needs_me %.2f", t.NeedsMe))
	}
	return keep(fmt.Sprintf("needs_me %.2f", t.NeedsMe))
}
