package main

import (
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/lsp"
)

// newLSP is the language servers that check edits in cwd, or nil when
// "lsp" is false. A bad lsp_servers entry is returned as a note and the
// built-in servers are used.
func newLSP(cfg *config.Config, cwd string, onFail func(server, reason string)) (*lsp.Manager, string) {
	if !cfg.LSPEnabled() {
		return nil, ""
	}
	specs, err := lsp.Specs(cfg.LSPServers)
	note := ""
	if err != nil {
		specs, note = lsp.Builtin, "config: "+err.Error()+"; using the built-in language servers"
	}
	m := lsp.NewManager(cwd, specs)
	m.OnFail = onFail
	return m, note
}
