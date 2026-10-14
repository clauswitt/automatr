package todoist

import (
	"strings"
	"testing"
	"time"
)

func TestKeyNormalizesReminderSubjects(t *testing.T) {
	a := Key("noreply-gt-kundeportal@dk.gt.com", "Påmindelse: Manglende oplysninger påkrævet for at færdiggøre din sag")
	b := Key("noreply-gt-kundeportal@dk.gt.com", "Re: Påmindelse: Manglende oplysninger påkrævet for at færdiggøre din sag")
	if a != b {
		t.Fatalf("expected same key, got %s and %s", a, b)
	}
}

func TestInferDueDateUsesExplicitFutureDate(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	got := InferDueDate(now, "Data access renewal for Fliva dev ApS is due by Dec 11, 2026", "", 48*time.Hour)
	if got != "2026-12-11" {
		t.Fatalf("due date = %s, want 2026-12-11", got)
	}
}

func TestAppendEmailLinkDoesNotDuplicateLink(t *testing.T) {
	task := EmailTask{
		Key:        "abc",
		Subject:    "Grant Thornton reminder",
		From:       "noreply-gt-kundeportal@dk.gt.com",
		ReceivedAt: time.Date(2026, 10, 14, 6, 30, 0, 0, time.UTC),
		WebLink:    "https://outlook.office.com/mail/abc",
	}
	desc := newDescription(marker(task.Key), task)
	again := appendEmailLink(desc, task)
	if desc != again {
		t.Fatalf("duplicate link appended:\n%s", again)
	}
	if !strings.Contains(desc, task.WebLink) {
		t.Fatalf("description missing link: %s", desc)
	}
}
