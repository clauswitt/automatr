package classify

import (
	"testing"
	"time"
)

func TestArchivePolicyDecide(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	policy := ArchivePolicy{
		ArchiveBelow:      0.15,
		TimeBoundAbove:    0.5,
		ColdOutreachAbove: 0.85,
		AuthHold:          time.Hour,
		CompanyDomains:    []string{"fliva.com"},
		AlwaysKeep:        []SenderRule{{From: "@dk.gt.com"}},
		AlwaysArchive:     []SenderRule{{From: "it-support@arnsbogroup.com", Subject: "quarantine notification"}},
	}
	recent := now.Add(-5 * time.Minute)

	tests := []struct {
		name    string
		triage  *Triage
		state   MessageState
		archive bool
	}{
		{"no triage keeps", nil, MessageState{From: "news@shop.com", ReceivedAt: recent}, false},
		{"newsletter archives", &Triage{NeedsMe: 0.02}, MessageState{From: "news@shop.com", ReceivedAt: recent}, true},
		{"personal mail keeps", &Triage{NeedsMe: 0.9}, MessageState{From: "anna@client.com", ReceivedAt: recent}, false},
		{"uncertain keeps", &Triage{NeedsMe: 0.3}, MessageState{From: "info@vendor.com", ReceivedAt: recent}, false},
		{"flagged keeps", &Triage{NeedsMe: 0.01}, MessageState{From: "news@shop.com", Flagged: true, ReceivedAt: recent}, false},
		{"high importance keeps", &Triage{NeedsMe: 0.01}, MessageState{From: "news@shop.com", Importance: "high", ReceivedAt: recent}, false},
		{"company domain keeps", &Triage{NeedsMe: 0.01}, MessageState{From: "bot@Fliva.com", ReceivedAt: recent}, false},
		{"unread fresh OTP keeps", &Triage{NeedsMe: 0.05, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", ReceivedAt: recent}, false},
		{"unread fresh company OTP keeps", &Triage{NeedsMe: 0.3, TimeBound: 0.99}, MessageState{From: "support@fliva.com", Subject: "Your Fliva login code", ReceivedAt: recent}, false},
		{"unread fresh OTP keeps even if needs_me high", &Triage{NeedsMe: 0.8, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", ReceivedAt: recent}, false},
		{"read OTP archives", &Triage{NeedsMe: 0.8, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", IsRead: true, ReceivedAt: recent}, true},
		{"read company OTP archives", &Triage{NeedsMe: 0.3, TimeBound: 0.99}, MessageState{From: "support@fliva.com", Subject: "Your Fliva login code", IsRead: true, ReceivedAt: recent}, true},
		{"cold pitch archives even if needs_me high", &Triage{NeedsMe: 0.9, ColdOutreach: 0.95}, MessageState{From: "mike@growthleaf.de", ReceivedAt: recent}, true},
		{"warm intro keeps", &Triage{NeedsMe: 0.8, ColdOutreach: 0.35}, MessageState{From: "sofie@partner.dk", ReceivedAt: recent}, false},
		{"flagged cold pitch keeps", &Triage{NeedsMe: 0.9, ColdOutreach: 0.95}, MessageState{From: "mike@growthleaf.de", Flagged: true, ReceivedAt: recent}, false},
		{"always_keep domain beats low needs_me", &Triage{NeedsMe: 0.01}, MessageState{From: "noreply-gt-kundeportal@dk.gt.com", ReceivedAt: recent}, false},
		{"always_archive works without triage", nil, MessageState{From: "IT-Support@arnsbogroup.com", Subject: "Quarantine Notification", ReceivedAt: recent}, true},
		{"always_archive subject must match", &Triage{NeedsMe: 0.9}, MessageState{From: "it-support@arnsbogroup.com", Subject: "Laptop replacement", ReceivedAt: recent}, false},
		{"flag beats always_archive", nil, MessageState{From: "it-support@arnsbogroup.com", Subject: "Quarantine Notification", Flagged: true, ReceivedAt: recent}, false},
		{"expired unread OTP archives", &Triage{NeedsMe: 0.8, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", ReceivedAt: now.Add(-2 * time.Hour)}, true},
		{"expired unread company OTP archives", &Triage{NeedsMe: 0.3, TimeBound: 0.99}, MessageState{From: "support@fliva.com", Subject: "Your Fliva login code", ReceivedAt: now.Add(-2 * time.Hour)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.Decide(tt.triage, tt.state, now)
			if got.Archive != tt.archive {
				t.Errorf("Decide() archive = %v (%s), want %v", got.Archive, got.Reason, tt.archive)
			}
		})
	}
}
