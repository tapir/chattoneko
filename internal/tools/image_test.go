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

// decodePNG asserts the bytes are a PNG of exactly w×h and returns the image.
func decodePNG(t *testing.T, data []byte, w, h int) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a decodable PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("output is %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}
	return img
}

func TestToPNG(t *testing.T) {
	t.Run("big png is capped and re-encoded", func(t *testing.T) {
		out, err := toPNG(encodePNG(t, 2000, 1000), true)
		if err != nil {
			t.Fatal(err)
		}
		decodePNG(t, out, 1280, 640)
	})

	t.Run("small png keeps its size", func(t *testing.T) {
		out, err := toPNG(encodePNG(t, 40, 30), true)
		if err != nil {
			t.Fatal(err)
		}
		decodePNG(t, out, 40, 30)
	})

	// A palette carries one alpha per colour, so a picture with soft edges is
	// left lossless and only a binary-alpha one is quantized.
	t.Run("soft alpha stays lossless", func(t *testing.T) {
		src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
		src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 0x40}) // translucent red
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatal(err)
		}
		out, err := toPNG(buf.Bytes(), true)
		if err != nil {
			t.Fatal(err)
		}
		img := decodePNG(t, out, 8, 8)
		if _, ok := img.(*image.Paletted); ok {
			t.Fatal("a translucent picture was quantized")
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a>>8 != 0x40 {
			t.Fatalf("alpha = %#x, want 0x40", a>>8)
		}
	})

	t.Run("binary alpha is quantized", func(t *testing.T) {
		src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
		for y := range 8 {
			for x := range 8 {
				src.SetNRGBA(x, y, color.NRGBA{uint8(x * 30), uint8(y * 30), 40, 255})
			}
		}
		src.SetNRGBA(0, 0, color.NRGBA{}) // fully transparent, not translucent
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatal(err)
		}
		out, err := toPNG(buf.Bytes(), true)
		if err != nil {
			t.Fatal(err)
		}
		pm, ok := decodePNG(t, out, 8, 8).(*image.Paletted)
		if !ok {
			t.Fatal("output is not an indexed PNG")
		}
		if len(pm.Palette) > paletteSize {
			t.Fatalf("palette has %d colours, want <= %d", len(pm.Palette), paletteSize)
		}
		if _, _, _, a := pm.At(0, 0).RGBA(); a != 0 {
			t.Fatal("the transparent pixel did not stay transparent")
		}
	})

	t.Run("quantization off stays lossless", func(t *testing.T) {
		out, err := toPNG(encodePNG(t, 40, 30), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := decodePNG(t, out, 40, 30).(*image.Paletted); ok {
			t.Fatal("a picture was quantized with the setting off")
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
		out, err := toPNG(buf.Bytes(), true)
		if err != nil {
			t.Fatal(err)
		}
		// Frame 2 leaves (0,0) black, so a white pixel proves only the first
		// frame survived.
		r, gg, b, _ := decodePNG(t, out, 20, 10).At(0, 0).RGBA()
		if r>>8 != 255 || gg>>8 != 255 || b>>8 != 255 {
			t.Fatalf("first frame not kept: (0,0) = %d,%d,%d", r>>8, gg>>8, b>>8)
		}
	})

	t.Run("undecodable is an error", func(t *testing.T) {
		if _, err := toPNG(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...), true); err == nil {
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
		_, err := toPNG(append([]byte("\x89PNG\r\n\x1a\n"), chunk...), true)
		if err == nil || !strings.Contains(err.Error(), "megapixel") {
			t.Fatalf("want a megapixel refusal, got %v", err)
		}
	})
}
