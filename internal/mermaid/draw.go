package mermaid

import (
	"container/heap"
	"slices"
	"strings"
)

// Directions: index into dx/dy; arm bit = 1<<d; opposite = d^2.
const (
	up = iota
	right
	down
	left
)

var (
	dx = [4]int{0, 1, 0, -1}
	dy = [4]int{-1, 0, 1, 0}
)

const (
	axisV = 1<<up | 1<<down
	axisH = 1<<left | 1<<right
)

func axisOf(d int) uint8 {
	if d == up || d == down {
		return axisV
	}
	return axisH
}

type cellKind uint8

const (
	kFree   cellKind = iota
	kBox             // node box: border and interior, never crossed
	kBorder          // cluster side: crossed perpendicularly only
	kHard            // cluster corner or title
)

const contCell = -1 // second half of a wide rune

type cell struct {
	ch                rune // explicit glyph; overrides arms
	arms, heavy, dots uint8
	kind              cellKind
	style             lineStyle
	noMerge           bool
	box               int32 // vertex of a kBox/kBorder cell
	own               int32 // occupying edge: -1 none, -2 several
	src, dst          int32 // merge keys of the occupying edges, -1 if mixed
}

type canvas struct {
	g     *graph
	w, h  int
	c     []cell
	extra map[int][]rune // zero-width runes following a cell

	// router scratch, reused across edges
	dist    []int32
	prev    []int32
	goal    []int8 // per cell: required arrival direction+1 at a target port
	goalAdd []int16
	pq      pqueue
	paths   [][]int32
	related []bool // clusters containing an endpoint of the edge being routed
}

func newCanvas(g *graph) *canvas {
	root := &g.verts[0]
	cv := &canvas{g: g, w: root.w + 2*margin, h: root.h + 2*margin}
	cv.c = make([]cell, cv.w*cv.h)
	for i := range cv.c {
		cv.c[i].box, cv.c[i].own, cv.c[i].src, cv.c[i].dst = -1, -1, -1, -1
	}
	return cv
}

func (cv *canvas) at(x, y int) *cell { return &cv.c[y*cv.w+x] }

func (cv *canvas) in(x, y int) bool { return x >= 0 && y >= 0 && x < cv.w && y < cv.h }

// text writes s at (x,y) and returns the number of cells used.
func (cv *canvas) text(x, y int, s string, kind cellKind) int {
	x0 := x
	for _, r := range s {
		w := widthCond.RuneWidth(r)
		if w == 0 {
			if x > x0 {
				if cv.extra == nil {
					cv.extra = map[int][]rune{}
				}
				i := y*cv.w + x - 1
				if cv.c[i].ch == contCell {
					i--
				}
				cv.extra[i] = append(cv.extra[i], r)
			}
			continue
		}
		if !cv.in(x+w-1, y) {
			break
		}
		c := cv.at(x, y)
		c.ch = r
		if kind != kFree {
			c.kind = kind
		}
		if w == 2 {
			c2 := cv.at(x+1, y)
			c2.ch = contCell
			if kind != kFree {
				c2.kind = kind
			}
		}
		x += w
	}
	return x - x0
}

func (cv *canvas) frame(v *vert, kind cellKind, idx int, corners [4]rune) {
	x0, y0, x1, y1 := v.x, v.y, v.x+v.w-1, v.y+v.h-1
	if kind == kBox {
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				c := cv.at(x, y)
				c.kind, c.box = kBox, int32(idx)
			}
		}
	}
	for x := x0 + 1; x < x1; x++ {
		for _, y := range [2]int{y0, y1} {
			c := cv.at(x, y)
			c.arms |= axisH
			c.kind, c.box = kind, int32(idx)
		}
	}
	for y := y0 + 1; y < y1; y++ {
		for _, x := range [2]int{x0, x1} {
			c := cv.at(x, y)
			c.arms |= axisV
			c.kind, c.box = kind, int32(idx)
		}
	}
	ck := kind
	if kind == kBorder {
		ck = kHard
	}
	for i, p := range [4][2]int{{x0, y0}, {x1, y0}, {x0, y1}, {x1, y1}} {
		c := cv.at(p[0], p[1])
		c.ch, c.kind, c.box = corners[i], ck, int32(idx)
	}
}

var (
	cornersRect    = [4]rune{'┌', '┐', '└', '┘'}
	cornersRound   = [4]rune{'╭', '╮', '╰', '╯'}
	cornersDiamond = [4]rune{'╱', '╲', '╲', '╱'}
)

func (cv *canvas) drawVerts() {
	g := cv.g
	for i := 1; i < len(g.verts); i++ {
		v := &g.verts[i]
		if v.cluster {
			cv.frame(v, kBorder, i, cornersRect)
			continue
		}
		corners := cornersRect
		switch v.shape {
		case shapeRound:
			corners = cornersRound
		case shapeDiamond:
			corners = cornersDiamond
		}
		cv.frame(v, kBox, i, corners)
		inner := v.w - 4
		for j, l := range v.lines {
			cv.text(v.x+2+(inner-strWidth(l))/2, v.y+1+j, l, kBox)
		}
	}
}

// drawTitles writes cluster titles into their top border, after routing, at
// the leftmost stretch no edge crosses (falling back to the bottom border,
// then the blank row inside the top border).
func (cv *canvas) drawTitles() {
	for i := 1; i < len(cv.g.verts); i++ {
		v := &cv.g.verts[i]
		if !v.cluster || v.lines[0] == "" {
			continue
		}
		t := " " + v.lines[0] + " "
		tw := strWidth(t)
		x, y, ok := cv.titleSpot(v, v.y, tw)
		if !ok {
			x, y, ok = cv.titleSpot(v, v.y+v.h-1, tw)
		}
		if !ok {
			x, y = v.x+1, v.y+1
			for xx := v.x + 1; xx+tw <= v.x+v.w-1; xx++ {
				if cv.freeRun(xx, y, tw) {
					x = xx
					break
				}
			}
		}
		cv.text(x, y, t, kHard)
	}
}

func (cv *canvas) titleSpot(v *vert, y, tw int) (int, int, bool) {
	plain := func(x int) bool {
		c := cv.at(x, y)
		if c.ch != 0 || c.arms != axisH {
			return false
		}
		for _, yy := range [2]int{y - 1, y + 1} {
			if cv.in(x, yy) {
				if n := cv.at(x, yy); n.ch != 0 && n.kind == kFree || n.arms&axisV != 0 && n.kind == kFree {
					return false
				}
			}
		}
		return true
	}
	for x := v.x + 2; x+tw <= v.x+v.w-2; x++ {
		ok := true
		for k := range tw {
			if !plain(x + k) {
				ok = false
				break
			}
		}
		if ok {
			return x, y, true
		}
	}
	return 0, 0, false
}

// Routing costs.
const (
	costStep        = 10
	costBend        = 25
	costCross       = 40
	costBorder      = 10  // crossing the frame of a cluster containing an endpoint
	costForeign     = 150 // crossing into an unrelated cluster
	costNearBox     = 8
	costSideLat     = 150 // side port on a forward edge
	costSideLatBack = 30  // side port on a backward or same-layer edge
	costSideBack    = 150
	costOffCenter   = 25
	costLoose       = 300
)

// ports lists the cells adjacent to a box side whose border cell is plain;
// dir is the outward direction of the side.
func (cv *canvas) ports(v *vert, fn func(x, y, d, off int)) {
	x0, y0, x1, y1 := v.x, v.y, v.x+v.w-1, v.y+v.h-1
	cx, cy := (x0+x1)/2, (y0+y1)/2
	plain := func(x, y int) bool { c := cv.at(x, y); return c.ch == 0 }
	for x := x0 + 1; x < x1; x++ {
		if plain(x, y0) && y0 > 0 {
			fn(x, y0-1, up, abs(x-cx))
		}
		if plain(x, y1) && y1+1 < cv.h {
			fn(x, y1+1, down, abs(x-cx))
		}
	}
	for y := y0 + 1; y < y1; y++ {
		if plain(x0, y) && x0 > 0 {
			fn(x0-1, y, left, abs(y-cy))
		}
		if plain(x1, y) && x1+1 < cv.w {
			fn(x1+1, y, right, abs(y-cy))
		}
	}
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func (cv *canvas) flowDir() int {
	switch cv.g.dir {
	case dirBT:
		return up
	case dirLR:
		return right
	case dirRL:
		return left
	}
	return down
}

// travelCost is the port cost for leaving or entering a box travelling in d.
// Forward edges strongly prefer the flow sides; others may use any side.
func (cv *canvas) travelCost(d int, fwd bool) int {
	f := cv.flowDir()
	switch {
	case d == f:
		return 0
	case d == f^2:
		return costSideBack
	case fwd:
		return costSideLat
	}
	return costSideLatBack
}

// forward reports whether b lies entirely downstream of a.
func (cv *canvas) forward(a, b *vert) bool {
	switch cv.g.dir {
	case dirLR:
		return b.x >= a.x+a.w
	case dirRL:
		return b.x+b.w <= a.x
	case dirBT:
		return b.y+b.h <= a.y
	}
	return b.y >= a.y+a.h
}

func (cv *canvas) srcKey(e *edge) int32 {
	if e.hFrom != headNone {
		return -1
	}
	return int32(e.from)
}

func dstKey(e *edge) int32 { return int32(e.to)<<2 | int32(e.hTo) }

func (cv *canvas) compatible(e *edge, c *cell) bool {
	if c.own == -1 || c.noMerge || c.style != e.style {
		return false
	}
	return (c.src >= 0 && c.src == cv.srcKey(e)) || (c.dst >= 0 && c.dst == dstKey(e))
}

func (cv *canvas) routeAll() {
	g := cv.g
	n := cv.w * cv.h
	cv.dist = make([]int32, 4*n)
	cv.prev = make([]int32, 4*n)
	cv.goal = make([]int8, n)
	cv.goalAdd = make([]int16, n)
	cv.paths = make([][]int32, len(g.edges))
	cv.related = make([]bool, len(g.verts))
	order := make([]int, 0, len(g.edges))
	for i := range g.edges {
		if g.edges[i].style != styleInvisible {
			order = append(order, i)
		}
	}
	dist := make([]int, len(g.edges))
	for _, i := range order {
		a, b := &g.verts[g.edges[i].from], &g.verts[g.edges[i].to]
		dist[i] = abs(2*a.x+a.w-2*b.x-b.w) + abs(2*a.y+a.h-2*b.y-b.h)
	}
	slices.SortStableFunc(order, func(a, b int) int { return dist[a] - dist[b] })
	for _, i := range order {
		p := cv.route(i, false)
		if p == nil {
			p = cv.route(i, true)
		}
		if p != nil {
			cv.commit(i, p)
			cv.paths[i] = p
		}
	}
}

// route finds a path of cells from a port of e.from to a port of e.to with
// A*, state = (cell, arrival direction). loose ignores other edges.
func (cv *canvas) route(ei int, loose bool) []int32 {
	g := cv.g
	e := &g.edges[ei]
	src, dst := &g.verts[e.from], &g.verts[e.to]
	for i := range cv.dist {
		cv.dist[i] = -1
	}
	clear(cv.related)
	for _, v := range [2]int{e.from, e.to} {
		for ; v >= 0; v = g.verts[v].parent {
			cv.related[v] = true
		}
	}
	enterable := func(c *cell) bool {
		if c.kind != kFree || c.ch != 0 {
			return false
		}
		return loose || c.arms == 0 || cv.compatible(e, c)
	}
	fwd := cv.forward(src, dst)
	var goals []int32
	cv.ports(dst, func(x, y, d, off int) {
		c := cv.at(x, y)
		ok := enterable(c)
		if !ok && c.ch != 0 && c.kind == kFree && cv.compatible(e, c) && e.hTo != headNone {
			ok = true // shared arrowhead
		}
		if !ok {
			return
		}
		i := int32(y*cv.w + x)
		cv.goal[i] = int8(d^2) + 1
		cv.goalAdd[i] = int16(cv.travelCost(d^2, fwd) + off*costOffCenter)
		goals = append(goals, i)
	})
	defer func() {
		for _, i := range goals {
			cv.goal[i] = 0
		}
	}()
	tx0, ty0, tx1, ty1 := dst.x-1, dst.y-1, dst.x+dst.w, dst.y+dst.h
	hfn := func(x, y int) int32 {
		d := max(tx0-x, 0, x-tx1) + max(ty0-y, 0, y-ty1)
		return int32(d * costStep)
	}
	horiz := g.dir.horizontal()
	mainDist := func(x, y int) int {
		if horiz {
			return max(src.x-x, 0, x-(src.x+src.w-1))
		}
		return max(src.y-y, 0, y-(src.y+src.h-1))
	}
	best, bestState := int32(-1), int32(-1)
	tryGoal := func(ci int32, d int, cost int32, st int32) {
		if gd := cv.goal[ci]; gd != 0 && int(gd-1) == d {
			t := cost + int32(cv.goalAdd[ci])
			if best < 0 || t < best {
				best, bestState = t, st
			}
		}
	}
	cv.pq = cv.pq[:0]
	cv.ports(src, func(x, y, d, off int) {
		c := cv.at(x, y)
		if !enterable(c) {
			return
		}
		ci := int32(y*cv.w + x)
		st := ci*4 + int32(d)
		cost := int32(cv.travelCost(d, fwd) + off*costOffCenter)
		if cv.dist[st] >= 0 && cv.dist[st] <= cost {
			return
		}
		cv.dist[st], cv.prev[st] = cost, -1
		tryGoal(ci, d, cost, st)
		heap.Push(&cv.pq, pqItem{cost + hfn(x, y), st})
	})
	for cv.pq.Len() > 0 {
		it := heap.Pop(&cv.pq).(pqItem)
		if best >= 0 && it.f >= best {
			break
		}
		st := it.st
		ci, d := st/4, int(st%4)
		g0 := cv.dist[st]
		if it.f-hfn(int(ci)%cv.w, int(ci)/cv.w) > g0 {
			continue // stale
		}
		x, y := int(ci)%cv.w, int(ci)/cv.w
		cur := &cv.c[ci]
		straight := !loose && (cur.kind == kBorder || (cur.arms != 0 && !cv.compatible(e, cur)))
		if cv.goal[ci] != 0 && cur.ch != 0 {
			continue // shared arrowhead: terminal only
		}
		for nd := range 4 {
			if nd == d^2 || (straight && nd != d) {
				continue
			}
			nx, ny := x+dx[nd], y+dy[nd]
			if !cv.in(nx, ny) {
				continue
			}
			ni := int32(ny*cv.w + nx)
			n := &cv.c[ni]
			if n.kind == kBox || n.kind == kHard {
				continue
			}
			cost := g0 + costStep
			if nd != d {
				cost += int32(costBend + mainDist(x, y))
			}
			nst := ni*4 + int32(nd)
			if n.ch != 0 {
				if cv.goal[ni] != 0 && cv.compatible(e, n) {
					tryGoal(ni, nd, cost, nst)
					if cv.dist[nst] < 0 || cost < cv.dist[nst] {
						cv.dist[nst], cv.prev[nst] = cost, st
					}
				}
				continue
			}
			switch {
			case n.arms == 0:
			case loose:
				cost += costLoose
			case n.kind == kBorder:
				if n.arms != axisOf(nd^1) {
					continue
				}
				if cv.related[n.box] {
					cost += costBorder
				} else {
					cost += costForeign
				}
			case cv.compatible(e, n):
			case n.arms == axisOf(nd^1):
				cost += costCross
			default:
				continue
			}
			if cv.nearBox(nx, ny, e) {
				cost += costNearBox
			}
			if cv.dist[nst] >= 0 && cv.dist[nst] <= cost {
				continue
			}
			cv.dist[nst], cv.prev[nst] = cost, st
			tryGoal(ni, nd, cost, nst)
			heap.Push(&cv.pq, pqItem{cost + hfn(nx, ny), nst})
		}
	}
	if bestState < 0 {
		return nil
	}
	var path []int32
	for st := bestState; st >= 0; st = cv.prev[st] {
		path = append(path, st)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// nearBox reports whether (x,y) touches a box or cluster frame other than
// the edge's ends.
func (cv *canvas) nearBox(x, y int, e *edge) bool {
	for d := range 4 {
		nx, ny := x+dx[d], y+dy[d]
		if !cv.in(nx, ny) {
			continue
		}
		c := cv.at(nx, ny)
		if c.kind != kFree && c.box >= 0 && c.box != int32(e.from) && c.box != int32(e.to) {
			return true
		}
	}
	return false
}

// commit draws a path (states = cell*4 + arrival direction) onto the canvas.
func (cv *canvas) commit(ei int, path []int32) {
	e := &cv.g.edges[ei]
	var hv, dt uint8
	switch e.style {
	case styleThick:
		hv = 0xF
	case styleDotted:
		dt = 0xF
	}
	addArm := func(ci int32, d int) {
		c := &cv.c[ci]
		c.arms |= 1 << d
		c.heavy |= hv & (1 << d)
		c.dots |= dt & (1 << d)
	}
	sk, dk := cv.srcKey(e), dstKey(e)
	for _, st := range path {
		c := &cv.c[st/4]
		switch {
		case c.own == -1 && c.arms == 0 && c.ch == 0:
			c.own, c.src, c.dst, c.style = int32(ei), sk, dk, e.style
		case c.own == int32(ei):
		case cv.compatible(e, c):
			c.own = -2
			if c.src != sk {
				c.src = -1
			}
			if c.dst != dk {
				c.dst = -1
			}
		default:
			c.own, c.noMerge = -2, true
		}
	}
	for i := 1; i < len(path); i++ {
		d := int(path[i] % 4)
		addArm(path[i-1]/4, d)
		addArm(path[i]/4, d^2)
	}
	first, last := path[0], path[len(path)-1]
	fd, ld := int(first%4), int(last%4) // outward from source; into target
	fc, lc := first/4, last/4
	if e.hFrom != headNone {
		cv.c[fc].ch = headGlyph(e.hFrom, fd^2)
	} else {
		addArm(fc, fd^2)
		addArm(fc-int32(dx[fd]+dy[fd]*cv.w), fd)
	}
	if e.hTo != headNone {
		cv.c[lc].ch = headGlyph(e.hTo, ld)
	} else {
		addArm(lc, ld)
		addArm(lc+int32(dx[ld]+dy[ld]*cv.w), ld^2)
	}
}

func headGlyph(h head, d int) rune {
	switch h {
	case headCircle:
		return 'o'
	case headCross:
		return 'x'
	}
	return [4]rune{'▲', '►', '▼', '◄'}[d]
}

// placeLabels writes edge labels onto (or next to) their routes.
func (cv *canvas) placeLabels() {
	for ei := range cv.g.edges {
		e := &cv.g.edges[ei]
		p := cv.paths[ei]
		if e.label == "" || p == nil {
			continue
		}
		cv.placeLabel(ei, e.label, p)
	}
}

func (cv *canvas) free(x, y int) bool {
	if !cv.in(x, y) {
		return false
	}
	c := cv.at(x, y)
	return c.kind == kFree && c.arms == 0 && c.ch == 0
}

func (cv *canvas) freeRun(x, y, n int) bool {
	for i := range n {
		if !cv.free(x+i, y) {
			return false
		}
	}
	return true
}

func (cv *canvas) placeLabel(ei int, label string, p []int32) {
	lw := strWidth(label)
	mid := len(p) / 2
	own := func(i int) (x, y int, a uint8, ok bool) {
		ci := p[i] / 4
		c := &cv.c[ci]
		x, y = int(ci)%cv.w, int(ci)/cv.w
		ok = c.own == int32(ei) && c.ch == 0 && c.kind == kFree && (c.arms == axisH || c.arms == axisV)
		return x, y, c.arms, ok
	}
	bestScore := -1
	var bx, by int
	var btext string
	consider := func(score, x, y int, s string) {
		if bestScore < 0 || score < bestScore {
			bestScore, bx, by, btext = score, x, y, s
		}
	}
	// 1. Horizontal runs of exclusively owned straight cells: write over the line.
	for i := 1; i < len(p)-1; {
		x, y, a, ok := own(i)
		if !ok || a != axisH {
			i++
			continue
		}
		j := i
		for j+1 < len(p)-1 {
			_, _, a2, ok2 := own(j + 1)
			if !ok2 || a2 != axisH {
				break
			}
			j++
		}
		n := j - i + 1
		x2, _, _, _ := own(j)
		lo := min(x, x2)
		s := ""
		switch {
		case lw+4 <= n:
			s = " " + label + " "
		case lw+2 <= n:
			s = label
		}
		if s != "" {
			sw := strWidth(s)
			score := abs((i+j)/2 - mid)
			consider(score, lo+(n-sw)/2, y, s)
		}
		i = j + 1
	}
	// 2. Vertical cells: centre the label across the line.
	for i := 1; i < len(p)-1; i++ {
		x, y, a, ok := own(i)
		if !ok || a != axisV {
			continue
		}
		lx := x - lw/2
		if cv.freeRun(lx-1, y, x-lx+1) && cv.freeRun(x+1, y, lx+lw-x) {
			consider(abs(i-mid), lx, y, label)
		}
	}
	// 3. Beside the route.
	if bestScore < 0 {
		for i := 1; i < len(p)-1; i++ {
			ci := p[i] / 4
			x, y := int(ci)%cv.w, int(ci)/cv.w
			score := 1000 + abs(i-mid)
			if cv.freeRun(x+1, y, lw+2) {
				consider(score, x+2, y, label)
			}
			if cv.freeRun(x-lw-2, y, lw+2) {
				consider(score+1, x-lw-1, y, label)
			}
			for _, yy := range [2]int{y - 1, y + 1} {
				if cv.freeRun(x-lw/2-1, yy, lw+2) {
					consider(score+2, x-lw/2, yy, label)
				}
			}
		}
	}
	// 4. Anywhere near the middle of the route.
	if bestScore < 0 {
		ci := p[mid] / 4
		x0, y0 := int(ci)%cv.w, int(ci)/cv.w
	search:
		for r := 1; r < cv.w+cv.h; r++ {
			for y := y0 - r; y <= y0+r; y++ {
				for x := x0 - r - lw; x <= x0+r; x++ {
					if cv.freeRun(x-1, y, lw+2) {
						consider(0, x, y, label)
						break search
					}
				}
			}
		}
	}
	if bestScore >= 0 {
		cv.text(bx, by, btext, kFree)
	}
}

// Junction glyphs indexed by arm mask (up=1, right=2, down=4, left=8).
var (
	lightGlyphs = [16]rune{' ', '│', '─', '╰', '│', '│', '╭', '├', '─', '╯', '─', '┴', '╮', '┤', '┬', '┼'}
	heavyGlyphs = [16]rune{' ', '┃', '━', '┗', '┃', '┃', '┏', '┣', '━', '┛', '━', '┻', '┓', '┫', '┳', '╋'}
)

// glyph returns the line-drawing rune for a set of arms.
func glyph(arms, heavy, dots uint8) rune {
	if heavy&arms == arms {
		return heavyGlyphs[arms]
	}
	if dots&arms == arms {
		switch heavyGlyphs[arms] {
		case '┃':
			return '┆'
		case '━':
			return '┄'
		}
	}
	return lightGlyphs[arms]
}

// output renders the canvas, trimming blank margins, and returns the text
// and its width in cells.
func (cv *canvas) output() (string, int) {
	rows := make([][]rune, 0, cv.h)
	lefts := make([]int, 0, cv.h)
	minLeft := cv.w
	for y := range cv.h {
		row := make([]rune, cv.w)
		first, last := -1, -1
		for x := range cv.w {
			c := cv.at(x, y)
			r := c.ch
			if r == 0 {
				if c.arms == 0 {
					r = ' '
				} else {
					r = glyph(c.arms, c.heavy, c.dots)
				}
			}
			row[x] = r
			if r != ' ' {
				if first < 0 {
					first = x
				}
				last = x
			}
		}
		rows = append(rows, row[:last+1])
		lefts = append(lefts, first)
		if first >= 0 {
			minLeft = min(minLeft, first)
		}
	}
	top, bot := 0, len(rows)-1
	for top <= bot && lefts[top] < 0 {
		top++
	}
	for bot >= top && lefts[bot] < 0 {
		bot--
	}
	var b strings.Builder
	width := 0
	for y := top; y <= bot; y++ {
		row := rows[y]
		if len(row) > minLeft {
			width = max(width, len(row)-minLeft)
			for x := minLeft; x < len(row); x++ {
				r := row[x]
				if r == contCell {
					continue
				}
				b.WriteRune(r)
				for _, z := range cv.extra[y*cv.w+x] {
					b.WriteRune(z)
				}
			}
		}
		if y < bot {
			b.WriteByte('\n')
		}
	}
	return b.String(), width
}

type pqItem struct {
	f  int32
	st int32
}

type pqueue []pqItem

func (q pqueue) Len() int { return len(q) }
func (q pqueue) Less(i, j int) bool {
	if q[i].f != q[j].f {
		return q[i].f < q[j].f
	}
	return q[i].st < q[j].st
}
func (q pqueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *pqueue) Push(x any)   { *q = append(*q, x.(pqItem)) }
func (q *pqueue) Pop() any {
	old := *q
	it := old[len(old)-1]
	*q = old[:len(old)-1]
	return it
}
