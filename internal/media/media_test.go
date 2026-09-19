package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The conversions are the whole package, so these run ffmpeg for real. A
// machine without one skips rather than fails: the binary is the ffmpeg build
// in the image and any system ffmpeg in development.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(ffmpegBin); err != nil {
		t.Skipf("no %s on PATH", ffmpegBin)
	}
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 11), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// makeTGA writes an uncompressed 24-bit true-colour TGA. TGA has no magic bytes
// and ffmpeg has no parser for it, so the ".tga" suffix on the temp file is the
// only thing that reaches the decoder — this is what proves it survives.
func makeTGA(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	head := make([]byte, 18)
	head[2] = 2 // uncompressed true-colour
	head[16] = 24
	binary.LittleEndian.PutUint16(head[12:], uint16(w))
	binary.LittleEndian.PutUint16(head[14:], uint16(h))
	buf.Write(head)
	for range w * h {
		buf.Write([]byte{200, 100, 50}) // BGR
	}
	return buf.Bytes()
}

// makeWAV writes a quarter second of 8 kHz mono tone: the smallest input the
// audio path accepts.
func makeWAV(t *testing.T) []byte {
	t.Helper()
	const rate, secs = 8000, 0.25
	samples := int(rate * secs)
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+samples*2))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16) // PCM chunk size
	binary.LittleEndian.PutUint16(data[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(data[22:], 1)  // mono
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2) // block align
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(samples*2))
	for i := range samples {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/rate))
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(v))
	}
	return data
}

func TestImageToPNG(t *testing.T) {
	requireFFmpeg(t)
	out, err := toPNG(context.Background(), ".png", makePNG(t, 60, 40))
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a PNG: %v", err)
	}
	if cfg.Width != 60 || cfg.Height != 40 {
		t.Errorf("output is %dx%d, want 60x40 (a small picture is never upscaled)", cfg.Width, cfg.Height)
	}
}

// The cap is on the long side, which for a portrait picture is its height:
// clamping the width alone left tall pictures uncapped.
func TestScaleCapsLongSide(t *testing.T) {
	requireFFmpeg(t)
	for _, tc := range []struct{ w, h, maxW, maxH int }{
		{40, 2000, 1080, 1080},
		{20, 30, 20, 30}, // a small portrait is never upscaled
	} {
		out, err := toPNG(context.Background(), ".png", makePNG(t, tc.w, tc.h))
		if err != nil {
			t.Fatalf("toPNG(%dx%d): %v", tc.w, tc.h, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("output is not a PNG: %v", err)
		}
		if cfg.Width > tc.maxW || cfg.Height > tc.maxH {
			t.Errorf("a %dx%d picture came out %dx%d, want at most %dx%d",
				tc.w, tc.h, cfg.Width, cfg.Height, tc.maxW, tc.maxH)
		}
	}
}

// The extension, not the bytes, is what reaches a TGA: ffmpeg has no parser for
// it at all.
func TestTGAByExtension(t *testing.T) {
	requireFFmpeg(t)
	out, err := toPNG(context.Background(), ".tga", makeTGA(t, 12, 9))
	if err != nil {
		t.Fatalf("toPNG(.tga): %v", err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a PNG: %v", err)
	}
	if cfg.Width != 12 || cfg.Height != 9 {
		t.Errorf("output is %dx%d, want 12x9", cfg.Width, cfg.Height)
	}
}

func TestAudioToMP3(t *testing.T) {
	requireFFmpeg(t)
	out, err := toMP3(context.Background(), ".wav", makeWAV(t))
	if err != nil {
		t.Fatalf("Audio: %v", err)
	}
	// An MP3 opens with an ID3 tag or a bare frame sync; lame writes the latter.
	if !bytes.HasPrefix(out, []byte("ID3")) && !(out[0] == 0xff && out[1]&0xe0 == 0xe0) {
		t.Fatalf("output opens %x, want an MP3 tag or frame sync", out[:2])
	}
}

// A file that lies about its extension is the conversion's to reject: the name
// decides what ffmpeg is asked for, and the bytes decide whether it works.
func TestRejectsUndecodable(t *testing.T) {
	requireFFmpeg(t)
	zip := []byte("PK\x03\x04 not a picture at all")
	if _, err := toPNG(context.Background(), ".png", zip); err == nil {
		t.Fatal("a zip named .png converted")
	}
	if _, err := toMP3(context.Background(), ".mp3", zip); err == nil {
		t.Fatal("a zip named .mp3 converted")
	}
}

// The point of the package: nothing it touches survives the call. The work
// directory is a private one, because `go test ./...` runs packages in parallel
// and every package that converts writes into the same TMPDIR.
func TestTempFilesAreGone(t *testing.T) {
	requireFFmpeg(t)
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	if _, err := toPNG(ctx, ".png", makePNG(t, 8, 8)); err != nil {
		t.Fatalf("Image: %v", err)
	}
	if _, err := toPNG(ctx, ".png", []byte("PK\x03\x04 junk")); err == nil {
		t.Fatal("junk converted")
	}
	entries, err := os.ReadDir(workDir())
	if err != nil {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("%d temp files left behind: %v", len(entries), names)
	}
}

// The user-facing half of a rejection: ffmpeg's diagnosis, not its internals.
func TestFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[png @ 0x5587a1] Invalid PNG signature.\ntrailing\n", "Invalid PNG signature."},
		{"[in#0 @ 0x7f00] Error opening input: Invalid data found", "Error opening input: Invalid data found"},
		{"Stream map '' matches no streams.\n", "Stream map '' matches no streams."},
		{"  \n", ""},
		{strings.Repeat("x", 300), strings.Repeat("x", 200) + "…"},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
