package attach

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"
)

// makePNG builds a solid-color PNG in memory.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{0, uint8(x), uint8(y), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func makeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h),
		[]color.Color{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetColorIndex(x, y, uint8(x%2))
		}
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

func TestProcessPNGtoPNG(t *testing.T) {
	payload := makePNG(t, 40, 30)
	res, err := Process("pic.png", payload, 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Kind != KindImage || res.Mime != "image/png" {
		t.Fatalf("kind=%q mime=%q", res.Kind, res.Mime)
	}
	img, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 30 {
		t.Fatalf("size mismatch: %v", img.Bounds())
	}
}

func TestProcessJPEGtoPNG(t *testing.T) {
	payload := makeJPEG(t, 30, 30)
	res, err := Process("photo.jpg", payload, 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Kind != KindImage {
		t.Fatalf("kind=%q", res.Kind)
	}
	_, format, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("re-encoded format=%q", format)
	}
}

func TestProcessGIFFirstFrame(t *testing.T) {
	payload := makeGIF(t, 20, 20)
	res, err := Process("anim.gif", payload, 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Kind != KindImage {
		t.Fatalf("kind=%q", res.Kind)
	}
}

func TestProcessWebP(t *testing.T) {
	payload, err := os.ReadFile("testdata/sample.webp")
	if err != nil {
		t.Skip("no webp fixture:", err)
	}
	res, err := Process("sample.webp", payload, 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Kind != KindImage {
		t.Fatalf("kind=%q", res.Kind)
	}
	img, format, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format != "png" {
		t.Fatalf("format=%q", format)
	}
	t.Logf("webp converted to png: %d bytes, %dx%d",
		len(res.Data), img.Bounds().Dx(), img.Bounds().Dy())
}

func TestProcessText(t *testing.T) {
	res, err := Process("notes.md", []byte("# hello\nsome text\n"), 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Kind != KindText {
		t.Fatalf("kind=%q", res.Kind)
	}
	if res.Mime != "text/markdown" {
		t.Fatalf("mime=%q", res.Mime)
	}
	// extension is only a hint: no-ext names default to text/plain
	res2, err := Process("Makefile", []byte("all:\n\techo hi\n"), 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res2.Kind != KindText || res2.Mime != "text/plain" {
		t.Fatalf("kind=%q mime=%q", res2.Kind, res2.Mime)
	}
}

func TestProcessRejectsBinary(t *testing.T) {
	// NUL bytes
	if _, err := Process("x.bin", []byte("abc\x00def"), 1<<20); err == nil {
		t.Fatal("accepted binary with NUL")
	}
	// invalid UTF-8
	if _, err := Process("y.txt", []byte{0xff, 0xfe, 0x01, 0x02}, 1<<20); err == nil {
		t.Fatal("accepted invalid utf-8")
	}
	// corrupt image that looks like png
	if _, err := Process("broken.png", []byte("\x89PNG\r\n\x1a\nbogus"), 1<<20); err == nil {
		t.Fatal("accepted corrupt png")
	}
}

func TestProcessTooLarge(t *testing.T) {
	payload := []byte("hello world")
	if _, err := Process("x.txt", payload, 4); err == nil {
		t.Fatal("accepted oversized file")
	}
	if _, err := Process("x.txt", payload, 1024); err != nil {
		t.Fatalf("rejected within limit: %v", err)
	}
}

func TestDownscale(t *testing.T) {
	payload := makePNG(t, 3000, 2000)
	res, err := Process("big.png", payload, 1<<30)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	img, _, err2 := image.Decode(bytes.NewReader(res.Data))
	if err2 != nil {
		t.Fatalf("decode: %v", err2)
	}
	b := img.Bounds()
	if b.Dx() > maxImageSide || b.Dy() > maxImageSide {
		t.Fatalf("not downscaled: %dx%d", b.Dx(), b.Dy())
	}
	// aspect ratio preserved
	if b.Dx() != 2048 {
		t.Fatalf("aspect wrong: %dx%d", b.Dx(), b.Dy())
	}
}

func TestSerializeText(t *testing.T) {
	out := SerializeText("we<\"ird.md", "att-123", "body text")
	if !strings.HasPrefix(out, "<file name=") {
		t.Fatalf("bad serialization: %q", out)
	}
	// The closer repeats the boundary id so a bare "</file>" inside file
	// content cannot terminate the block early.
	if !strings.HasSuffix(out, `</file id="att-123">`) {
		t.Fatalf("closer missing boundary id: %q", out)
	}
	if strings.Contains(out, `we<"ird.md`) {
		t.Fatalf("filename not escaped: %q", out)
	}
	if !strings.Contains(out, "\nbody text\n") {
		t.Fatalf("content missing: %q", out)
	}
}

func TestSerializeImageDescription(t *testing.T) {
	out := SerializeImageDescription("cat.png", "att-42", "a fluffy orange cat")
	// Same <file> envelope as text attachments, keeping the image's own
	// filename so the model associates the block with the attachment.
	if !strings.HasPrefix(out, `<file name="cat.png" id="att-42">`) {
		t.Fatalf("bad envelope: %q", out)
	}
	if !strings.HasSuffix(out, `</file id="att-42">`) {
		t.Fatalf("closer missing boundary id: %q", out)
	}
	// The header marks the block as an image description rather than file
	// content, and the description follows verbatim.
	if !strings.Contains(out, "[Detailed text description of the image file \"cat.png\"") {
		t.Fatalf("description header missing: %q", out)
	}
	if !strings.Contains(out, "\na fluffy orange cat") {
		t.Fatalf("description text missing: %q", out)
	}
}

// fakePNGHeader builds a minimal structurally-valid PNG (signature + IHDR
// only, no pixel data) claiming the given dimensions.
func fakePNGHeader(t testing.TB, w, h uint32) []byte {
	t.Helper()
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type: truecolor
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	writePNGChunk(&b, "IHDR", ihdr)
	return b.Bytes()
}

func writePNGChunk(b *bytes.Buffer, typ string, data []byte) {
	var lenb [4]byte
	binary.BigEndian.PutUint32(lenb[:], uint32(len(data)))
	b.Write(lenb[:])
	b.WriteString(typ)
	b.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var cb [4]byte
	binary.BigEndian.PutUint32(cb[:], crc.Sum32())
	b.Write(cb[:])
}

func TestProcessRejectsHugeDimensions(t *testing.T) {
	// "Decompression bomb": a few dozen bytes claiming 100k x 100k pixels
	// must be rejected from the header alone, without decoding any pixels.
	_, err := Process("bomb.png", fakePNGHeader(t, 100000, 100000), 1<<20)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestProcessRejectsCorruptPixels(t *testing.T) {
	// Header parses (DecodeConfig succeeds) but the pixel stream is garbage.
	payload := append(fakePNGHeader(t, 64, 64), 0xde, 0xad)
	_, err := Process("broken.png", payload, 1<<20)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "corrupt png") {
		t.Fatalf("want corrupt png, got %v", err)
	}
}

func TestProcessRejectsEmpty(t *testing.T) {
	if _, err := Process("empty.txt", nil, 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for empty file, got %v", err)
	}
}

func TestProcessSizeMatchesData(t *testing.T) {
	res, err := Process("pic.png", makePNG(t, 50, 40), 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.Size != int64(len(res.Data)) {
		t.Fatalf("size %d != len(data) %d", res.Size, len(res.Data))
	}
	res2, err := Process("notes.md", []byte("# hi\n"), 1<<20)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res2.Size != int64(len(res2.Data)) {
		t.Fatalf("size %d != len(data) %d", res2.Size, len(res2.Data))
	}
}

func TestCleanFilename(t *testing.T) {
	for _, bad := range []string{
		"", "   ", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb",
		"name" + strings.Repeat("x", 200),
		// Bidi control marks (extension-spoofing vector).
		"\u202eevil.pdf", "a\u200fb", "a\u2066b", "a\u202eb",
	} {
		if _, err := CleanFilename(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	got, err := CleanFilename("  notes.md ")
	if err != nil || got != "notes.md" {
		t.Fatalf("clean = %q, %v", got, err)
	}
	// ZWJ/ZWNJ are joiners used in emoji sequences, not direction spoofers:
	// they stay accepted.
	if _, err := CleanFilename("fam\u200dily.png"); err != nil {
		t.Fatalf("ZWJ name rejected: %v", err)
	}
}
