package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

func TestCapacityLine(t *testing.T) {
	acct := func(limits ...llm.UsageLimit) accountUsage {
		return accountUsage{report: &llm.UsageReport{Limits: limits}}
	}
	pu := providerUsage{accounts: []accountUsage{
		// Each account counts its most-used limit per window.
		acct(llm.UsageLimit{Window: "7d", Used: 1}, llm.UsageLimit{Window: "7d", Used: 0.01}),
		acct(llm.UsageLimit{Window: "7d", Used: 0.5}, llm.UsageLimit{Window: "5h", Used: 0.25}),
		// Unreported limits and failed accounts are left out.
		acct(llm.UsageLimit{Window: "5h", Used: -1}, llm.UsageLimit{Window: "7d", Tier: "Fable", Used: 0}),
		{err: errors.New("401")},
	}}
	want := "5h → 0.25/1 account used (0.75× quota left) · 7d → 1.50/2 accounts used (0.50× quota left) · 7d (Fable) → 0.00/1 account used (1.00× quota left)"
	if got := capacityLine(pu); got != want {
		t.Fatalf("capacityLine =\n %s\nwant\n %s", got, want)
	}
}

func TestRenderUsageAge(t *testing.T) {
	r := &llm.UsageReport{Limits: []llm.UsageLimit{{Label: "Claude 5 Hour", Window: "5h", Used: 0.5}}}
	pu := providerUsage{name: "anthropic", title: "Anthropic", accounts: []accountUsage{
		{label: "a@x", report: r, at: time.Now().Add(-3 * time.Minute), from: "answer",
			note: "usage endpoint: HTTP 429: Rate limited. Please try again later."},
		{label: "b@x", report: r, at: time.Now(), from: "endpoint"},
		{label: "c@x", report: r, at: time.Now().Add(-2 * time.Hour), from: "endpoint"},
		{label: "d@x", err: errors.New("usage endpoint: HTTP 429: Rate limited. Please try again later. (asked again after 10:15)")},
	}}
	var b strings.Builder
	renderUsage(&b, palette{}, []providerUsage{pu}, time.Second, nil)
	out := b.String()
	for _, want := range []string{
		"a@x · from the answers, 3m ago · usage endpoint: HTTP 429: Rate limited. Please try again later.\n",
		"b@x\n",
		"c@x · 2h ago\n",
		"d@x · usage endpoint: HTTP 429: Rate limited. Please try again later. (asked again after 10:15)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}
