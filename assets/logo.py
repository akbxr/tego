"""tego's pixel logo: one bitmap, rendered as SVG and PNG.

    python3 assets/logo.py assets    # needs Pillow

writes tego.svg (the README's), tego.png (a 1280x400 banner) and
tego-icon.png (1024x1024, "tg": the Discord application's icon).
"""
import sys
from PIL import Image, ImageDraw

# Glyphs on a 9-row grid: rows 0-1 ascender, 2-6 x-height (baseline 6), 7-8 descender.
G = {
    "t": [".##..", ".##..", "#####", ".##..", ".##..", ".##..", "..###", ".....", "....."],
    "e": ["......", "......", ".####.", "##..##", "######", "##....", ".####.", "......", "......"],
    "g": ["......", "......", ".#####", "##..##", "##..##", "##..##", ".#####", "....##", ".####."],
    "o": ["......", "......", ".####.", "##..##", "##..##", "##..##", ".####.", "......", "......"],
}
# Palettes: row colours top to bottom (light to deep), and the shadow.
PALETTES = {
    "magenta": (["#ffd1f0", "#ffb3e6", "#ff8fd8", "#ff6ec7", "#f24fb8", "#d93aa6", "#b82d94", "#9a2585", "#7d1f76"], "#2b0b35"),
    "mint": (["#e0fff4", "#c4fdea", "#9ff7dc", "#7cefcd", "#5fe3bd", "#48d0aa", "#36b896", "#289c80", "#1d816b"], "#0b3a30"),
}
# "te" in tego's prompt magenta, "go" in mint.
COLOURS = {"t": "magenta", "e": "magenta", "g": "mint", "o": "mint"}
BG = "#120d1a"


def bitmap(word, gap=1):
    """The word's cells, each with its letter's palette."""
    cells, x0 = {}, 0
    for ch in word:
        g = G[ch]
        for y in range(9):
            for x, c in enumerate(g[y]):
                if c == "#":
                    cells[(x0 + x, y)] = COLOURS[ch]
        x0 += len(g[0]) + gap
    return cells


def bbox(cells):
    """The cells' box, with a column and a row more for the shadow."""
    xs, ys = [x for x, _ in cells], [y for _, y in cells]
    return min(xs), max(xs) + 2, min(ys), max(ys) + 2


def layers(cells):
    """(cells, colour) to draw in order: every shadow, then the letters row by row."""
    out = []
    for name, (_, shadow) in PALETTES.items():
        out.append(({(x + 1, y + 1) for (x, y), p in cells.items() if p == name}, shadow))
    for name, (rows, _) in PALETTES.items():
        for y, colour in enumerate(rows):
            out.append(({(x, yy) for (x, yy), p in cells.items() if p == name and yy == y}, colour))
    return [(cs, c) for cs, c in out if cs]


def svg(path, word="tego", px=10, bg=BG, pad=(4, 2)):
    cells = bitmap(word)
    x0, x1, y0, y1 = bbox(cells)
    W, H = (x1 - x0 + 2 * pad[0]) * px, (y1 - y0 + 2 * pad[1]) * px
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" shape-rendering="crispEdges">',
           "<title>tego</title>"]
    if bg:
        out.append(f'<rect width="{W}" height="{H}" rx="{px}" fill="{bg}"/>')
    for cs, colour in layers(cells):  # one path per colour keeps the file small
        d = "".join(f"M{(x-x0+pad[0])*px} {(y-y0+pad[1])*px}h{px}v{px}h-{px}z" for x, y in sorted(cs))
        out.append(f'<path fill="{colour}" d="{d}"/>')
    out.append("</svg>")
    open(path, "w").write("\n".join(out) + "\n")


def png(path, word="tego", size=(1200, 400), fill=0.78, bg=BG):
    cells = bitmap(word)
    x0, x1, y0, y1 = bbox(cells)
    px = int(min(size[0] * fill / (x1 - x0), size[1] * fill / (y1 - y0)))
    img = Image.new("RGBA", size, bg)
    d = ImageDraw.Draw(img)
    ox = (size[0] - (x1 - x0) * px) // 2 - x0 * px
    oy = (size[1] - (y1 - y0) * px) // 2 - y0 * px
    for cs, colour in layers(cells):
        for x, y in cs:
            X, Y = ox + x * px, oy + y * px
            d.rectangle([X, Y, X + px - 1, Y + px - 1], fill=colour)
    img.save(path)


if __name__ == "__main__":
    out = sys.argv[1]
    svg(f"{out}/tego.svg")
    png(f"{out}/tego.png", size=(1280, 400), fill=0.72)
    png(f"{out}/tego-icon.png", word="tg", size=(1024, 1024), fill=0.62)
