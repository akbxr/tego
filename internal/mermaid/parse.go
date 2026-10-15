package mermaid

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type direction uint8

const (
	dirTD direction = iota
	dirBT
	dirLR
	dirRL
)

func (d direction) horizontal() bool { return d == dirLR || d == dirRL }

type shape uint8

const (
	shapeRect shape = iota
	shapeRound
	shapeDiamond
)

type lineStyle uint8

const (
	styleSolid lineStyle = iota
	styleDotted
	styleThick
	styleInvisible
)

type head uint8

const (
	headNone head = iota
	headArrow
	headCircle
	headCross
)

// vert is a node or a cluster (subgraph). Index 0 is the root cluster.
type vert struct {
	id      string
	lines   []string // node label lines, or cluster title (single line)
	shape   shape
	cluster bool
	claimed bool // a node already assigned to a subgraph by mention
	parent  int
	kids    []int

	// Layout: x,y relative to the parent's origin until absolutized.
	x, y, w, h int
	tmp        int
}

type edge struct {
	from, to int
	label    string
	style    lineStyle
	hFrom    head
	hTo      head
}

type graph struct {
	dir   direction
	verts []vert
	edges []edge
	ids   map[string]int
}

func parse(src string) (*graph, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	i := 0
	// Front matter.
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "---" {
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "---"; i++ {
		}
		i++
	}
	g := &graph{ids: map[string]int{}}
	g.verts = append(g.verts, vert{cluster: true, parent: -1})
	header := false
	stack := []int{0}
	for ; i < len(lines); i++ {
		for _, st := range splitStatements(lines[i]) {
			st = strings.TrimSpace(st)
			if st == "" || strings.HasPrefix(st, "%%") {
				continue
			}
			if !header {
				if err := g.header(st); err != nil {
					return nil, err
				}
				header = true
				continue
			}
			kw, rest := keyword(st)
			switch kw {
			case "subgraph":
				stack = append(stack, g.subgraph(rest, stack[len(stack)-1]))
				continue
			case "end":
				if rest == "" {
					if len(stack) == 1 {
						return nil, errors.New("mermaid: unexpected end")
					}
					stack = stack[:len(stack)-1]
					continue
				}
			case "direction", "classDef", "class", "style", "linkStyle", "click",
				"accTitle", "accDescr", "title":
				continue
			}
			if strings.HasPrefix(st, "accTitle:") || strings.HasPrefix(st, "accDescr") {
				continue
			}
			if err := g.statement(st, stack[len(stack)-1]); err != nil {
				return nil, err
			}
		}
	}
	if !header {
		return nil, errors.New("mermaid: empty diagram")
	}
	return g, nil
}

// splitStatements splits a line on ';' outside quotes and brackets.
func splitStatements(line string) []string {
	if !strings.Contains(line, ";") {
		return []string{line}
	}
	var out []string
	depth, start := 0, 0
	quote := false
	for i := range len(line) {
		switch c := line[i]; {
		case c == '"':
			quote = !quote
		case quote:
		case c == '[' || c == '(' || c == '{':
			depth++
		case c == ']' || c == ')' || c == '}':
			if depth > 0 {
				depth--
			}
		case c == ';' && depth == 0:
			out = append(out, line[start:i])
			start = i + 1
		}
	}
	return append(out, line[start:])
}

func keyword(st string) (string, string) {
	end := strings.IndexFunc(st, unicode.IsSpace)
	if end < 0 {
		return st, ""
	}
	return st[:end], strings.TrimSpace(st[end:])
}

func (g *graph) header(st string) error {
	kw, rest := keyword(st)
	switch kw {
	case "graph", "flowchart", "flowchart-elk":
	default:
		return fmt.Errorf("mermaid: unsupported diagram type %q", kw)
	}
	switch strings.TrimSpace(rest) {
	case "", "TD", "TB", "v":
		g.dir = dirTD
	case "BT", "^":
		g.dir = dirBT
	case "LR", ">":
		g.dir = dirLR
	case "RL", "<":
		g.dir = dirRL
	default:
		return fmt.Errorf("mermaid: unknown direction %q", rest)
	}
	return nil
}

func (g *graph) subgraph(rest string, parent int) int {
	id, title := rest, rest
	if i := strings.IndexByte(rest, '['); i >= 0 && strings.HasSuffix(rest, "]") {
		id = strings.TrimSpace(rest[:i])
		title = unquote(strings.TrimSpace(rest[i+1 : len(rest)-1]))
	} else if strings.HasPrefix(rest, "\"") {
		title = unquote(rest)
		id = ""
	}
	if id == "" {
		id = fmt.Sprintf("\x00subgraph%d", len(g.verts))
	}
	title = strings.Join(labelLines(title), " ")
	v, ok := g.ids[id]
	for p := parent; ok && p >= 0; p = g.verts[p].parent {
		if p == v { // "subgraph A" inside A: keep the tree acyclic
			ok = false
			id = fmt.Sprintf("\x00subgraph%d", len(g.verts))
		}
	}
	if !ok {
		v = len(g.verts)
		g.verts = append(g.verts, vert{id: id})
		g.ids[id] = v
	} else {
		g.detach(v)
	}
	g.verts[v].cluster = true
	g.verts[v].lines = []string{title}
	g.attach(v, parent)
	return v
}

func (g *graph) detach(v int) {
	p := g.verts[v].parent
	if p < 0 {
		return
	}
	kids := g.verts[p].kids
	for i, k := range kids {
		if k == v {
			g.verts[p].kids = append(kids[:i], kids[i+1:]...)
			break
		}
	}
}

func (g *graph) attach(v, parent int) {
	g.verts[v].parent = parent
	g.verts[parent].kids = append(g.verts[parent].kids, v)
}

// node returns the vertex for id, creating it, and applies subgraph membership:
// the first mention inside a subgraph moves a node into it.
func (g *graph) node(id string, sub int) int {
	v, ok := g.ids[id]
	if !ok {
		v = len(g.verts)
		g.verts = append(g.verts, vert{id: id, lines: []string{id}})
		g.ids[id] = v
		g.attach(v, sub)
		g.verts[v].claimed = sub != 0
		return v
	}
	if sub != 0 && !g.verts[v].claimed && !g.verts[v].cluster {
		g.detach(v)
		g.attach(v, sub)
		g.verts[v].claimed = true
	}
	return v
}

type scanner struct {
	s   string
	pos int
}

func (sc *scanner) skipSpace() {
	for sc.pos < len(sc.s) && (sc.s[sc.pos] == ' ' || sc.s[sc.pos] == '\t') {
		sc.pos++
	}
}

func (sc *scanner) eof() bool { return sc.pos >= len(sc.s) }

func (sc *scanner) peek(off int) byte {
	if sc.pos+off < len(sc.s) {
		return sc.s[sc.pos+off]
	}
	return 0
}

// edgeStart reports whether an edge token starts at the current position.
func (sc *scanner) edgeStart() bool {
	a, b := sc.peek(0), sc.peek(1)
	switch a {
	case '-':
		return b == '-' || b == '.' || b == '>'
	case '=':
		return b == '=' || b == '>'
	case '.':
		return b == '-'
	case '~':
		return b == '~'
	case '<':
		return b == '-' || b == '='
	}
	return false
}

func (g *graph) statement(st string, sub int) error {
	sc := &scanner{s: st}
	prev, err := g.nodeGroup(sc, sub)
	if err != nil {
		return err
	}
	for {
		sc.skipSpace()
		if sc.eof() {
			return nil
		}
		e, err := sc.edge()
		if err != nil {
			return fmt.Errorf("mermaid: %v in %q", err, st)
		}
		next, err := g.nodeGroup(sc, sub)
		if err != nil {
			return err
		}
		for _, a := range prev {
			for _, b := range next {
				e.from, e.to = a, b
				g.edges = append(g.edges, e)
			}
		}
		prev = next
	}
}

func (g *graph) nodeGroup(sc *scanner, sub int) ([]int, error) {
	var out []int
	for {
		v, err := g.nodeRef(sc, sub)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		sc.skipSpace()
		if sc.peek(0) != '&' {
			return out, nil
		}
		sc.pos++
	}
}

func (g *graph) nodeRef(sc *scanner, sub int) (int, error) {
	sc.skipSpace()
	start := sc.pos
	for !sc.eof() {
		c := sc.s[sc.pos]
		if c == ' ' || c == '\t' || c == '&' || c == '|' || strings.IndexByte("[({>", c) >= 0 || sc.edgeStart() {
			break
		}
		if c == ':' && strings.HasPrefix(sc.s[sc.pos:], ":::") {
			break
		}
		if c == '@' && sc.peek(1) == '{' {
			break
		}
		sc.pos++
	}
	id := sc.s[start:sc.pos]
	if id == "" {
		return 0, fmt.Errorf("mermaid: expected node id in %q", sc.s)
	}
	v := g.node(id, sub)
	label, sh, ok, err := sc.shape()
	if err != nil {
		return 0, err
	}
	if ok && !g.verts[v].cluster {
		g.verts[v].lines = labelLines(label)
		g.verts[v].shape = sh
	}
	if strings.HasPrefix(sc.s[sc.pos:], ":::") {
		sc.pos += 3
		for !sc.eof() && sc.s[sc.pos] != ' ' && sc.s[sc.pos] != '\t' && sc.s[sc.pos] != '&' && !sc.edgeStart() {
			sc.pos++
		}
	}
	return v, nil
}

var shapeDelims = []struct {
	open, close string
	shape       shape
}{
	{"(((", ")))", shapeRound},
	{"([", "])", shapeRound},
	{"[[", "]]", shapeRect},
	{"[(", ")]", shapeRound},
	{"[/", "/]", shapeRect},
	{"[/", "\\]", shapeRect},
	{"[\\", "\\]", shapeRect},
	{"[\\", "/]", shapeRect},
	{"((", "))", shapeRound},
	{"{{", "}}", shapeDiamond},
	{"[", "]", shapeRect},
	{"(", ")", shapeRound},
	{"{", "}", shapeDiamond},
	{">", "]", shapeRect},
}

func (sc *scanner) shape() (string, shape, bool, error) {
	rest := sc.s[sc.pos:]
	if strings.HasPrefix(rest, "@{") {
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return "", 0, false, fmt.Errorf("mermaid: unterminated @{ in %q", sc.s)
		}
		body := rest[2:end]
		sc.pos += end + 1
		if i := strings.Index(body, "label:"); i >= 0 {
			l := strings.TrimSpace(body[i+6:])
			if strings.HasPrefix(l, "\"") {
				if j := strings.IndexByte(l[1:], '"'); j >= 0 {
					return l[1 : j+1], shapeRect, true, nil
				}
			}
			if j := strings.IndexByte(l, ','); j >= 0 {
				l = l[:j]
			}
			return strings.TrimSpace(l), shapeRect, true, nil
		}
		return "", 0, false, nil
	}
	for _, d := range shapeDelims {
		if !strings.HasPrefix(rest, d.open) {
			continue
		}
		body := rest[len(d.open):]
		var label string
		var n int
		if t := strings.TrimLeft(body, " "); strings.HasPrefix(t, "\"") {
			q := strings.IndexByte(t[1:], '"')
			if q < 0 {
				return "", 0, false, fmt.Errorf("mermaid: unterminated string in %q", sc.s)
			}
			after := strings.TrimLeft(t[q+2:], " ")
			if !strings.HasPrefix(after, d.close) {
				continue
			}
			label = t[1 : q+1]
			n = len(body) - len(after) + len(d.close)
		} else {
			c := strings.Index(body, d.close)
			if c < 0 {
				continue
			}
			label = body[:c]
			n = c + len(d.close)
		}
		sc.pos += len(d.open) + n
		return label, d.shape, true, nil
	}
	return "", 0, false, nil
}

// edge parses an edge token plus an optional |label|.
func (sc *scanner) edge() (edge, error) {
	var e edge
	if sc.peek(0) == '<' {
		e.hFrom = headArrow
		sc.pos++
	}
	start := sc.pos
	for !sc.eof() && strings.IndexByte("-=.~", sc.s[sc.pos]) >= 0 {
		sc.pos++
	}
	body := sc.s[start:sc.pos]
	if body == "" && sc.peek(0) != '>' {
		return e, errors.New("expected edge")
	}
	e.hTo = sc.endHead()
	if e.hTo == headNone && e.hFrom == headNone && (body == "--" || body == "-." || body == "==") {
		// "-- text -->" form.
		closer := map[string]string{"--": "--", "-.": ".-", "==": "=="}[body]
		i := strings.Index(sc.s[sc.pos:], closer)
		if i < 0 {
			return e, errors.New("unterminated edge label")
		}
		e.label = cleanLabel(unquote(strings.TrimSpace(sc.s[sc.pos : sc.pos+i])))
		sc.pos += i
		start = sc.pos
		for !sc.eof() && strings.IndexByte("-=.", sc.s[sc.pos]) >= 0 {
			sc.pos++
		}
		body += sc.s[start:sc.pos]
		e.hTo = sc.endHead()
	}
	switch {
	case strings.Contains(body, "~"):
		e.style = styleInvisible
	case strings.Contains(body, "."):
		e.style = styleDotted
	case strings.Contains(body, "="):
		e.style = styleThick
	}
	sc.skipSpace()
	if sc.peek(0) == '|' {
		end := strings.IndexByte(sc.s[sc.pos+1:], '|')
		if end < 0 {
			return e, errors.New("unterminated edge label")
		}
		e.label = cleanLabel(unquote(strings.TrimSpace(sc.s[sc.pos+1 : sc.pos+1+end])))
		sc.pos += end + 2
	}
	return e, nil
}

func (sc *scanner) endHead() head {
	switch c := sc.peek(0); c {
	case '>':
		sc.pos++
		return headArrow
	case 'o', 'x':
		n := sc.peek(1)
		if n == 0 || n == ' ' || n == '\t' || n == '|' {
			sc.pos++
			if c == 'o' {
				return headCircle
			}
			return headCross
		}
	}
	return headNone
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		s = s[1 : len(s)-1]
	}
	return s
}

var entities = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&nbsp;", " ", "&#39;", "'",
	"#quot;", "\"", "#amp;", "&", "#lt;", "<", "#gt;", ">", "#35;", "#", "#59;", ";",
)

// labelLines splits a node label on <br> and \n, strips simple HTML tags and
// decodes common entities.
func labelLines(s string) []string {
	s = unquote(strings.TrimSpace(s))
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == 'n' {
			b.WriteByte('\n')
			i++
			continue
		}
		if c == '<' {
			if j := strings.IndexByte(s[i:], '>'); j > 0 {
				tag := strings.ToLower(strings.TrimSpace(strings.Trim(s[i+1:i+j], "/ ")))
				if tag == "br" {
					b.WriteByte('\n')
					i += j
					continue
				}
				if isSimpleTag(tag) {
					i += j
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	parts := strings.Split(entities.Replace(b.String()), "\n")
	out := parts[:0]
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func isSimpleTag(t string) bool {
	switch t {
	case "b", "i", "u", "em", "strong", "small", "big", "code", "sub", "sup", "span", "p", "div":
		return true
	}
	return false
}

func cleanLabel(s string) string { return strings.Join(labelLines(s), " ") }
