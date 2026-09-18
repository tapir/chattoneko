package attach

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"
)

// ---- payloads ----

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// webmHeader is the EBML magic plus a "webm" DocType — enough for the sniffer,
// which never parses the container. mkvHeader is the same magic with the
// Matroska DocType, which the sniffer has to tell apart.
var (
	webmHeader = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x93, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}
	mkvHeader  = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x93, 0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}
)

// Headers for the formats the sniffer has to recognize. Each is only as long as
// classification needs — nothing here is decoded, so a payload never is.
var (
	gifHeader  = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00")
	bmpHeader  = append([]byte("BM"), make([]byte, 60)...)
	icoHeader  = []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10, 0x00, 0x00, 0x01, 0x00, 0x20, 0x00, 0x68, 0x04}
	wavHeader  = append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), make([]byte, 20)...)
	id3Header  = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 20)...)
	mp3Frame   = append([]byte{0xff, 0xfb, 0x90, 0x00}, make([]byte, 60)...) // sync, MPEG1 layer III, 128k/44.1k
	flacHeader = append([]byte("fLaC\x00\x00\x00\x22"), make([]byte, 40)...)
	oggHeader  = append([]byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00"), make([]byte, 30)...)
	pdfHeader  = []byte("%PDF-1.7 fake")
	zipHeader  = []byte{0x50, 0x4b, 0x03, 0x04, 0x00, 0x01}
	// Video and image formats the app does not support: an ISO-BMFF file and an
	// ISOBMFF photo brand, both of which a tool may still hand over.
	mp4Header  = append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), make([]byte, 20)...)
	avifHeader = append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1"), make([]byte, 20)...)
	tiffHeader = []byte{0x49, 0x49, 0x2a, 0x00, 0x08, 0x00, 0x00, 0x00}
)

// bareMP3Frame is an MP3 with no ID3 tag: a bare Xing frame, MPEG2 layer III.
// Go's sniffer misses it, which is why the tool path matches it by hand.
var bareMP3Frame = append([]byte{0xff, 0xf2, 0x58, 0xc4}, make([]byte, 60)...)

func sampleWebP(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile("testdata/sample.webp")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	return payload
}

// ---- upload classification: the extension decides, the bytes are converted ----

func TestClassifyMediaConverts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		filename    string
		data        []byte
		wantKind    string
		wantMime    string
		wantName    string
		wantExt     string
		wantConvert string
	}{
		{"png", "diagram.png", makePNG(t, 8, 6), KindImage, MimePNG, "diagram.png", ".png", ConvertImage},
		{"tga", "sprite.tga", []byte("not really a tga"), KindImage, MimePNG, "sprite.png", ".tga", ConvertImage},
		{"mp3", "memo.mp3", bareMP3Frame, KindFile, MimeMP3, "memo.mp3", ".mp3", ConvertAudio},
		{"video", "clip.mp4", mp4Header, KindFile, MimeMP3, "clip.mp3", ".mp4", ConvertAudio},
		{"pdf", "invoice.pdf", pdfHeader, KindFile, MimePDF, "invoice.pdf", "", ConvertNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Classify(tc.filename, tc.data, 1<<20)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if res.Kind != tc.wantKind || res.Mime != tc.wantMime || res.Name != tc.wantName {
				t.Fatalf("got kind=%q mime=%q name=%q, want %q/%q/%q",
					res.Kind, res.Mime, res.Name, tc.wantKind, tc.wantMime, tc.wantName)
			}
			if res.Ext != tc.wantExt || res.Convert != tc.wantConvert {
				t.Fatalf("got ext=%q convert=%q, want %q/%q",
					res.Ext, res.Convert, tc.wantExt, tc.wantConvert)
			}
		})
	}
}

// The stored name is the conversion's, not the upload's: whatever a picture or a
// recording arrived as, it lands as a .png or an .mp3. Text and PDF keep theirs.
func TestClassifyNameFollowsConversion(t *testing.T) {
	for _, tc := range []struct {
		filename string
		data     []byte
		want     string
	}{
		{"photo.jpeg", makeJPEG(t, 8, 6), "photo.png"},
		{"a.b.c.WEBP", sampleWebP(t), "a.b.c.png"},
		{"memo.webm", webmHeader, "memo.mp3"},
		{"invoice.txt", pdfHeader, "invoice.txt"}, // text wins: the CONTENT decides
		{"notes.md", []byte("# hi\n"), "notes.md"},
		{"paper.pdf", pdfHeader, "paper.pdf"},
	} {
		res, err := Classify(tc.filename, tc.data, 1<<20)
		if err != nil {
			t.Fatalf("Classify(%q): %v", tc.filename, err)
		}
		if res.Name != tc.want {
			t.Errorf("Classify(%q) name = %q, want %q", tc.filename, res.Name, tc.want)
		}
	}
}

// A tool's file is stored exactly as fetched: every supported media format
// becomes a preview under its own mime, and nothing is converted.
func TestProcessAnyToolMedia(t *testing.T) {
	jpg := makeJPEG(t, 8, 6)
	for _, tc := range []struct {
		name               string
		filename           string
		data               []byte
		wantKind, wantMime string
	}{
		{"jpeg", "photo.jpg", jpg, KindImage, MimeJPEG},
		{"jpeg keeps its spelling", "photo.jpeg", jpg, KindImage, MimeJPEG},
		{"misnamed jpeg", "photo", jpg, KindImage, MimeJPEG}, // gains .jpg
		{"gif", "anim.gif", gifHeader, KindImage, MimeGIF},
		{"bmp", "bitmap.bmp", bmpHeader, KindImage, MimeBMP},
		// ICO is not a supported image: no conversion path handles it, so it is
		// kept as the download the tool policy keeps any unknown binary as.
		{"ico is not an image", "favicon.ico", icoHeader, KindFile, "image/x-icon"},
		{"wav", "rec.wav", wavHeader, KindFile, MimeWAV},
		{"tagged mp3", "song.mp3", id3Header, KindFile, MimeMP3},
		{"bare-frame mp3", "song.mp3", mp3Frame, KindFile, MimeMP3},
		{"flac", "track.flac", flacHeader, KindFile, MimeFLAC},
		{"ogg", "track.ogg", oggHeader, KindFile, MimeOGG},
		{"opus", "voice.opus", oggHeader, KindFile, MimeOGG},
		{"webm", "rec.webm", webmHeader, KindFile, MimeAudio},
		{"pdf", "doc.pdf", pdfHeader, KindFile, MimePDF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ProcessAny(tc.filename, tc.data, 1<<20)
			if err != nil {
				t.Fatalf("ProcessAny: %v", err)
			}
			if res.Kind != tc.wantKind || res.Mime != tc.wantMime {
				t.Fatalf("got kind=%q mime=%q, want %q/%q", res.Kind, res.Mime, tc.wantKind, tc.wantMime)
			}
			if !bytes.Equal(res.Data, tc.data) || res.Size != int64(len(tc.data)) {
				t.Fatal("bytes must be stored verbatim")
			}
			// The name keeps whatever suffix already agrees with the bytes; only an
			// extension-less one gains a suffix.
			wantName := tc.filename
			if !strings.Contains(tc.filename, ".") {
				wantName = tc.filename + ExtForMime(tc.wantMime)
			}
			if res.Name != wantName {
				t.Fatalf("name = %q, want %q", res.Name, wantName)
			}
		})
	}
}

// A format no policy supports is still stored for a tool, as a download: the
// model found a file on the web and the user can have it whatever it is. Video
// containers are labelled as themselves, so the client shows a chip and not a
// player that cannot play them.
func TestProcessAnyKeepsUnsupportedAsDownload(t *testing.T) {
	for _, tc := range []struct {
		filename string
		data     []byte
		wantMime string
	}{
		{"clip.mp4", mp4Header, "video/mp4"},
		{"movie.mkv", mkvHeader, "video/x-matroska"},
		{"photo.avif", avifHeader, "application/octet-stream"},
		{"scan.tiff", tiffHeader, "application/octet-stream"},
		{"archive.zip", zipHeader, "application/zip"},
		{"photo.jpg", zipHeader, "application/zip"}, // a name that lies: the bytes win
	} {
		res, err := ProcessAny(tc.filename, tc.data, 1<<20)
		if err != nil {
			t.Fatalf("ProcessAny(%q): %v", tc.filename, err)
		}
		if res.Kind != KindFile || res.Mime != tc.wantMime {
			t.Errorf("ProcessAny(%q) = %s/%s, want file/%s", tc.filename, res.Kind, res.Mime, tc.wantMime)
		}
		if !bytes.Equal(res.Data, tc.data) {
			t.Errorf("ProcessAny(%q) changed the bytes", tc.filename)
		}
		// No preview: none of these is audio, so the client shows a download.
		if got := Type(res.Kind, res.Mime); got != KindFile {
			t.Errorf("Type(%q) = %q, want file", res.Mime, got)
		}
	}
	// Matroska shares the EBML magic with WebM: only the DocType tells them
	// apart, and getting it wrong would hand the client an unplayable player.
	if res, err := ProcessAny("rec.webm", webmHeader, 1<<20); err != nil || res.Mime != MimeAudio {
		t.Fatalf("webm = %+v, %v", res, err)
	}
}

// An upload is either on the extension list or text — nothing else is stored,
// whatever its bytes are. Formats no build of ffmpeg here decodes (tiff, ico,
// avif) are refused by the name rather than by a failed conversion.
func TestClassifyRejectsUnknown(t *testing.T) {
	for _, tc := range []struct {
		filename string
		data     []byte
	}{
		{"scan.tiff", tiffHeader},
		{"sticker.ico", icoHeader},
		{"photo.avif", avifHeader},
		{"archive.zip", zipHeader},
		{"notes", []byte{0x00, 0x01, 0xff}}, // extension-less and not text
		{"liar.png", zipHeader},             // accepted here; the conversion rejects it
	} {
		_, err := Classify(tc.filename, tc.data, 1<<20)
		want := ErrUnsupported
		if tc.filename == "liar.png" {
			want = nil
		}
		if want == nil {
			if err != nil {
				t.Errorf("Classify(%q) = %v, want the conversion to be the judge", tc.filename, err)
			}
			continue
		}
		if !errors.Is(err, want) {
			t.Errorf("Classify(%q) = %v, want ErrUnsupported", tc.filename, err)
		}
	}
	_, err := Classify("archive.zip", zipHeader, 1<<20)
	if !strings.Contains(err.Error(), Accepted) || strings.Contains(err.Error(), ToolAccepted) {
		t.Fatalf("error %q should name the upload list", err)
	}
}

// IsRasterImage is the gate on create_file's PNG conversion: the five formats
// it decodes, and nothing else — ICO in particular.
func TestIsRasterImage(t *testing.T) {
	jpg := makeJPEG(t, 8, 6)
	for _, tc := range []struct {
		name string
		data []byte
		want bool
	}{
		{"jpeg", jpg, true},
		{"png", makePNG(t, 8, 6), true},
		{"webp", sampleWebP(t), true},
		{"gif", gifHeader, true},
		{"bmp", bmpHeader, true},
		{"ico", icoHeader, false},
		{"wav", wavHeader, false},
		{"pdf", pdfHeader, false},
		{"text", []byte("hello"), false},
	} {
		if got := IsRasterImage(tc.data); got != tc.want {
			t.Errorf("IsRasterImage(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Only PNG may go to a model as an image. Everything else previews in the
// browser but takes the <file> reference path. History is rebuilt every
// turn, so a mime the provider rejects would break that chat permanently.
func TestSendsAsImage(t *testing.T) {
	for mime, want := range map[string]bool{
		MimePNG:  true,
		MimeWebP: false, MimeJPEG: false, MimeGIF: false, MimeBMP: false, "image/x-icon": false,
		MimePDF: false, MimeAudio: false, "": false,
	} {
		if got := SendsAsImage(mime); got != want {
			t.Errorf("SendsAsImage(%q) = %v, want %v", mime, got, want)
		}
	}
}

// A binary an upload refuses is still a download on the tool path: a file the
// model fetched reaches the user whatever it is.
func TestToolBinaryKeptAsDownload(t *testing.T) {
	junk := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	if _, err := Classify("x.bin", junk, 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("upload: want ErrUnsupported, got %v", err)
	}
	res, err := ProcessAny("x.bin", junk, 1<<20)
	if err != nil {
		t.Fatalf("ProcessAny: %v", err)
	}
	if res.Kind != KindFile || !bytes.Equal(res.Data, junk) {
		t.Fatalf("ProcessAny = %+v", res)
	}
}

func TestClassifyText(t *testing.T) {
	for _, tc := range []struct {
		filename, mime string
	}{
		{"notes.md", "text/markdown"},
		{"notes.markdown", "text/markdown"},
		{"data.json", "application/json"},
		{"log", "text/plain"},       // extension-less
		{"main.go", "text/plain"},   // an extension the table doesn't know
		{"weird.bin", "text/plain"}, // the CONTENT decides, not the name
	} {
		res, err := Classify(tc.filename, []byte("hello\n"), 1<<20)
		if err != nil {
			t.Fatalf("Classify(%q): %v", tc.filename, err)
		}
		if res.Kind != KindText || res.Mime != tc.mime || res.Name != tc.filename || res.Convert != ConvertNone {
			t.Fatalf("Classify(%q) = kind=%q mime=%q name=%q convert=%q, want text/%s",
				tc.filename, res.Kind, res.Mime, res.Name, res.Convert, tc.mime)
		}
	}
	// Binary-looking text (a NUL byte) is not text.
	if _, err := Classify("x.txt", []byte("a\x00b"), 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NUL byte accepted: %v", err)
	}
}

func TestClassifyTooLarge(t *testing.T) {
	payload := []byte("hello world")
	if _, err := Classify("x.txt", payload, 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// Media is measured after its conversion, so the stored cap does not apply
	// to the bytes that arrive: a 40 MB shot that converts to a 2 MB PNG is the
	// handler's to accept, and the raw ceiling is what bounds the upload.
	if _, err := Classify("pic.png", makePNG(t, 8, 6), 64); err != nil {
		t.Fatalf("image rejected before its conversion: %v", err)
	}
	if _, err := Classify("x.txt", payload, 1024); err != nil {
		t.Fatalf("rejected within limit: %v", err)
	}
	// maxBytes <= 0 means "only the raw ceiling".
	if _, err := Classify("x.txt", payload, 0); err != nil {
		t.Fatalf("no cap: %v", err)
	}
}

func TestClassifyRejectsEmpty(t *testing.T) {
	if _, err := Classify("empty.txt", nil, 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for empty file, got %v", err)
	}
}

func TestType(t *testing.T) {
	cases := []struct{ kind, mime, want string }{
		{KindImage, MimePNG, "image"},
		{KindImage, MimeWebP, "image"},
		{KindText, "text/markdown", "text"},
		{KindText, "application/json", "text"}, // the kind decides, not the mime
		{KindFile, MimeAudio, "audio"},
		{KindFile, "audio/mpeg", "audio"}, // a tool's recording: audio, but not one a specialist takes
		{KindFile, MimePDF, "document"},
		{KindFile, "application/zip", "file"},
		{KindFile, "video/mp4", "file"},
		{KindFile, "application/octet-stream", "file"},
	}
	for _, tc := range cases {
		if got := Type(tc.kind, tc.mime); got != tc.want {
			t.Errorf("Type(%q, %q) = %q, want %q", tc.kind, tc.mime, got, tc.want)
		}
	}
}

func TestSerializeText(t *testing.T) {
	out := SerializeText("we<\"ird.md", "att-123", "body text")
	if !strings.HasPrefix(out, `<file name=`) || !strings.Contains(out, `type="text"`) {
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

func TestSerializeRef(t *testing.T) {
	out := SerializeRef("we<\"ird.pdf", "att-9", KindFile, MimePDF, 42)
	for _, want := range []string{"att-9", MimePDF, "42 bytes", `type="document"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "we<\"ird.pdf") {
		t.Fatalf("filename not escaped: %s", out)
	}
	// The closing tag repeats the id, same envelope as SerializeText.
	if !strings.HasSuffix(out, "</file id=\"att-9\">") {
		t.Fatalf("bad envelope: %s", out)
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

// ExtForMime must agree with the mime Process derives from the extension it
// hands back, strip parameters, and stay quiet about types it doesn't know.
func TestExtForMime(t *testing.T) {
	cases := []struct{ ctype, want string }{
		{"application/json", ".json"},
		{"application/json; charset=utf-8", ".json"},
		{" TEXT/HTML ", ".html"},
		{"text/markdown", ".md"}, // shortest of md/markdown, stable across runs
		{"text/yaml", ".yml"},    // shortest of yaml/yml
		{MimePNG, ".png"},        // media are in the same table
		{MimeJPEG, ".jpg"},
		{MimeWebP, ".webp"},
		{MimeAudio, ".webm"},
		{MimeOGG, ".ogg"}, // shortest of ogg/opus
		{MimePDF, ".pdf"},
		{"video/mp4", ""},              // unsupported: no suffix is invented
		{"application/vnd.custom", ""}, // unknown: no invented suffix
		{"", ""},
		{"text/plain", ""},
	}
	for _, tc := range cases {
		if got := ExtForMime(tc.ctype); got != tc.want {
			t.Errorf("ExtForMime(%q) = %q, want %q", tc.ctype, got, tc.want)
		}
	}
	// Round trip: the extension we suggest must map back to the same mime,
	// otherwise a fetched file would be named for one type and stored as another.
	for _, mime := range textExts {
		if got := ExtForMime(mime); got != "" && textExts[strings.TrimPrefix(got, ".")] != mime {
			t.Errorf("%q -> %q -> a different mime", mime, got)
		}
	}
}

func TestIsText(t *testing.T) {
	if IsText(nil) {
		t.Fatal("empty data reported as text (callers reject it first)")
	}
	for _, yes := range []string{"hello", "a\tb\nc", "# markdown\n"} {
		if !IsText([]byte(yes)) {
			t.Errorf("%q should be text", yes)
		}
	}
	for _, no := range []string{"a\x00b", "\xff\xfe binary", strings.Repeat("\x01", 20)} {
		if IsText([]byte(no)) {
			t.Errorf("%q should not be text", no)
		}
	}
}
