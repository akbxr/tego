package llm

import "strings"

// Anthropic cache diagnostics: a request names the session's previous
// response (diagnostics.previous_message_id) and the API answers, on
// message_start, with the first part of the prompt that differs from that
// request's (model, system, tools or messages) and the input it put at the
// miss. The OAuth path has always sent the field, as Claude Code does;
// the response was not read.

// anthropicAPIBase is the Claude API's address, the default base.
const anthropicAPIBase = "https://api.anthropic.com"

// diagnosticsBase is the API-key base that takes diagnostics (tests: a
// local server).
var diagnosticsBase = anthropicAPIBase

// diagnoses reports whether requests ask for cache diagnostics: only the
// Claude API takes the field, and other Anthropic-compatible endpoints may
// refuse it.
func (p *anthropicProvider) diagnoses() bool {
	return p.oauth != nil || strings.TrimRight(p.base, "/") == diagnosticsBase
}

// diagnosedClass: the classes whose responses the next request of the
// session is compared with. A compaction, title or keep-alive in between
// would have it compared with a prompt it does not repeat.
func diagnosedClass(class string) bool {
	return class == "" || class == ClassMain || class == ClassSubagent
}

// prevMessage is the session's last compared response ("" = none yet).
func (p *anthropicProvider) prevMessage(sid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.sessions[sid]; s != nil {
		return s.prevMsg
	}
	return ""
}

// notePrevMessage records a response the session's next request is
// compared with (the API-key path; OAuth's finish does it).
func (p *anthropicProvider) notePrevMessage(sid, class, id string) {
	if id == "" || !diagnosedClass(class) || !p.diagnoses() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[string]*ccSession{}
	}
	s := p.sessions[sid]
	if s == nil {
		s = &ccSession{}
		p.sessions[sid] = s
	}
	s.prevMsg = id
}

// aDiagResult is a response's diagnostics: no cache_miss_reason (or a
// null one, the comparison still running) when nothing is known to differ.
type aDiagResult struct {
	CacheMissReason *struct {
		Type   string `json:"type"`
		Tokens int    `json:"cache_missed_input_tokens"`
	} `json:"cache_miss_reason"`
}

func (d *aDiagResult) miss() *CacheMiss {
	if d == nil || d.CacheMissReason == nil || d.CacheMissReason.Type == "" {
		return nil
	}
	return &CacheMiss{Reason: d.CacheMissReason.Type, Tokens: d.CacheMissReason.Tokens}
}
