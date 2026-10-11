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
	// AuthHold: how long an unread OTP stays in the inbox before it counts as expired.
	AuthHold time.Duration
	// CompanyDomains: mail from these sender domains is never archived.
	CompanyDomains []string
}

// MessageState is the live Outlook state the policy needs alongside triage.
type MessageState struct {
	From       string
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

// Decide applies the hard "never archive" rules first, then holds OTPs until
// they are read or expired, and finally archives only what Jev is confident
// does not need the owner.
func (p ArchivePolicy) Decide(t *Triage, m MessageState, now time.Time) ArchiveDecision {
	if t == nil {
		return keep("no triage")
	}
	if m.Flagged {
		return keep("flagged")
	}
	if strings.EqualFold(m.Importance, "high") {
		return keep("high importance")
	}
	if domain := extractDomain(m.From); domain != "" {
		for _, d := range p.CompanyDomains {
			if strings.EqualFold(domain, d) {
				return keep("company domain")
			}
		}
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

	if t.NeedsMe < p.ArchiveBelow {
		return archive(fmt.Sprintf("needs_me %.2f", t.NeedsMe))
	}
	return keep(fmt.Sprintf("needs_me %.2f", t.NeedsMe))
}
