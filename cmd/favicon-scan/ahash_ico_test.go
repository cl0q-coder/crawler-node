package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestAHashIdentical(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 8), uint8(y * 8), 128, 255})
		}
	}
	a, b := AverageHash(img), AverageHash(img)
	if d := Hamming(a, b); d != 0 {
		t.Fatalf("identical images: distance %d", d)
	}
}

func TestAHashBlackWhite(t *testing.T) {
	white := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	black := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			white.Set(x, y, color.White)
			black.Set(x, y, color.Black)
		}
	}
	// Solid images: every cell equals the mean, so no bits set in either.
	if d := Hamming(AverageHash(white), AverageHash(black)); d != 0 {
		t.Fatalf("solid white vs solid black: distance %d, want 0 (uniform images carry no structure)", d)
	}
	// Half-white/half-black vs inverted: maximally different structure.
	hw := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	hb := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if x < 8 {
				hw.Set(x, y, color.White)
				hb.Set(x, y, color.Black)
			} else {
				hw.Set(x, y, color.Black)
				hb.Set(x, y, color.White)
			}
		}
	}
	if d := Hamming(AverageHash(hw), AverageHash(hb)); d != 64 {
		t.Fatalf("inverted halves: distance %d, want 64", d)
	}
}

func TestAHashSmallPerturbation(t *testing.T) {
	// Horizontal gradient base: left half dark, right half bright.
	base := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8(x * 4)
			base.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	// Variant: same gradient with one 8x8 black block in the bright half.
	varnt := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8(x * 4)
			varnt.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	for y := 24; y < 32; y++ {
		for x := 48; x < 56; x++ {
			varnt.Set(x, y, color.Black)
		}
	}
	d := Hamming(AverageHash(base), AverageHash(varnt))
	if d == 0 {
		t.Fatal("expected nonzero distance for perturbed image")
	}
	if d > 8 {
		t.Fatalf("perturbation distance %d too large; aHash should be stable", d)
	}
}

// buildTestICO wraps payloads as ICO entries: dir + entries + payload blobs.
func buildTestICO(t *testing.T, payloads [][]byte, dims [][2]int) []byte {
	t.Helper()
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(len(payloads)))
	off := 6 + 16*len(payloads)
	for i, p := range payloads {
		w, h := dims[i][0], dims[i][1]
		var e [16]byte
		if w < 256 {
			e[0] = byte(w)
		}
		if h < 256 {
			e[1] = byte(h)
		}
		binary.LittleEndian.PutUint32(e[8:12], uint32(len(p)))
		binary.LittleEndian.PutUint32(e[12:16], uint32(off))
		buf.Write(e[:])
		off += len(p)
	}
	for _, p := range payloads {
		buf.Write(p)
	}
	return buf.Bytes()
}

func TestDecodeICOPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 48, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 48; x++ {
			img.Set(x, y, color.RGBA{10, 20, 30, 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	// Two entries: 16x16 and 48x48 PNGs; decoder must pick the largest.
	small := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	var smallBuf bytes.Buffer
	if err := png.Encode(&smallBuf, small); err != nil {
		t.Fatal(err)
	}
	ico := buildTestICO(t,
		[][]byte{smallBuf.Bytes(), pngBuf.Bytes()},
		[][2]int{{16, 16}, {48, 48}})
	got, err := decodeICO(ico)
	if err != nil {
		t.Fatalf("decodeICO: %v", err)
	}
	if got.Bounds().Dx() != 48 || got.Bounds().Dy() != 48 {
		t.Fatalf("expected 48x48 (largest), got %v", got.Bounds())
	}
	// Also via the generic path.
	got2, format, err := decodeIconImage(ico)
	if err != nil || format != "ico" {
		t.Fatalf("decodeIconImage: %v %q", err, format)
	}
	_ = got2
}

func TestDecodeICOBMP32(t *testing.T) {
	w, h := 4, 4
	stride := w * 4
	px := make([]byte, stride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// BGRA: pure red, opaque.
			px[y*stride+4*x+0] = 0x00
			px[y*stride+4*x+1] = 0x00
			px[y*stride+4*x+2] = 0xFF
			px[y*stride+4*x+3] = 0xFF
		}
	}
	var hdr bytes.Buffer
	binary.Write(&hdr, binary.LittleEndian, uint32(40)) // header size
	binary.Write(&hdr, binary.LittleEndian, int32(w))
	binary.Write(&hdr, binary.LittleEndian, int32(h*2)) // includes AND mask
	binary.Write(&hdr, binary.LittleEndian, uint16(1))
	binary.Write(&hdr, binary.LittleEndian, uint16(32))
	binary.Write(&hdr, binary.LittleEndian, uint32(0)) // compression
	binary.Write(&hdr, binary.LittleEndian, uint32(len(px)))
	binary.Write(&hdr, binary.LittleEndian, int32(0))
	binary.Write(&hdr, binary.LittleEndian, int32(0))
	binary.Write(&hdr, binary.LittleEndian, uint32(0))
	binary.Write(&hdr, binary.LittleEndian, uint32(0))
	payload := append(hdr.Bytes(), px...)
	ico := buildTestICO(t, [][]byte{payload}, [][2]int{{w, h}})
	img, err := decodeICO(ico)
	if err != nil {
		t.Fatalf("decodeICO bmp32: %v", err)
	}
	r, g, b, _ := img.At(0, 0).RGBA()
	if r>>8 != 0xFF || g>>8 != 0x00 || b>>8 != 0x00 {
		t.Fatalf("expected red pixel, got %02x %02x %02x", r>>8, g>>8, b>>8)
	}
}

func TestDecodeICOBadMagic(t *testing.T) {
	if _, err := decodeICO([]byte("not an ico at all................")); err == nil {
		t.Fatal("expected error for bad magic")
	}
	if _, err := decodeICO([]byte{0, 0}); err == nil {
		t.Fatal("expected error for truncated input")
	}
}
