package main

import (
	"errors"
	"testing"

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
