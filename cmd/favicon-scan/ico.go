package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
)

// Minimal ICO decoder (no new deps): picks the largest entry, supports
// PNG-compressed entries (the common case) and 32/24-bit BMP entries.
// Enough for favicon aHash computation in the #111 v1 measurement.

var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

type icoEntry struct {
	width, height int
	offset        int
	size          int
}

func decodeICO(data []byte) (image.Image, error) {
	if len(data) < 6 {
		return nil, fmt.Errorf("ico: too short")
	}
	if binary.LittleEndian.Uint16(data[0:2]) != 0 || binary.LittleEndian.Uint16(data[2:4]) != 1 {
		return nil, fmt.Errorf("ico: bad magic/type")
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count == 0 || count > 256 {
		return nil, fmt.Errorf("ico: bad entry count %d", count)
	}
	if len(data) < 6+16*count {
		return nil, fmt.Errorf("ico: truncated directory")
	}
	var best *icoEntry
	for i := 0; i < count; i++ {
		e := data[6+16*i : 6+16*(i+1)]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		h := int(e[1])
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(e[8:12]))
		off := int(binary.LittleEndian.Uint32(e[12:16]))
		if off < 0 || size <= 0 || off+size > len(data) {
			continue
		}
		cand := &icoEntry{width: w, height: h, offset: off, size: size}
		if best == nil || w*h > best.width*best.height {
			best = cand
		}
	}
	if best == nil {
		return nil, fmt.Errorf("ico: no usable entries")
	}
	payload := data[best.offset : best.offset+best.size]
	if bytes.HasPrefix(payload, pngMagic) {
		// Q4 (#283) bomb guard: a PNG entry's declared dimensions are
		// trusted by image.Decode's allocator — screen them first.
		if iconDimsTooBig(payload) {
			return nil, fmt.Errorf("ico png entry: dimensions exceed %dpx cap", maxIconPixelDim)
		}
		img, _, err := image.Decode(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("ico png entry: %w", err)
		}
		return img, nil
	}
	return decodeICOBMP(payload, best.width, best.height)
}

// decodeICOBMP decodes a BMP payload inside an ICO entry (32-bit BGRA or
// 24-bit BGR, bottom-up or top-down). The AND mask is ignored: for aHash
// purposes the color plane is what matters.
func decodeICOBMP(payload []byte, w, h int) (image.Image, error) {
	if len(payload) < 40 {
		return nil, fmt.Errorf("ico bmp: too short")
	}
	if binary.LittleEndian.Uint32(payload[0:4]) != 40 {
		return nil, fmt.Errorf("ico bmp: unsupported header size")
	}
	pw := int(int32(binary.LittleEndian.Uint32(payload[4:8])))
	ph := int(int32(binary.LittleEndian.Uint32(payload[8:12])))
	planes := binary.LittleEndian.Uint16(payload[12:14])
	bpp := binary.LittleEndian.Uint16(payload[14:16])
	if planes != 1 || (bpp != 32 && bpp != 24) {
		return nil, fmt.Errorf("ico bmp: unsupported planes=%d bpp=%d", planes, bpp)
	}
	topDown := false
	if ph < 0 {
		topDown = true
		ph = -ph
	}
	// ICO BMP height includes the AND mask (2x image height).
	imgH := ph / 2
	if imgH <= 0 {
		imgH = ph
	}
	if pw <= 0 || imgH <= 0 {
		return nil, fmt.Errorf("ico bmp: bad dimensions %dx%d", pw, imgH)
	}
	// Q4 (#283) bomb guard: cap dimensions BEFORE the stride*imgH multiply
	// below. Huge BMP headers (e.g. 2^31x2^31) overflowed `need` negative,
	// bypassed the length check, and then panicked fatally inside
	// image.NewNRGBA. 4096 keeps the worst-case alloc at 64 MiB.
	if pw > maxIconPixelDim || imgH > maxIconPixelDim {
		return nil, fmt.Errorf("ico bmp: dimensions %dx%d exceed %dpx cap", pw, imgH, maxIconPixelDim)
	}
	stride := ((pw*int(bpp) + 31) / 32) * 4
	need := 40 + stride*imgH
	if len(payload) < need {
		return nil, fmt.Errorf("ico bmp: truncated pixels")
	}
	out := image.NewNRGBA(image.Rect(0, 0, pw, imgH))
	px := payload[40:]
	for y := 0; y < imgH; y++ {
		srcY := y
		if !topDown {
			srcY = imgH - 1 - y
		}
		row := px[srcY*stride:]
		for x := 0; x < pw; x++ {
			switch bpp {
			case 32:
				b, g, r, a := row[4*x], row[4*x+1], row[4*x+2], row[4*x+3]
				out.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
			case 24:
				b, g, r := row[3*x], row[3*x+1], row[3*x+2]
				out.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 0xFF})
			}
		}
	}
	return out, nil
}
