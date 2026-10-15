package main

// Track Q rung Q4 (#283): favicon decode fuzz — bomb guard, malformed
// ICO/PNG/WebP inputs, AND-mask/best-fit selection logic.
// Acceptance: crash-free + bounded memory.
//
// Two fuzz targets mirror the #279/#295 infra: seed corpus runs under
// `go test ./...`, the mutator gets a bounded CI window (see
// .github/workflows/ci.yml `fuzz-favicon` job).
//
// Known-deferred class (skip rule, not ours to fix): none currently — the
// stdlib PNG/GIF/JPEG decode-bomb class is guarded at OUR layer by
// iconDimsTooBig (declared dims are screened before the stdlib allocator
// runs), and the hand-rolled ICO BMP path is capped by maxIconPixelDim.

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// ---------- seed builders ----------

// testBMP builds a BMP payload as found inside an ICO entry: 40-byte
// BITMAPINFOHEADER, 32/24-bit pixels, bottom-up unless topDown. The height
// field follows the ICO AND-mask convention (2x the image height).
func testBMP(w, hDecoded, bpp int, topDown bool) []byte {
	ph := 2 * hDecoded
	if topDown {
		ph = -ph
	}
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:4], 40)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(int32(w)))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(int32(ph)))
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], uint16(bpp))
	stride := ((w*bpp + 31) / 32) * 4
	pix := make([]byte, stride*hDecoded)
	for i := range pix {
		pix[i] = byte(i * 31)
	}
	return append(hdr, pix...)
}

// icoSeedEntry is one directory entry for testICO. w/h are the ICO byte
// values (0 encodes 256).
type icoSeedEntry struct {
	w, h    byte
	payload []byte
}

func testICO(entries ...icoSeedEntry) []byte {
	n := len(entries)
	out := make([]byte, 6+16*n)
	binary.LittleEndian.PutUint16(out[0:2], 0)
	binary.LittleEndian.PutUint16(out[2:4], 1)
	binary.LittleEndian.PutUint16(out[4:6], uint16(n))
	off := 6 + 16*n
	for i, e := range entries {
		d := out[6+16*i : 6+16*(i+1)]
		d[0], d[1] = e.w, e.h
		d[2] = 32 // color count (unused by decoder)
		binary.LittleEndian.PutUint32(d[8:12], uint32(len(e.payload)))
		binary.LittleEndian.PutUint32(d[12:16], uint32(off))
		off += len(e.payload)
	}
	for _, e := range entries {
		out = append(out, e.payload...)
	}
	return out
}

// testPNGBytes encodes a tiny valid PNG.
func testPNGBytes(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 16), G: uint8(y * 16), B: 0x80, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// testGIFBytes is a hand-rolled minimal 1x1 GIF89a.
func testGIFBytes() []byte {
	return []byte{
		'G', 'I', 'F', '8', '9', 'a', // magic
		0x01, 0x00, 0x01, 0x00, // 1x1 logical screen
		0x80, 0x00, 0x00, // GCT flag, bg, aspect
		0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, // black, white
		0x21, 0xF9, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, // GCE
		0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, // image descriptor
		0x02, 0x02, 0x44, 0x01, 0x00, // image data
		0x3B, // trailer
	}
}

// pngBombBytes crafts a PNG whose IHDR declares huge dimensions (CRC left
// zero — the bomb guard rejects before any CRC/decode work).
func pngBombBytes(w, h uint32) []byte {
	out := append([]byte{}, pngMagic...)
	out = append(out, 0, 0, 0, 13) // IHDR length
	out = append(out, 'I', 'H', 'D', 'R')
	var dims [8]byte
	binary.BigEndian.PutUint32(dims[0:4], w)
	binary.BigEndian.PutUint32(dims[4:8], h)
	out = append(out, dims[:]...)
	out = append(out, 8, 2, 0, 0, 0) // bitdepth/colorType/comp/filter/interlace
	out = append(out, 0, 0, 0, 0)    // CRC (unchecked — guard fires first)
	return out
}

// gifBombBytes declares huge logical-screen dimensions.
func gifBombBytes(w, h uint16) []byte {
	out := []byte{'G', 'I', 'F', '8', '9', 'a', 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(out[6:8], w)
	binary.LittleEndian.PutUint16(out[8:10], h)
	return append(out, 0x3B)
}

// jpegBombBytes is SOI + SOF0 declaring huge dimensions.
func jpegBombBytes(w, h uint16) []byte {
	out := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x08, 0x08}
	var dims [4]byte
	binary.BigEndian.PutUint16(dims[0:2], h)
	binary.BigEndian.PutUint16(dims[2:4], w)
	out = append(out, dims[:]...)
	return append(out, 0x01, 0xFF, 0xD9)
}

// icoBMPBombBytes builds an ICO whose single BMP entry header claims ~2^31
// x 2^31 px with only a 48-byte payload. Pre-Q4 this overflowed the
// stride*imgH multiply in decodeICOBMP (need went negative), bypassed the
// length check, and panicked fatally inside image.NewNRGBA.
func icoBMPBombBytes() []byte {
	hdr := make([]byte, 40)
	binary.LittleEndian.PutUint32(hdr[0:4], 40)
	binary.LittleEndian.PutUint32(hdr[4:8], 0x7FFFFFFF)  // pw
	binary.LittleEndian.PutUint32(hdr[8:12], 0x7FFFFFFF) // ph -> imgH ~2^30
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], 32)
	payload := append(hdr, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22)
	return testICO(icoSeedEntry{w: 16, h: 16, payload: payload})
}

// faviconFuzzSeeds is the shared seed corpus: valid inputs of every
// supported shape plus malformed/bomb inputs. Bomb seeds assert graceful
// errors, never panics or unbounded allocs.
func faviconFuzzSeeds() [][]byte {
	png16 := testPNGBytes(16, 16)
	return [][]byte{
		// Valid shapes.
		testICO(icoSeedEntry{w: 16, h: 16, payload: png16}),                      // ICO, PNG entry
		testICO(icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 32, false)}), // ICO, 32-bit BMP
		testICO(icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 24, false)}), // ICO, 24-bit BMP
		testICO(icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 32, true)}),  // ICO, top-down BMP
		testICO(icoSeedEntry{w: 0, h: 0, payload: testBMP(256, 256, 32, false)}), // ICO, 0-byte = 256px
		testPNGBytes(4, 4), // raw PNG
		testGIFBytes(),     // raw GIF
		// Multi-entry best-fit selection: 16/32/48 -> must pick 48.
		testICO(
			icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 32, false)},
			icoSeedEntry{w: 48, h: 48, payload: testBMP(48, 48, 32, false)},
			icoSeedEntry{w: 32, h: 32, payload: testBMP(32, 32, 32, false)},
		),
		// Malformed ICOs: must error, never panic.
		{},                                   // empty
		{0x00},                               // truncated
		{0x00, 0x00, 0x01, 0x00, 0x01, 0x00}, // valid header, zero payload
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, // bad magic/type
		{0x00, 0x00, 0x01, 0x00, 0x00, 0x00}, // count = 0
		{0x00, 0x00, 0x01, 0x00, 0x01, 0x01}, // count = 257 > 256
		{0x00, 0x00, 0x01, 0x00, 0x02, 0x00, 0xAA},                       // count 2, truncated directory
		testICO(icoSeedEntry{w: 16, h: 16, payload: []byte{0x01, 0x02}}), // BMP payload too short
		append(append([]byte{}, pngMagic...), 0xDE, 0xAD),                // PNG magic + garbage
		// Non-image / wrong-format inputs.
		[]byte("<html><head><link rel=\"icon\" href=\"/x.png\"></head></html>"),
		[]byte("RIFF\x00\x00\x00\x00WEBPgarbage"),
		[]byte{0xFF, 0xD8, 0xFF, 0xD9},
		// Bomb inputs: graceful errors, bounded memory.
		icoBMPBombBytes(),
		pngBombBytes(100000, 100000),
		gifBombBytes(60000, 60000),
		jpegBombBytes(20000, 20000),
	}
}

// ---------- property tests ----------

// TestDecodeIconValidSeeds: every valid seed decodes with expected dims.
func TestDecodeIconValidSeeds(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		wantW   int
		wantH   int
		wantFmt string
	}{
		{"ico-png-entry", testICO(icoSeedEntry{w: 16, h: 16, payload: testPNGBytes(16, 16)}), 16, 16, "ico"},
		{"ico-bmp32", testICO(icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 32, false)}), 16, 16, "ico"},
		{"ico-bmp24", testICO(icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 24, false)}), 16, 16, "ico"},
		{"ico-topdown", testICO(icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 32, true)}), 16, 16, "ico"},
		{"raw-png", testPNGBytes(4, 4), 4, 4, "png"},
		{"raw-gif", testGIFBytes(), 1, 1, "gif"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, format, err := decodeIconImage(tc.data)
			if err != nil {
				t.Fatalf("decodeIconImage: %v", err)
			}
			if format != tc.wantFmt {
				t.Errorf("format = %q, want %q", format, tc.wantFmt)
			}
			b := img.Bounds()
			if b.Dx() != tc.wantW || b.Dy() != tc.wantH {
				t.Errorf("dims = %dx%d, want %dx%d", b.Dx(), b.Dy(), tc.wantW, tc.wantH)
			}
		})
	}
}

// TestDecodeICOBestFitSelection: with 16/32/48px entries the decoder must
// pick the largest (48px), not the first.
func TestDecodeICOBestFitSelection(t *testing.T) {
	data := testICO(
		icoSeedEntry{w: 16, h: 16, payload: testBMP(16, 16, 32, false)},
		icoSeedEntry{w: 48, h: 48, payload: testBMP(48, 48, 32, false)},
		icoSeedEntry{w: 32, h: 32, payload: testBMP(32, 32, 32, false)},
	)
	img, err := decodeICO(data)
	if err != nil {
		t.Fatalf("decodeICO: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 48 || b.Dy() != 48 {
		t.Errorf("best-fit picked %dx%d, want 48x48", b.Dx(), b.Dy())
	}
}

// TestDecodeICOAndMaskHeight: an ICO BMP whose header height is 2x the image
// height (AND mask appended) must decode to the image height, not 2x.
func TestDecodeICOAndMaskHeight(t *testing.T) {
	payload := testBMP(16, 16, 32, false) // ph = 32 = 2*16
	maskStride := ((16 + 31) / 32) * 4
	payload = append(payload, make([]byte, maskStride*16)...) // AND mask rows
	data := testICO(icoSeedEntry{w: 16, h: 16, payload: payload})
	img, err := decodeICO(data)
	if err != nil {
		t.Fatalf("decodeICO: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 16 || b.Dy() != 16 {
		t.Errorf("AND-mask height mishandled: got %dx%d, want 16x16", b.Dx(), b.Dy())
	}
}

// TestDecodeIconMalformedNoPanic: garbage inputs must produce errors, never
// panics.
func TestDecodeIconMalformedNoPanic(t *testing.T) {
	bads := [][]byte{
		{},
		{0x00},
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0x00, 0x00, 0x01, 0x00, 0x00, 0x00},
		{0x00, 0x00, 0x01, 0x00, 0x01, 0x01},
		[]byte("<html>not an image</html>"),
		[]byte("RIFF\x00\x00\x00\x00WEBPgarbage"),
		append(append([]byte{}, pngMagic...), 0xDE, 0xAD, 0xBE, 0xEF),
		testICO(icoSeedEntry{w: 16, h: 16, payload: []byte{0x01}}),
	}
	for _, bad := range bads {
		// A panic here fails the test — that is the assertion.
		_, _, _ = decodeIconImage(bad)
		_, _ = decodeICO(bad)
	}
}

// TestDecodeIconBombGuards: bomb inputs must return errors (bounded memory),
// never panic or OOM.
func TestDecodeIconBombGuards(t *testing.T) {
	bombs := map[string][]byte{
		"ico-bmp-stride-overflow": icoBMPBombBytes(),
		"png-ihdr-bomb":           pngBombBytes(100000, 100000),
		"gif-lsd-bomb":            gifBombBytes(60000, 60000),
		"jpeg-sof-bomb":           jpegBombBytes(20000, 20000),
		"ico-png-entry-bomb": testICO(icoSeedEntry{w: 16, h: 16,
			payload: pngBombBytes(100000, 100000)}),
	}
	for name, bomb := range bombs {
		t.Run(name, func(t *testing.T) {
			// A panic or runaway alloc here fails the test.
			if _, _, err := decodeIconImage(bomb); err == nil {
				t.Errorf("bomb input decoded without error")
			}
		})
	}
}

// ---------- fuzz targets ----------

// assertFuzzImage: decoded fuzzer output must respect the bomb cap, and the
// aHash path (used by getIcon on every fetched favicon) must not panic.
func assertFuzzImage(t *testing.T, img image.Image, format string) {
	t.Helper()
	if img == nil {
		t.Fatalf("nil image with nil error (format %q)", format)
	}
	b := img.Bounds()
	if b.Dx() > maxIconPixelDim || b.Dy() > maxIconPixelDim {
		t.Errorf("format %q decoded %dx%d, exceeds %dpx bomb cap", format, b.Dx(), b.Dy(), maxIconPixelDim)
	}
	_ = AverageHash(img)
}

// FuzzDecodeIcon: arbitrary bytes through the full decode path
// (stdlib PNG/GIF/JPEG + ICO fallback). Crash-free + bounded memory.
func FuzzDecodeIcon(f *testing.F) {
	for _, seed := range faviconFuzzSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		img, format, err := decodeIconImage(data)
		if err != nil {
			return
		}
		assertFuzzImage(t, img, format)
	})
}

// FuzzDecodeICO: arbitrary bytes at the hand-rolled ICO parser — directory
// walk, best-fit selection, PNG-entry branch, BMP 24/32-bit branch, AND
// mask height handling. Crash-free + bounded memory.
func FuzzDecodeICO(f *testing.F) {
	for _, seed := range faviconFuzzSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		img, err := decodeICO(data)
		if err != nil {
			return
		}
		assertFuzzImage(t, img, "ico")
	})
}
