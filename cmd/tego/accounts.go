package main

import (
	"fmt"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// accountsCommand lists logged-in accounts per provider and, with an
// argument, sets how several accounts are used (saved to config.json).
func (a *app) accountsCommand(args string) {
	switch args {
	case llm.StrategyFallback, llm.StrategyRoundRobin:
		a.cfg.Accounts = args
		if err := a.cfg.Save(); err != nil {
			a.ui.Errorf("saving config: %v", err)
		}
		if a.ag.Provider != nil {
			if err := a.setModel(a.ag.ProviderName + "/" + a.ag.Model); err != nil {
				a.ui.Errorf("%v", err)
			}
		}
		a.ui.Dim("accounts → %s", args)
		a.refreshStatus()
		return
	case "":
	default:
		a.ui.Errorf("usage: /accounts [fallback|round-robin]")
		return
	}
	c := a.ui.c
	mode := a.cfg.Accounts
	if mode != llm.StrategyRoundRobin {
		mode = llm.StrategyFallback
	}
	fmt.Fprintf(a.out, "mode: %s%s%s  (fallback = stay on one account, switch when it hits a limit · round-robin = next account for each new prompt)\n", c.bold, mode, c.reset)
	var live []llm.AccountStatus
	if pool, ok := a.ag.Provider.(*llm.Pool); ok {
		live = pool.Status()
	}
	any := false
	for _, provider := range a.store.Providers() {
		accts := a.store.Accounts(provider)
		any = true
		fmt.Fprintf(a.out, "%s%s%s (%d)\n", c.bold, provider, c.reset, len(accts))
		for _, acct := range accts {
			line := "  " + acct.Label() + c.dim + " · " + acct.Type + c.reset
			if provider == a.ag.ProviderName {
				for _, st := range live {
					if st.Label != acct.Label() {
						continue
					}
					if st.Active {
						line += " " + c.green + "● in use" + c.reset
					}
					if wait := time.Until(st.CoolUntil); wait > 0 {
						line += fmt.Sprintf(" %s⏸ limited, %s left: %s%s", c.yellow, wait.Round(time.Second), st.Reason, c.reset)
					}
				}
			}
			fmt.Fprintln(a.out, line)
		}
	}
	if !any {
		fmt.Fprintln(a.out, "no stored accounts (use /login)")
	}
}
