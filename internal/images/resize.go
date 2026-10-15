package images

import (
	"image"
	"image/color"
	"math"
)

// rowFunc writes row y (0-based within the bounds) of an image into dst as
// premultiplied RGBA in 0..255, 4 floats per pixel.
type rowFunc func(y int, dst []float32)

// rowReader returns a row decoder with fast paths for the types the
// standard decoders produce.
func rowReader(img image.Image) rowFunc {
	b := img.Bounds()
	w := b.Dx()
	switch m := img.(type) {
	case *image.RGBA:
		return func(y int, dst []float32) {
			pix := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:4*w]
			for i, c := range pix {
				dst[i] = float32(c)
			}
		}
	case *image.NRGBA:
		return func(y int, dst []float32) {
			pix := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:4*w]
			for i := 0; i < len(pix); i += 4 {
				a := float32(pix[i+3])
				dst[i] = float32(pix[i]) * a / 255
				dst[i+1] = float32(pix[i+1]) * a / 255
				dst[i+2] = float32(pix[i+2]) * a / 255
				dst[i+3] = a
			}
		}
	case *image.Gray:
		return func(y int, dst []float32) {
			pix := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:w]
			for i, c := range pix {
				v := float32(c)
				dst[4*i], dst[4*i+1], dst[4*i+2], dst[4*i+3] = v, v, v, 255
			}
		}
	case *image.YCbCr:
		return func(y int, dst []float32) {
			for x := range w {
				yi := m.YOffset(b.Min.X+x, b.Min.Y+y)
				ci := m.COffset(b.Min.X+x, b.Min.Y+y)
				r, g, bl := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
				dst[4*x], dst[4*x+1], dst[4*x+2], dst[4*x+3] = float32(r), float32(g), float32(bl), 255
			}
		}
	case *image.Paletted:
		pal := make([][4]float32, len(m.Palette))
		for i, c := range m.Palette {
			r, g, bl, a := c.RGBA()
			pal[i] = [4]float32{float32(r) / 257, float32(g) / 257, float32(bl) / 257, float32(a) / 257}
		}
		return func(y int, dst []float32) {
			pix := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):][:w]
			for i, c := range pix {
				var p [4]float32
				if int(c) < len(pal) {
					p = pal[c]
				}
				copy(dst[4*i:4*i+4], p[:])
			}
		}
	}
	return func(y int, dst []float32) {
		for x := range w {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			dst[4*x], dst[4*x+1], dst[4*x+2], dst[4*x+3] = float32(r)/257, float32(g)/257, float32(bl)/257, float32(a)/257
		}
	}
}

// boxWeights maps each of dst output cells to the src cells it covers:
// cell i covers src[first[i] : first[i]+n[i]] with weights w[off[i]:], each
// the covered fraction of the cell, so the weights of a cell sum to 1.
type boxWeights struct {
	first, n, off []int
	w             []float32
}

func newBoxWeights(src, dst int) boxWeights {
	bw := boxWeights{first: make([]int, dst), n: make([]int, dst), off: make([]int, dst), w: make([]float32, 0, src+dst)}
	scale := float64(src) / float64(dst)
	for i := range dst {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		j0 := int(lo)
		j1 := min(int(math.Ceil(hi)), src)
		bw.first[i], bw.n[i], bw.off[i] = j0, j1-j0, len(bw.w)
		for j := j0; j < j1; j++ {
			cover := min(hi, float64(j+1)) - max(lo, float64(j))
			bw.w = append(bw.w, float32(cover/scale))
		}
	}
	return bw
}

// resize downscales img so its long edge is maxEdge, keeping the aspect
// ratio, with an area-averaging box filter in premultiplied alpha. It
// streams source rows, so memory beyond the output is a few rows.
func resize(img image.Image, maxEdge int) *image.NRGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := maxEdge, maxEdge
	if sw >= sh {
		dh = max(1, (sh*maxEdge+sw/2)/sw)
	} else {
		dw = max(1, (sw*maxEdge+sh/2)/sh)
	}
	xs, ys := newBoxWeights(sw, dw), newBoxWeights(sh, dh)
	read := rowReader(img)
	src := make([]float32, 4*sw)
	hrow := make([]float32, 4*dw)
	acc := make([]float32, 4*dw)
	out := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		clear(acc)
		for k := range ys.n[y] {
			read(ys.first[y]+k, src)
			for x := range dw {
				var r, g, bl, a float32
				ws := xs.w[xs.off[x]:][:xs.n[x]]
				p := src[4*xs.first[x]:]
				for _, wt := range ws {
					r += p[0] * wt
					g += p[1] * wt
					bl += p[2] * wt
					a += p[3] * wt
					p = p[4:]
				}
				hrow[4*x], hrow[4*x+1], hrow[4*x+2], hrow[4*x+3] = r, g, bl, a
			}
			wy := ys.w[ys.off[y]+k]
			for i, v := range hrow {
				acc[i] += v * wy
			}
		}
		pix := out.Pix[y*out.Stride:][:4*dw]
		for i := 0; i < len(acc); i += 4 {
			a := acc[i+3]
			if a < 0.5 {
				continue // transparent: leave zero
			}
			pix[i] = unit8(acc[i] * 255 / a)
			pix[i+1] = unit8(acc[i+1] * 255 / a)
			pix[i+2] = unit8(acc[i+2] * 255 / a)
			pix[i+3] = unit8(a)
		}
	}
	return out
}

func unit8(v float32) uint8 {
	if v >= 255 {
		return 255
	}
	if v <= 0 {
		return 0
	}
	return uint8(v + 0.5)
}

// flatten composites img over white for JPEG encoding; opaque images are
// returned as is.
func flatten(img image.Image) image.Image {
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	read := rowReader(img)
	row := make([]float32, 4*w)
	for y := range h {
		read(y, row)
		pix := out.Pix[y*out.Stride:][:4*w]
		for i := 0; i < len(row); i += 4 {
			white := 255 - row[i+3]
			pix[i] = unit8(row[i] + white)
			pix[i+1] = unit8(row[i+1] + white)
			pix[i+2] = unit8(row[i+2] + white)
			pix[i+3] = 255
		}
	}
	return out
}
