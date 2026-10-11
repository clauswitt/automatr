package summarize

import (
	"strings"
	"testing"
	"time"
)

func TestWriteMarkdownSectionsSplitsFiledAway(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	digest := &DigestSummary{Categories: map[string][]EmailSummary{
		"Clients": {
			{Subject: "Re: proposal", From: "anna@client.dk", Category: "Clients", Summary: "Anna wants the timeline by Thursday", WebLink: "l1", ReceivedAt: now},
		},
		"Marketing": {
			{Subject: "50% off", From: "news@shop.com", Category: "Marketing", Summary: "**50% off** - news@shop.com", WebLink: "l2", ReceivedAt: now, Archived: true},
		},
		"Product Updates": {
			{Subject: "Changelog", From: "noreply@github.com", Category: "Product Updates", Summary: "should not be shown", WebLink: "l3", ReceivedAt: now.Add(time.Hour), Archived: true},
		},
	}}

	var b strings.Builder
	WriteMarkdownSections(&b, digest, []string{"Clients", "Marketing", "Product Updates"})
	out := b.String()

	for _, want := range []string{
		"## Clients (1)\n\n- Anna wants the timeline by Thursday [↗](l1)",
		"## Filed away (2)",
		"- Product Updates · noreply@github.com — Changelog [↗](l3)\n- Marketing · news@shop.com — 50% off [↗](l2)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"## Marketing", "## Product Updates", "should not be shown"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %q in:\n%s", unwanted, out)
		}
	}
}

func TestOrderedCategoriesIncludesUnlisted(t *testing.T) {
	emails := map[string][]EmailSummary{"Needs Review": {{}}, "Clients": {{}}, "Zeta": {{}}}
	got := strings.Join(OrderedCategories([]string{"Clients", "Marketing"}, emails), ",")
	if want := "Needs Review,Clients,Marketing,Zeta"; got != want {
		t.Errorf("OrderedCategories = %s, want %s", got, want)
	}
}
