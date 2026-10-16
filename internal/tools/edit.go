package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

type editTool struct{ env *Env }

func (t *editTool) Def() llm.Tool {
	return llm.Tool{
		Name: "edit",
		Description: "Replace old_string with new_string in a file. old_string must match one location exactly " +
			"(include surrounding lines to make it unique) unless replace_all is set. " +
			"For several changes to one file, pass them all at once in edits (applied in order, each to the result of the previous; " +
			"if any fails, nothing is changed) instead of several calls or a shell script. " +
			"Copy text without read's line-number prefixes. A file you read with the read tool must not have changed since; " +
			"a file you did not read (e.g. only saw through grep) can be edited when each old_string matches it exactly once. " +
			"Otherwise nothing is changed and the error returns the current content, then retry. Use write to create files.",
		Schema: schema(`{"type":"object","properties":{` +
			`"path":{"type":"string"},` +
			`"old_string":{"type":"string","description":"Exact text to replace (one change)"},` +
			`"new_string":{"type":"string","description":"Replacement text (one change)"},` +
			`"replace_all":{"type":"boolean","description":"Replace every occurrence"},` +
			`"edits":{"type":"array","description":"Several changes to the file, instead of old_string/new_string","items":{"type":"object","properties":{` +
			`"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},` +
			`"required":["old_string","new_string"]}}},` +
			`"required":["path"]}`),
	}
}

// oneEdit is one replacement of an edit call.
type oneEdit struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// maxEditHunks bounds the diff lines shown per change of a multi-edit.
const maxEditHunks = 40

func (t *editTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
		oneEdit
		Edits []oneEdit `json:"edits"`
	}
	if err := decode(input, &in); err != nil {
		return "", err
	}
	if in.Path == "" {
		return "", errors.New("path is required")
	}
	edits := in.Edits
	switch {
	case len(edits) > 0 && (in.OldString != "" || in.NewString != ""):
		return "", errors.New("pass either old_string/new_string or edits, not both")
	case len(edits) == 0:
		edits = []oneEdit{in.oneEdit}
	}
	p := t.env.abs(in.Path)
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", in.Path)
	}
	data, err := t.env.readText(ctx, p)
	if err != nil {
		return "", err
	}
	// A file read in this session must not have changed since. One never
	// read (seen through grep or sed, as models often do) may be edited only
	// by exact, unique matches: they prove the text is there now, so nothing
	// stale is overwritten; anything looser could land in the wrong place.
	sum := sha256.Sum256(data)
	read := t.env.wasSeen(p)
	if read {
		if err := t.env.checkSeen(p, sum, "edit"); err != nil {
			return "", err
		}
	}
	// Every change is applied in memory first: one that fails leaves the
	// file untouched.
	content := string(data)
	cur, count := content, 0
	var hows, hunks []string
	per := max(maxEditHunks/len(edits), 6)
	for i, e := range edits {
		res, err := applyEdit(cur, e.OldString, e.NewString, e.ReplaceAll)
		if !read && (err != nil || res.how != "" || e.ReplaceAll) {
			what := "did not match it exactly once"
			if len(edits) > 1 {
				what = fmt.Sprintf("change %d of %d %s", i+1, len(edits), what)
			}
			return "", t.env.showFresh(p, sum, fmt.Sprintf("%s was not read in this session and %s", t.env.rel(p), what), "edit")
		}
		if err != nil {
			if len(edits) > 1 {
				return "", fmt.Errorf("%s: change %d of %d: %w (nothing was changed)", t.env.rel(p), i+1, len(edits), err)
			}
			return "", fmt.Errorf("%s: %w", t.env.rel(p), err)
		}
		hunks = append(hunks, diffSnippet(cur, res.content, per))
		if res.how != "" {
			hows = append(hows, res.how)
		}
		cur, count = res.content, count+res.count
	}
	if err := t.env.writeText(ctx, p, []byte(cur), st.Mode().Perm()); err != nil {
		return "", err
	}
	t.env.markSeen(p, sha256.Sum256([]byte(cur)))
	var b strings.Builder
	fmt.Fprintf(&b, "Edited %s: %d replacement", t.env.rel(p), count)
	if count != 1 {
		b.WriteByte('s')
	}
	if len(edits) > 1 {
		fmt.Fprintf(&b, " in %d changes", len(edits))
	}
	if len(hows) > 0 {
		fmt.Fprintf(&b, " (matched %s)", strings.Join(dedupe(hows), ", "))
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(hunks, "\n... next change\n"))
	return strings.TrimSuffix(b.String(), "\n") + t.env.diagnose(ctx, p), nil
}

// Preview is the diff an edit or write call would make, in the format of
// their results, without writing anything; "" when there is none to show
// (a new file, or a call that would fail). For a user asked to approve it.
func Preview(env *Env, name string, input json.RawMessage) string {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		oneEdit
		Edits []oneEdit `json:"edits"`
	}
	if decode(input, &in) != nil || in.Path == "" {
		return ""
	}
	data, err := os.ReadFile(env.abs(in.Path))
	if err != nil {
		return ""
	}
	switch name {
	case "write":
		return diffSnippet(string(data), in.Content, maxEditHunks)
	case "edit":
		edits := in.Edits
		if len(edits) == 0 {
			edits = []oneEdit{in.oneEdit}
		}
		cur, per := string(data), max(maxEditHunks/len(edits), 6)
		var hunks []string
		for _, e := range edits {
			res, err := applyEdit(cur, e.OldString, e.NewString, e.ReplaceAll)
			if err != nil {
				return ""
			}
			hunks = append(hunks, diffSnippet(cur, res.content, per))
			cur = res.content
		}
		return strings.Join(hunks, "\n... next change\n")
	}
	return ""
}

func dedupe(s []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

type editResult struct {
	content string
	count   int
	how     string // "" for exact; otherwise which fallback matched
}

// applyEdit replaces old with new in content. It tries an exact match, then
// line-ending-normalized, then per-line trailing-whitespace-insensitive, then
// per-line indentation-insensitive matching (re-indenting new). The file's
// line endings are preserved.
func applyEdit(content, old, new string, all bool) (editResult, error) {
	if old == "" {
		return editResult{}, errors.New("old_string is empty; use write to create or overwrite a file")
	}
	if old == new {
		return editResult{}, errors.New("old_string and new_string are identical")
	}
	eol := detectEOL(content)
	oldLF, newLF := toLF(old), toLF(new)
	newEOL := newLF
	if eol == "\r\n" {
		newEOL = strings.ReplaceAll(newLF, "\n", "\r\n")
	}

	// Stage 1+2: exact, then with old's line endings converted to LF / CRLF.
	type cand struct{ old, how string }
	cands := []cand{{old, ""}}
	if oldLF != old {
		cands = append(cands, cand{oldLF, "after normalizing line endings"})
	}
	if strings.Contains(content, "\r\n") {
		if c := strings.ReplaceAll(oldLF, "\n", "\r\n"); c != old {
			cands = append(cands, cand{c, "after normalizing line endings"})
		}
	}
	for _, c := range cands {
		n := strings.Count(content, c.old)
		if n == 0 {
			continue
		}
		if n > 1 && !all {
			return editResult{}, multiErr(n, offsetsToLines(content, c.old))
		}
		repl := newEOL
		if !all {
			n = 1
		}
		return editResult{strings.Replace(content, c.old, repl, n), n, c.how}, nil
	}

	// Stage 3+4: whole-line block matching.
	lines := splitLines(content)
	oldBody := strings.TrimSuffix(oldLF, "\n")
	newBody := newLF
	if oldBody != oldLF {
		newBody = strings.TrimSuffix(newLF, "\n")
	}
	oldLines := strings.Split(oldBody, "\n")
	modes := []struct {
		how string
		eq  func(f, o string) bool
	}{
		{"ignoring trailing whitespace", func(f, o string) bool {
			return strings.TrimRight(f, " \t") == strings.TrimRight(o, " \t")
		}},
		{"ignoring indentation", func(f, o string) bool {
			return strings.TrimSpace(f) == strings.TrimSpace(o)
		}},
	}
	for mi, m := range modes {
		matches := findBlocks(lines, oldLines, m.eq)
		if len(matches) == 0 {
			continue
		}
		if len(matches) > 1 && !all {
			ln := make([]int, len(matches))
			for i, s := range matches {
				ln[i] = s + 1
			}
			return editResult{}, multiErr(len(matches), ln)
		}
		out := content
		for k := len(matches) - 1; k >= 0; k-- {
			i := matches[k]
			first, last := lines[i], lines[i+len(oldLines)-1]
			body := newBody
			if mi == 1 {
				body = reindent(oldLines, lines[i:i+len(oldLines)], newBody)
			}
			le := eol
			if first.crlf {
				le = "\r\n"
			} else if first.hasNL {
				le = "\n"
			}
			if le == "\r\n" {
				body = strings.ReplaceAll(body, "\n", "\r\n")
			}
			out = out[:first.start] + body + out[last.end:]
		}
		return editResult{out, len(matches), m.how}, nil
	}
	return editResult{}, notFoundErr(lines, oldLines)
}

type lineSpan struct {
	start, end int // byte range of line text, excluding terminator
	text       string
	crlf       bool // terminated by \r\n
	hasNL      bool // terminated by \n or \r\n
}

func splitLines(s string) []lineSpan {
	var out []lineSpan
	start := 0
	for start <= len(s) {
		i := strings.IndexByte(s[start:], '\n')
		if i < 0 {
			if start < len(s) {
				out = append(out, lineSpan{start: start, end: len(s), text: s[start:]})
			}
			break
		}
		end := start + i
		ls := lineSpan{start: start, end: end, hasNL: true}
		if end > start && s[end-1] == '\r' {
			ls.end--
			ls.crlf = true
		}
		ls.text = s[start:ls.end]
		out = append(out, ls)
		start = end + 1
	}
	return out
}

func findBlocks(lines []lineSpan, old []string, eq func(f, o string) bool) []int {
	var out []int
	n := len(old)
	for i := 0; i+n <= len(lines); i++ {
		ok := true
		for j := range n {
			if !eq(lines[i+j].text, old[j]) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, i)
			i += n - 1
		}
	}
	return out
}

func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// reindent shifts new's indentation by the delta between old's first
// non-blank line and the corresponding file line.
func reindent(old []string, file []lineSpan, new string) string {
	var oi, fi string
	found := false
	for j, o := range old {
		if strings.TrimSpace(o) != "" {
			oi, fi = leadingWS(o), leadingWS(file[j].text)
			found = true
			break
		}
	}
	if !found || oi == fi {
		return new
	}
	nl := strings.Split(new, "\n")
	for k, l := range nl {
		if strings.TrimSpace(l) == "" {
			continue
		}
		switch {
		case strings.HasPrefix(l, oi):
			nl[k] = fi + l[len(oi):]
		case strings.HasPrefix(fi, oi): // file is more indented: add
			nl[k] = fi[len(oi):] + l
		case strings.HasPrefix(oi, fi): // file is less indented: remove
			rem := oi[len(fi):]
			if strings.HasPrefix(l, rem) {
				nl[k] = l[len(rem):]
			} else {
				nl[k] = strings.TrimLeft(l, " \t")
			}
		}
	}
	return strings.Join(nl, "\n")
}

func detectEOL(s string) string {
	crlf := strings.Count(s, "\r\n")
	if crlf > 0 && crlf*2 >= strings.Count(s, "\n") {
		return "\r\n"
	}
	return "\n"
}

func toLF(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func offsetsToLines(content, sub string) []int {
	var out []int
	line, prev := 1, 0
	for off := 0; ; {
		i := strings.Index(content[off:], sub)
		if i < 0 {
			break
		}
		pos := off + i
		line += strings.Count(content[prev:pos], "\n")
		prev = pos
		out = append(out, line)
		off = pos + len(sub)
	}
	return out
}

func multiErr(n int, lines []int) error {
	parts := make([]string, 0, len(lines))
	for i, l := range lines {
		if i == 20 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprint(l))
	}
	return fmt.Errorf("old_string matches %d locations (lines %s); add surrounding context to make it unique or set replace_all",
		n, strings.Join(parts, ", "))
}

// notFoundErr builds a no-match error pointing at the most similar region.
func notFoundErr(lines []lineSpan, old []string) error {
	msg := "old_string not found (also tried ignoring line endings, trailing whitespace and indentation). Re-read the file and copy the exact text."
	if len(lines) == 0 {
		return errors.New(msg)
	}
	n := len(old)
	k := n
	if k > 8 {
		k = 8
	}
	og := make([][]uint32, k)
	for j := range k {
		og[j] = bigrams(old[j])
	}
	fg := make([][]uint32, len(lines))
	gram := func(i int) []uint32 {
		if fg[i] == nil {
			fg[i] = bigrams(lines[i].text)
		}
		return fg[i]
	}
	best, bestScore := -1, 0.0
	windows := len(lines) - k + 1
	if windows < 1 {
		windows = 1
	}
	for i := range windows {
		var sum float64
		cnt := 0
		for j := 0; j < k && i+j < len(lines); j++ {
			sum += dice(gram(i+j), og[j])
			cnt++
		}
		if s := sum / float64(k); s > bestScore && cnt > 0 {
			best, bestScore = i, s
		}
	}
	if best < 0 || bestScore < 0.3 {
		return errors.New(msg)
	}
	show := n
	if show > 12 {
		show = 12
	}
	end := best + show
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nMost similar region (lines %d-%d, %.0f%% similar):\n", msg, best+1, end, bestScore*100)
	for i := best; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, truncRunes(lines[i].text, 200))
	}
	return errors.New(strings.TrimSuffix(b.String(), "\n"))
}

// bigrams returns the sorted unique rune bigrams of the trimmed line.
func bigrams(s string) []uint32 {
	r := []rune(strings.TrimSpace(s))
	if len(r) < 2 {
		if len(r) == 1 {
			return []uint32{uint32(r[0])}
		}
		return []uint32{}
	}
	out := make([]uint32, 0, len(r)-1)
	for i := 0; i+1 < len(r); i++ {
		out = append(out, uint32(r[i])*0x9E3779B1^uint32(r[i+1]))
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	u := out[:0]
	for i, v := range out {
		if i == 0 || v != out[i-1] {
			u = append(u, v)
		}
	}
	return u
}

func dice(a, b []uint32) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	i, j, inter := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			inter++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return 2 * float64(inter) / float64(len(a)+len(b))
}

// diffSnippet renders the changed region between a and b as a single
// unified-style hunk with line numbers, capped at max lines.
func diffSnippet(a, b string, max int) string {
	al := strings.Split(toLF(a), "\n")
	bl := strings.Split(toLF(b), "\n")
	if len(al) > 1 && len(bl) > 1 && al[len(al)-1] == "" && bl[len(bl)-1] == "" {
		al, bl = al[:len(al)-1], bl[:len(bl)-1]
	}
	p := 0
	for p < len(al) && p < len(bl) && al[p] == bl[p] {
		p++
	}
	s := 0
	for s < len(al)-p && s < len(bl)-p && al[len(al)-1-s] == bl[len(bl)-1-s] {
		s++
	}
	const ctx = 2
	cs := p - ctx
	if cs < 0 {
		cs = 0
	}
	ae, be := len(al)-s, len(bl)-s
	ce := ae + ctx
	if ce > len(al) {
		ce = len(al)
	}
	var out []string
	for i := cs; i < p; i++ {
		out = append(out, fmt.Sprintf(" %d\t%s", i+1, al[i]))
	}
	for i := p; i < ae; i++ {
		out = append(out, fmt.Sprintf("-%d\t%s", i+1, al[i]))
	}
	for i := p; i < be; i++ {
		out = append(out, fmt.Sprintf("+%d\t%s", i+1, bl[i]))
	}
	for i := ae; i < ce; i++ {
		out = append(out, fmt.Sprintf(" %d\t%s", be+(i-ae)+1, al[i]))
	}
	if len(out) > max {
		extra := len(out) - max
		out = append(out[:max], fmt.Sprintf("... %d more diff lines", extra))
	}
	for i, l := range out {
		out[i] = truncRunes(l, 300)
	}
	return strings.Join(out, "\n")
}
