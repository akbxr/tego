package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/lineedit"
)

// pendingLogin is a browser OAuth flow running in the background. The
// callback server usually completes it; otherwise the user pastes the
// redirect URL with /login <url>, which is fed to the flow through w.
type pendingLogin struct {
	provider string
	w        *io.PipeWriter
	cancel   context.CancelFunc
}

type loginResult struct {
	provider string
	err      error
}

var oauthLabels = map[string]string{
	"anthropic":          "Claude Pro/Max · browser login",
	"openai-codex":       "ChatGPT Plus/Pro (Codex) · browser login",
	"google-antigravity": "Google account: Gemini & Claude · browser login",
}

// loginCommand handles /login [provider | redirect-URL | cancel].
func (a *app) loginCommand(args string) {
	if p := a.login; p != nil {
		switch {
		case args == "":
			a.ui.Dim("login to %s in progress: finish it in the browser, or paste the redirect URL with /login <url> (/login cancel to stop)", p.provider)
		case args == "cancel":
			p.cancel()
		default:
			line := args + "\n"
			go func() { _, _ = io.WriteString(p.w, line) }()
		}
		return
	}
	provider := args
	if provider == "" {
		var ok bool
		if provider, ok = a.pickProvider("Log in · subscriptions open the browser, other providers ask for an API key", false); !ok {
			return
		}
	}
	if !slices.Contains(a.cfg.ProviderNames(), provider) {
		a.ui.Errorf("unknown provider %q", provider)
		return
	}
	if slices.Contains(config.OAuthProviders, provider) {
		a.startOAuth(provider)
		return
	}
	// API key: read it on the real terminal without echo, outside the TUI.
	resume := a.tui.Suspend()
	err := storeKey(a.store, provider)
	resume()
	if err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.afterLogin(provider)
}

func (a *app) startOAuth(provider string) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	a.login = &pendingLogin{provider: provider, w: pw, cancel: cancel}
	done := a.loginDone
	go func() {
		err := auth.Login(ctx, a.store, provider, pr, tuiLoginOut{a.out})
		_ = pr.Close()
		done <- loginResult{provider: provider, err: err}
	}()
}

// tuiLoginOut adapts the login instructions to the TUI, where pasted input
// goes through /login <url> rather than stdin.
type tuiLoginOut struct{ w io.Writer }

func (o tuiLoginOut) Write(p []byte) (int, error) {
	s := strings.Replace(string(p), "paste the final redirect URL or code here:",
		"paste the final redirect URL or code with /login <url> (/login cancel stops).", 1)
	_, err := io.WriteString(o.w, s)
	return len(p), err
}

// finishLogin runs on the event loop when a background OAuth flow ends.
func (a *app) finishLogin(res loginResult) {
	a.login = nil
	switch {
	case errors.Is(res.err, context.Canceled):
		a.ui.Dim("login to %s cancelled", res.provider)
	case res.err != nil:
		a.ui.Errorf("login to %s failed: %v", res.provider, res.err)
	case a.cancel != nil:
		a.reloadProvider = res.provider // reported and applied when the running turn ends
	default:
		a.afterLogin(res.provider)
	}
}

// afterLogin makes new credentials take effect: rebuild the current
// provider if it is the one logged into, or pick a model if there is none.
func (a *app) afterLogin(provider string) {
	a.ui.Dim("logged in to %s", provider)
	switch {
	case a.ag.Provider == nil:
		spec, err := a.cfg.DefaultModel(a.store)
		if err != nil {
			a.ui.Errorf("%v", err)
			return
		}
		a.switchModel(spec)
	case a.ag.ProviderName == provider:
		if err := a.setModel(provider + "/" + a.ag.Model); err != nil {
			a.ui.Errorf("%v", err)
		}
	default:
		a.ui.Dim("switch to it with /model")
	}
	a.refreshStatus()
}

// logoutCommand removes stored credentials: /logout [provider [all]]. With
// several accounts it asks which one (or all) to remove.
func (a *app) logoutCommand(args string) {
	provider, rest, _ := strings.Cut(args, " ")
	if provider == "" {
		var ok bool
		if provider, ok = a.pickProvider("Log out · remove stored credentials", true); !ok {
			return
		}
	}
	accts := a.store.Accounts(provider)
	if len(accts) == 0 {
		a.ui.Errorf("no stored credentials for %s", provider)
		return
	}
	all := strings.TrimSpace(rest) == "all" || len(accts) == 1
	var drop auth.Credential
	if !all {
		items := []lineedit.PickItem{{Label: "all accounts", Detail: fmt.Sprintf("remove all %d", len(accts))}}
		for _, c := range accts {
			items = append(items, lineedit.PickItem{Label: c.Label(), Detail: c.Type})
		}
		idx, err := a.tui.Pick("Log out of "+provider+" · which account?", items, "", 1)
		if err != nil {
			return
		}
		if idx == 0 {
			all = true
		} else {
			drop = accts[idx-1]
		}
	}
	var err error
	if all {
		err = a.store.Delete(provider)
	} else {
		err = a.store.DeleteAccount(provider, drop.ID)
	}
	if err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	if all {
		a.ui.Dim("logged out of %s", provider)
	} else {
		a.ui.Dim("removed %s from %s (%d account(s) left)", drop.Label(), provider, len(accts)-1)
	}
	if a.ag.ProviderName == provider {
		if left := a.store.Accounts(provider); len(left) > 0 {
			if err := a.setModel(provider + "/" + a.ag.Model); err != nil {
				a.ui.Errorf("%v", err)
			}
		} else {
			a.ui.Dim("the current model used %s; pick another with /model (environment keys still work)", provider)
		}
		a.refreshStatus()
	}
}

// pickProvider lists subscription logins first, then API-key providers;
// stored=true lists only providers with saved credentials.
func (a *app) pickProvider(title string, stored bool) (string, bool) {
	var names []string
	var items []lineedit.PickItem
	add := func(name, desc string) {
		accts := a.store.Accounts(name)
		if stored && len(accts) == 0 {
			return
		}
		switch {
		case len(accts) > 1:
			desc = fmt.Sprintf("✓ %d accounts · %s", len(accts), desc)
		case len(accts) == 1 && accts[0].Type == "oauth":
			desc = "✓ " + accts[0].Label() + " · " + desc
		case len(accts) == 1:
			desc = "✓ API key saved · " + desc
		}
		names = append(names, name)
		items = append(items, lineedit.PickItem{Label: name, Detail: desc})
	}
	for _, p := range config.OAuthProviders {
		desc := oauthLabels[p]
		if desc == "" {
			desc = "browser login"
		}
		add(p, desc)
	}
	for _, p := range a.cfg.ProviderNames() {
		if slices.Contains(config.OAuthProviders, p) || p == "ollama" {
			continue
		}
		add(p, "API key")
	}
	if len(items) == 0 {
		a.ui.Dim("no stored credentials")
		return "", false
	}
	idx, err := a.tui.Pick(title, items, "", 0)
	if err != nil {
		if !errors.Is(err, lineedit.ErrCanceled) {
			a.ui.Errorf("%v", err)
		}
		return "", false
	}
	return names[idx], true
}
