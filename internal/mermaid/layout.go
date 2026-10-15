package mermaid

import (
	"math"
	"slices"
	"strings"

	"github.com/mattn/go-runewidth"
)

var widthCond = &runewidth.Condition{}

// strWidth sums rune widths; text is written rune by rune with the same rule,
// so measurements and drawing always agree.
func strWidth(s string) int {
	w := 0
	for _, r := range s {
		w += widthCond.RuneWidth(r)
	}
	return w
}

const (
	maxLabelWidth   = 30 // node label lines are word-wrapped beyond this
	clusterPadMain  = 2  // blank cells between a cluster border and its content, along the flow
	clusterPadCross = 1  // ... and across it
	crossGapLR      = 1
	crossGapTD      = 3
	layerGapLR      = 4
	layerGapTD      = 2
	margin          = 3 // routing space around the whole diagram (trimmed on output)
)

// unit is a box (or a dummy for a long edge) inside one container's layering.
type unit struct {
	vert        int // vertex index, -1 for dummies
	layer       int
	main, cross int // extents along / across the flow
	pos         int // cross-axis start
	up, down    []int
}

// link joins units in adjacent layers; offU/offV are attachment offsets along
// the cross axis, relative to each unit's start.
type link struct {
	u, v       int // u in layer l, v in layer l+1
	offU, offV int
}

func (g *graph) mainSize(v *vert) int {
	if g.dir.horizontal() {
		return v.w
	}
	return v.h
}

func (g *graph) crossSize(v *vert) int {
	if g.dir.horizontal() {
		return v.h
	}
	return v.w
}

func (g *graph) crossPos(v *vert) int {
	if g.dir.horizontal() {
		return v.y
	}
	return v.x
}

// lift returns the child of c that is v or contains v, or -1.
func (g *graph) lift(v, c int) int {
	for v >= 0 {
		p := g.verts[v].parent
		if p == c {
			return v
		}
		v = p
	}
	return -1
}

// attach returns the cross offset, relative to kid k, at which an edge to its
// descendant x should attach.
func (g *graph) attachOffset(k, x int) int {
	off := g.crossSize(&g.verts[x]) / 2
	for y := x; y != k; y = g.verts[y].parent {
		off += g.crossPos(&g.verts[y])
	}
	return off
}

func (g *graph) sizeNode(v *vert) {
	var lines []string
	for _, l := range v.lines {
		lines = append(lines, wrap(l, maxLabelWidth)...)
	}
	v.lines = lines
	w := 1
	for _, l := range lines {
		w = max(w, strWidth(l))
	}
	v.w = w + 4
	v.h = len(lines) + 2
}

// wrap breaks s at spaces so that lines fit width where possible.
func wrap(s string, width int) []string {
	if strWidth(s) <= width {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	cw := 0
	for _, word := range strings.Fields(s) {
		ww := strWidth(word)
		if cw > 0 && cw+1+ww > width {
			out = append(out, cur.String())
			cur.Reset()
			cw = 0
		}
		if cw > 0 {
			cur.WriteByte(' ')
			cw++
		}
		cur.WriteString(word)
		cw += ww
	}
	return append(out, cur.String())
}

func (g *graph) layout() {
	g.layoutCluster(0)
	g.absolutize(0, margin, margin)
}

func (g *graph) absolutize(c, x, y int) {
	v := &g.verts[c]
	v.x += x
	v.y += y
	for _, k := range v.kids {
		g.absolutize(k, v.x, v.y)
	}
}

func (g *graph) layoutCluster(c int) {
	kids := g.verts[c].kids
	for i, k := range kids {
		if g.verts[k].cluster {
			g.layoutCluster(k)
		} else {
			g.sizeNode(&g.verts[k])
		}
		g.verts[k].tmp = i
	}
	n := len(kids)

	// Edges between distinct children of c.
	type ledge struct{ a, b, e int }
	var les []ledge
	for ei := range g.edges {
		e := &g.edges[ei]
		a, b := g.lift(e.from, c), g.lift(e.to, c)
		if a < 0 || b < 0 || a == b {
			continue
		}
		les = append(les, ledge{g.verts[a].tmp, g.verts[b].tmp, ei})
	}

	// Break cycles: DFS in declaration order, back edges are reversed.
	out := make([][]int, n)
	for i, le := range les {
		out[le.a] = append(out[le.a], i)
	}
	rev := make([]bool, len(les))
	state := make([]uint8, n)
	var dfs func(u int)
	dfs = func(u int) {
		state[u] = 1
		for _, i := range out[u] {
			switch b := les[i].b; state[b] {
			case 0:
				dfs(b)
			case 1:
				rev[i] = true
			}
		}
		state[u] = 2
	}
	for u := range n {
		if state[u] == 0 {
			dfs(u)
		}
	}

	// Longest-path layering over the DAG, then pull sources down next to
	// their successors.
	succ := make([][]int, n)
	indeg := make([]int, n)
	for i, le := range les {
		a, b := le.a, le.b
		if rev[i] {
			a, b = b, a
		}
		succ[a] = append(succ[a], b)
		indeg[b]++
	}
	layer := make([]int, n)
	queue := make([]int, 0, n)
	deg := slices.Clone(indeg)
	for u := range n {
		if deg[u] == 0 {
			queue = append(queue, u)
		}
	}
	for i := 0; i < len(queue); i++ {
		u := queue[i]
		for _, v := range succ[u] {
			layer[v] = max(layer[v], layer[u]+1)
			if deg[v]--; deg[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	for u := range n {
		if indeg[u] == 0 && len(succ[u]) > 0 {
			m := math.MaxInt
			for _, v := range succ[u] {
				m = min(m, layer[v])
			}
			layer[u] = m - 1
		}
	}
	nl := 0
	for _, l := range layer {
		nl = max(nl, l+1)
	}
	if g.dir == dirBT || g.dir == dirRL {
		for u := range layer {
			layer[u] = nl - 1 - layer[u]
		}
	}

	// Units, dummies and links.
	units := make([]unit, n, n+len(les))
	for i, k := range kids {
		v := &g.verts[k]
		units[i] = unit{vert: k, layer: layer[i], main: g.mainSize(v), cross: g.crossSize(v)}
	}
	var links []link
	horiz := g.dir.horizontal()
	gapNeed := make([]int, nl)
	for _, le := range les {
		e := &g.edges[le.e]
		a, b := le.a, le.b
		offA, offB := g.attachOffset(kids[a], e.from), g.attachOffset(kids[b], e.to)
		if layer[a] > layer[b] {
			a, b, offA, offB = b, a, offB, offA
		}
		lw := 0
		if e.label != "" && e.style != styleInvisible {
			lw = strWidth(e.label)
		}
		span := layer[b] - layer[a]
		if span == 1 && (lw > 0 || e.hFrom != headNone) {
			need := 3 // room for a line between two heads, or a label row
			if horiz && lw > 0 {
				need = lw + 8
			}
			gapNeed[layer[a]] = max(gapNeed[layer[a]], need)
		}
		prev, prevOff := a, offA
		for l := layer[a] + 1; l < layer[b]; l++ {
			d := unit{vert: -1, layer: l, main: 1, cross: 1}
			if lw > 0 && l == layer[a]+span/2 {
				if horiz {
					d.main = lw + 4
				} else {
					d.cross = lw + 2
				}
			}
			units = append(units, d)
			di := len(units) - 1
			links = append(links, link{prev, di, prevOff, d.cross / 2})
			prev, prevOff = di, d.cross/2
		}
		links = append(links, link{prev, b, prevOff, offB})
	}
	for i, lk := range links {
		units[lk.u].down = append(units[lk.u].down, i)
		units[lk.v].up = append(units[lk.v].up, i)
	}

	layers := make([][]int, nl)
	for i := range units {
		l := units[i].layer
		layers[l] = append(layers[l], i)
	}
	orderLayers(units, links, layers)

	crossGap := crossGapTD
	if horiz {
		crossGap = crossGapLR
	}
	placeCross(units, links, layers, crossGap)

	// Main axis: layer slots and gaps.
	slot := make([]int, nl)
	for i := range units {
		slot[units[i].layer] = max(slot[units[i].layer], units[i].main)
	}
	bends := make([]int, nl)
	for _, lk := range links {
		if units[lk.u].pos+lk.offU != units[lk.v].pos+lk.offV {
			bends[units[lk.u].layer]++
		}
	}
	start := make([]int, nl)
	pos := 0
	for l := range nl {
		start[l] = pos
		gap := layerGapTD + min((bends[l]+1)/2, 3)
		if horiz {
			gap = layerGapLR + min(bends[l], 4)
		}
		pos += slot[l] + max(gap, gapNeed[l])
	}
	contentMain, contentCross := 0, 0
	if nl > 0 {
		contentMain = start[nl-1] + slot[nl-1]
	}
	for i := range units {
		contentCross = max(contentCross, units[i].pos+units[i].cross)
	}
	for i := range n {
		u := &units[i]
		v := &g.verts[u.vert]
		m := start[u.layer] + (slot[u.layer]-u.main)/2
		if horiz {
			v.x, v.y = m, u.pos
		} else {
			v.x, v.y = u.pos, m
		}
	}

	cv := &g.verts[c]
	w, h := contentCross, contentMain
	if horiz {
		w, h = contentMain, contentCross
	}
	if c == 0 {
		cv.w, cv.h = w, h
		return
	}
	offX, offY := 1+clusterPadCross, 1+clusterPadMain
	if horiz {
		offX, offY = offY, offX
	}
	cv.w, cv.h = w+2*offX, h+2*offY
	if tw := strWidth(cv.lines[0]) + 6; cv.w < tw {
		offX += (tw - cv.w) / 2
		cv.w = tw
	}
	for _, k := range kids {
		g.verts[k].x += offX
		g.verts[k].y += offY
	}
}

// orderLayers reduces crossings with barycenter sweeps, keeping the best order.
func orderLayers(units []unit, links []link, layers [][]int) {
	if len(layers) < 2 {
		return
	}
	order := make([]int, len(units))
	renumber := func(l []int) {
		for i, u := range l {
			order[u] = i
		}
	}
	for _, l := range layers {
		renumber(l)
	}
	crossings := func() int {
		c := 0
		for i := range links {
			a := &links[i]
			for j := i + 1; j < len(links); j++ {
				b := &links[j]
				if units[a.u].layer != units[b.u].layer {
					continue
				}
				du, dv := order[a.u]-order[b.u], order[a.v]-order[b.v]
				if du*dv < 0 {
					c++
				}
			}
		}
		return c
	}
	best := crossings()
	saved := make([][]int, len(layers))
	save := func() {
		for i, l := range layers {
			saved[i] = append(saved[i][:0], l...)
		}
	}
	save()
	bary := make([]float64, len(units))
	sweep := func(l int, upward bool) {
		for _, u := range layers[l] {
			nb := units[u].up
			if upward {
				nb = units[u].down
			}
			if len(nb) == 0 {
				bary[u] = float64(order[u])
				continue
			}
			s := 0
			for _, li := range nb {
				if upward {
					s += order[links[li].v]
				} else {
					s += order[links[li].u]
				}
			}
			bary[u] = float64(s) / float64(len(nb))
		}
		slices.SortStableFunc(layers[l], func(a, b int) int {
			switch {
			case bary[a] < bary[b]:
				return -1
			case bary[a] > bary[b]:
				return 1
			}
			return 0
		})
		renumber(layers[l])
	}
	for range 6 {
		if best == 0 {
			break
		}
		for l := 1; l < len(layers); l++ {
			sweep(l, false)
		}
		for l := len(layers) - 2; l >= 0; l-- {
			sweep(l, true)
		}
		if c := crossings(); c < best {
			best = c
			save()
		}
	}
	for i := range layers {
		layers[i] = append(layers[i][:0], saved[i]...)
	}
}

// placeCross assigns cross-axis positions: each unit is pulled towards its
// neighbours, subject to order and spacing (isotonic regression per layer).
func placeCross(units []unit, links []link, layers [][]int, gap int) {
	for _, l := range layers {
		p := 0
		for _, u := range l {
			units[u].pos = p
			p += units[u].cross + gap
		}
	}
	// desired is the mean aligned position over neighbours; boxes follow
	// boxes and only fall back to dummies when they have no box neighbour.
	desired := func(u int, up, down bool) float64 {
		var s, n [2]int
		add := func(nb, p int) {
			k := 0
			if units[nb].vert < 0 {
				k = 1
			}
			s[k] += p
			n[k]++
		}
		if up {
			for _, li := range units[u].up {
				lk := &links[li]
				add(lk.u, units[lk.u].pos+lk.offU-lk.offV)
			}
		}
		if down {
			for _, li := range units[u].down {
				lk := &links[li]
				add(lk.v, units[lk.v].pos+lk.offV-lk.offU)
			}
		}
		k := 0
		if n[0] == 0 || units[u].vert < 0 {
			if n[0]+n[1] == 0 {
				return float64(units[u].pos)
			}
			s[0], n[0] = s[0]+s[1], n[0]+n[1]
		}
		return float64(s[k]) / float64(n[k])
	}
	// Weighted pool-adjacent-violators; dummies yield to boxes.
	type block struct {
		sum, w float64
		first  int
	}
	var blocks []block
	target := make([]float64, 0, 16)
	pass := func(l []int, up, down bool) {
		target = target[:0]
		blocks = blocks[:0]
		off := 0
		for i, u := range l {
			d := desired(u, up, down)
			target = append(target, d-float64(off))
			off += units[u].cross + gap
			w := 100.0
			if units[u].vert < 0 {
				w = 1
			}
			blocks = append(blocks, block{target[i] * w, w, i})
			for len(blocks) > 1 {
				a, b := blocks[len(blocks)-2], blocks[len(blocks)-1]
				if a.sum/a.w <= b.sum/b.w {
					break
				}
				blocks = blocks[:len(blocks)-1]
				blocks[len(blocks)-1] = block{a.sum + b.sum, a.w + b.w, a.first}
			}
		}
		off = 0
		bi := 0
		for i, u := range l {
			for bi+1 < len(blocks) && blocks[bi+1].first <= i {
				bi++
			}
			units[u].pos = int(math.Round(blocks[bi].sum/blocks[bi].w)) + off
			off += units[u].cross + gap
		}
	}
	for range 8 {
		for l := 1; l < len(layers); l++ {
			pass(layers[l], true, false)
		}
		for l := len(layers) - 2; l >= 0; l-- {
			pass(layers[l], false, true)
		}
	}
	for _, l := range layers {
		pass(l, true, true)
	}
	lo := math.MaxInt
	for i := range units {
		lo = min(lo, units[i].pos)
	}
	for i := range units {
		units[i].pos -= lo
	}
}
