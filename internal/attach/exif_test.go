package attach

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

// exifApp1 builds an APP1 EXIF segment holding only an Orientation tag in
// IFD0, in either TIFF byte order.
func exifApp1(t *testing.T, orient uint16, littleEndian bool) []byte {
	t.Helper()
	var bo binary.ByteOrder = binary.BigEndian
	bo2 := "MM"
	if littleEndian {
		bo = binary.LittleEndian
		bo2 = "II"
	}
	tiff := make([]byte, 8+2+12)
	copy(tiff[0:2], bo2)
	bo.PutUint16(tiff[2:4], 42) // TIFF magic
	bo.PutUint32(tiff[4:8], 8)  // IFD0 offset
	bo.PutUint16(tiff[8:10], 1) // one entry
	bo.PutUint16(tiff[10:12], 0x0112)
	bo.PutUint16(tiff[12:14], 3) // SHORT
	bo.PutUint32(tiff[14:18], 1) // count
	bo.PutUint16(tiff[18:20], orient)
	body := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1}
	var lenb [2]byte
	binary.BigEndian.PutUint16(lenb[:], uint16(len(body)+2)) // length includes itself
	seg = append(seg, lenb[:]...)
	return append(seg, body...)
}

// jpegWithOrientation rewrites a plain JPEG so it carries an EXIF
// orientation tag, inserted right after SOI like real cameras write it.
func jpegWithOrientation(t *testing.T, w, h int, orient uint16, littleEndian bool) []byte {
	t.Helper()
	jpg := makeJPEG(t, w, h)
	out := make([]byte, 0, len(jpg)+40)
	out = append(out, jpg[:2]...) // SOI
	out = append(out, exifApp1(t, orient, littleEndian)...)
	out = append(out, jpg[2:]...)
	return out
}

func TestJPEGOrientationParser(t *testing.T) {
	if got := jpegOrientation(makeJPEG(t, 8, 8)); got != 1 {
		t.Fatalf("no EXIF: got %d, want 1", got)
	}
	for _, le := range []bool{false, true} {
		for want := uint16(1); want <= 8; want++ {
			data := jpegWithOrientation(t, 8, 8, want, le)
			if got := jpegOrientation(data); got != int(want) {
				t.Fatalf("littleEndian=%v orient=%d: got %d", le, want, got)
			}
		}
	}
	// Garbage inputs must degrade to 1, never fail.
	for _, data := range [][]byte{
		nil, {}, {0xFF, 0xD8},
		{0x00, 0x01, 0x02},                        // not a JPEG
		{0xFF, 0xD8, 0xFF, 0xE1, 0x00},            // truncated segment length
		{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01},      // segLen < 2
		append(makeJPEG(t, 8, 8)[:2], 0xFF, 0xDA), // SOI + SOS, no EXIF
	} {
		if got := jpegOrientation(data); got != 1 {
			t.Fatalf("garbage % x: got %d, want 1", data[:min(len(data), 6)], got)
		}
	}
	// APP1 present but without Exif header: no orientation.
	seg := []byte{0xFF, 0xE1, 0x00, 0x08, 'X', 'x', 'i', 'f', 0, 0, 0, 0}
	data := append(append([]byte{0xFF, 0xD8}, seg...), makeJPEG(t, 8, 8)[2:]...)
	if got := jpegOrientation(data); got != 1 {
		t.Fatalf("non-EXIF APP1: got %d, want 1", got)
	}
}

func TestApplyOrientationNoop(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for _, orient := range []int{0, 1, 9, -1} {
		if got := applyOrientation(img, orient); got != img {
			t.Fatalf("orient %d: expected identity", orient)
		}
	}
}

// corners is a 3x2 image with a distinct color in each corner so every
// transform moves at least one known pixel to a checkable place.
func corners() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})     // red TL
	img.Set(2, 0, color.RGBA{0, 255, 0, 255})     // green TR
	img.Set(0, 1, color.RGBA{0, 0, 255, 255})     // blue BL
	img.Set(2, 1, color.RGBA{255, 255, 255, 255}) // white BR
	return img
}

func TestApplyOrientationCorners(t *testing.T) {
	red, green, blue, white :=
		color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255},
		color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 255, 255}

	// Expected output dimensions and the color that must land at (0,0).
	cases := []struct {
		orient  int
		w, h    int
		topLeft color.RGBA
	}{
		{1, 3, 2, red},   // identity
		{2, 3, 2, green}, // horizontal flip: TR -> TL
		{3, 3, 2, white}, // 180°: BR -> TL
		{4, 3, 2, blue},  // vertical flip: BL -> TL
		{5, 2, 3, red},   // transpose: TL stays
		{6, 2, 3, blue},  // 90° CW: BL -> TL
		{7, 2, 3, white}, // transverse: BR -> TL
		{8, 2, 3, green}, // 90° CCW: TR -> TL
	}
	for _, c := range cases {
		dst := applyOrientation(corners(), c.orient)
		b := dst.Bounds()
		if b.Dx() != c.w || b.Dy() != c.h {
			t.Fatalf("orient %d: dims %dx%d, want %dx%d", c.orient, b.Dx(), b.Dy(), c.w, c.h)
		}
		if got := dst.At(0, 0).(color.RGBA); got != c.topLeft {
			t.Fatalf("orient %d: (0,0)=%v, want %v", c.orient, got, c.topLeft)
		}
	}
}

// Full-pixel check of orientation 6 (90° CW), the orientation portrait
// phone photos almost always carry.
func TestApplyOrientationFullRotation(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			src.Set(x, y, color.RGBA{uint8(x * 10), uint8(y * 40), 7, 255})
		}
	}
	dst := applyOrientation(src, 6)
	b := dst.Bounds()
	if b.Dx() != 2 || b.Dy() != 3 {
		t.Fatalf("dims %v, want 2x3", b)
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 2; x++ {
			want := src.At(y, 1-x) // inverse of dst(x,y)=src(y, h-1-x)
			if got := dst.At(x, y); got != want {
				t.Fatalf("dst(%d,%d)=%v, want %v", x, y, got, want)
			}
		}
	}
}

// End to end: Process() must bake the EXIF orientation into the PNG it
// stores. A 40x30 JPEG tagged orientation 6 comes out 30x40, upright.
func TestProcessAppliesJPEGOrientation(t *testing.T) {
	res, err := Process("portrait.jpg", jpegWithOrientation(t, 40, 30, 6, false), 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 30 || b.Dy() != 40 {
		t.Fatalf("dims %dx%d, want 30x40 (orientation not applied)", b.Dx(), b.Dy())
	}

	// Control: same JPEG without the EXIF tag keeps 40x30.
	res2, err := Process("plain.jpg", makeJPEG(t, 40, 30), 1<<20)
	if err != nil {
		t.Fatalf("process plain: %v", err)
	}
	img2, _, err := image.Decode(bytes.NewReader(res2.Data))
	if err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	if b := img2.Bounds(); b.Dx() != 40 || b.Dy() != 30 {
		t.Fatalf("plain dims %dx%d, want 40x30", b.Dx(), b.Dy())
	}
}

// Downscaling commutes with the orientation transform: a large rotated JPEG
// must come out upright at the capped size no matter which runs first.
// 3000x4000 stored pixels tagged orientation 6 (rotate 90° CW for display)
// become 2048x1536 — landscape, bounded, upright.
func TestProcessLargeOrientedImageDownscaled(t *testing.T) {
	res, err := Process("big-portrait.jpg", jpegWithOrientation(t, 3000, 4000, 6, false), 1<<30)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 2048 || b.Dy() != 1536 {
		t.Fatalf("dims %dx%d, want 2048x1536 (downscale + rotate combined)", b.Dx(), b.Dy())
	}
}
