package classify

import (
	"testing"
	"time"
)

func TestArchivePolicyDecide(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	policy := ArchivePolicy{
		ArchiveBelow:   0.15,
		TimeBoundAbove: 0.5,
		AuthHold:       time.Hour,
		CompanyDomains: []string{"fliva.com"},
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
		{"unread fresh OTP keeps even if needs_me high", &Triage{NeedsMe: 0.8, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", ReceivedAt: recent}, false},
		{"read OTP archives", &Triage{NeedsMe: 0.8, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", IsRead: true, ReceivedAt: recent}, true},
		{"expired unread OTP archives", &Triage{NeedsMe: 0.8, TimeBound: 0.97}, MessageState{From: "no-reply@github.com", ReceivedAt: now.Add(-2 * time.Hour)}, true},
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
