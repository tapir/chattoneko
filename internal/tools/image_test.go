package tools

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"

	"github.com/skrashevich/go-webp"
)

// scaleToFit mirrors media.js scaleToFit, so these are the numbers that
// function's own doc comment promises.
func TestScaleToFit(t *testing.T) {
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{100, 50, 100, 50},     // already small: never upscaled
		{1280, 900, 1280, 900}, // exactly at the cap: untouched
		{2000, 1000, 1280, 640},
		{5000, 3000, 2500, 1500}, // the 2x floor beats the 1280 cap
		{3000, 5000, 1500, 2500}, // portrait is capped by height
		{1281, 1000, 1280, 999},
	} {
		if w, h := scaleToFit(tc.w, tc.h, maxImageSide); w != tc.wantW || h != tc.wantH {
			t.Errorf("scaleToFit(%d, %d) = %dx%d, want %dx%d", tc.w, tc.h, w, h, tc.wantW, tc.wantH)
		}
	}
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 7), uint8(y * 11), 40, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// decodeWebP asserts the bytes are a WebP of exactly w×h and returns the image.
func decodeWebP(t *testing.T, data []byte, w, h int) image.Image {
	t.Helper()
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a decodable WebP: %v", err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("output is %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}
	return img
}

func TestToWebP(t *testing.T) {
	t.Run("big png is capped and re-encoded", func(t *testing.T) {
		out, err := toWebP(encodePNG(t, 2000, 1000))
		if err != nil {
			t.Fatal(err)
		}
		decodeWebP(t, out, 1280, 640)
	})

	t.Run("small png keeps its size", func(t *testing.T) {
		out, err := toWebP(encodePNG(t, 40, 30))
		if err != nil {
			t.Fatal(err)
		}
		decodeWebP(t, out, 40, 30)
	})

	t.Run("transparency survives", func(t *testing.T) {
		src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
		src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 0x40}) // translucent red
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatal(err)
		}
		out, err := toWebP(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, a := decodeWebP(t, out, 8, 8).At(0, 0).RGBA(); a == 0xffff {
			t.Fatal("alpha was flattened to opaque")
		}
	})

	// A GIF's later frames are dropped: image.Decode composes the first one
	// only, which is the documented behaviour.
	t.Run("gif keeps its first frame", func(t *testing.T) {
		g := &gif.GIF{
			Image: []*image.Paletted{
				image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.Black, color.White}),
				image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.Black, color.White}),
			},
			Delay: []int{10, 10},
		}
		g.Image[0].SetColorIndex(0, 0, 1) // frame 1 is white at (0,0)
		var buf bytes.Buffer
		if err := gif.EncodeAll(&buf, g); err != nil {
			t.Fatal(err)
		}
		out, err := toWebP(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		// Frame 2 leaves (0,0) black, so a white pixel proves only the first
		// frame survived.
		r, gg, b, _ := decodeWebP(t, out, 20, 10).At(0, 0).RGBA()
		if r>>8 != 255 || gg>>8 != 255 || b>>8 != 255 {
			t.Fatalf("first frame not kept: (0,0) = %d,%d,%d", r>>8, gg>>8, b>>8)
		}
	})

	t.Run("undecodable is an error", func(t *testing.T) {
		if _, err := toWebP(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)); err == nil {
			t.Fatal("want an error for a PNG header with no payload")
		}
	})

	t.Run("decode bomb is refused before pixels", func(t *testing.T) {
		// A valid PNG header claiming 65535x65535 — 16 GiB of raster — and no
		// pixel data at all. Reading the config must be enough to refuse it.
		ihdr := append([]byte("IHDR"), 0, 0, 0xff, 0xff, 0, 0, 0xff, 0xff, 8, 2, 0, 0, 0)
		chunk := binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)-4))
		chunk = append(chunk, ihdr...)
		chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(ihdr))
		_, err := toWebP(append([]byte("\x89PNG\r\n\x1a\n"), chunk...))
		if err == nil || !strings.Contains(err.Error(), "megapixel") {
			t.Fatalf("want a megapixel refusal, got %v", err)
		}
	})
}
