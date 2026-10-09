package main

import (
	"image"
	"math/bits"
)

// 64-bit average hash (aHash) for the #111 v1 re-encoded-variant measurement.
// Downsamples to 8x8 grayscale via box averaging (no new deps), then sets one
// bit per cell above the mean. Coarse, but sufficient to estimate how many
// near-duplicate (resized/re-encoded) brand favicons exist per exact hit —
// the number that decides whether pHash v2 is worth building.

// AverageHash computes the 64-bit aHash of img.
func AverageHash(img image.Image) uint64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	var cells [64]float64
	for cy := 0; cy < 8; cy++ {
		for cx := 0; cx < 8; cx++ {
			x0 := b.Min.X + cx*w/8
			x1 := b.Min.X + (cx+1)*w/8
			y0 := b.Min.Y + cy*h/8
			y1 := b.Min.Y + (cy+1)*h/8
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			var sum float64
			var n float64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					// ITU-R BT.601 luma on 16-bit channels.
					lum := (299*float64(r) + 587*float64(g) + 114*float64(bl)) / 1000 / 257
					sum += lum
					n++
				}
			}
			cells[cy*8+cx] = sum / n
		}
	}
	var mean float64
	for _, v := range cells {
		mean += v
	}
	mean /= 64
	var hash uint64
	for i, v := range cells {
		if v > mean {
			hash |= 1 << (63 - i)
		}
	}
	return hash
}

// Hamming returns the Hamming distance between two 64-bit hashes.
func Hamming(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}
