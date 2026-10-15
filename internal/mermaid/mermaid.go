// Package mermaid renders Mermaid flowcharts as Unicode box diagrams for the
// terminal: layered layout, orthogonal edge routing on a character grid.
package mermaid

import "errors"

// Render returns a plain-text diagram (no ANSI), lines joined by \n, each line
// no wider than maxWidth display cells when possible. A left-right diagram
// that is too wide is retried top-down; if that is still too wide the
// narrower result is returned. maxWidth <= 0 means unlimited. Diagram types
// other than graph/flowchart yield an error.
func Render(src string, maxWidth int) (string, error) {
	g, err := parse(src)
	if err != nil {
		return "", err
	}
	if len(g.verts) == 1 {
		return "", errors.New("mermaid: diagram has no nodes")
	}
	out, w := render(g)
	if maxWidth > 0 && w > maxWidth && g.dir.horizontal() {
		g2, _ := parse(src)
		g2.dir = dirTD
		if out2, w2 := render(g2); w2 < w {
			return out2, nil
		}
	}
	return out, nil
}

func render(g *graph) (string, int) {
	g.layout()
	cv := newCanvas(g)
	cv.drawVerts()
	cv.routeAll()
	cv.drawTitles()
	cv.placeLabels()
	return cv.output()
}
