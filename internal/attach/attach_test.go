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

func sampleWebP(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile("testdata/sample.webp")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	return payload
}

// ---- classification: magic bytes only, nothing decoded, nothing converted ----

func TestProcessMediaStoredVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name           string
		filename       string
		data           []byte
		wantKind       string
		wantMime       string
		wantName       string
		allowedAsImage bool
	}{
		{"webp", "sticker.webp", sampleWebP(t), KindImage, MimeWebP, "sticker.webp", true},
		{"audio webm", "memo.webm", webmHeader, KindFile, MimeAudio, "memo.webm", false},
		{"pdf", "invoice.pdf", []byte("%PDF-1.7 fake"), KindFile, MimePDF, "invoice.pdf", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Process(tc.filename, tc.data, 1<<20)
			if err != nil {
				t.Fatalf("process: %v", err)
			}
			if res.Kind != tc.wantKind || res.Mime != tc.wantMime || res.Name != tc.wantName {
				t.Fatalf("got kind=%q mime=%q name=%q, want %q/%q/%q",
					res.Kind, res.Mime, res.Name, tc.wantKind, tc.wantMime, tc.wantName)
			}
			// Verbatim: the bytes come back untouched and the size agrees.
			if !bytes.Equal(res.Data, tc.data) || res.Size != int64(len(tc.data)) {
				t.Fatalf("data changed (%d -> %d bytes)", len(tc.data), res.Size)
			}
		})
	}
}

// The stored name follows the bytes, not the upload: a stale or lying client
// cannot keep WebP bytes stored under a .jpg name. Text is the exception — its
// mime comes FROM the extension.
func TestProcessNameFollowsMime(t *testing.T) {
	webp := sampleWebP(t)
	for _, tc := range []struct {
		filename string
		data     []byte
		want     string
	}{
		{"photo.jpg", webp, "photo.webp"},
		{"photo", webp, "photo.webp"},
		{"a.b.c.PNG", webp, "a.b.c.webp"}, // the bytes win over the name
		{"memo.mp3", webmHeader, "memo.webm"},
		{"invoice.txt", []byte("%PDF-1.7 fake"), "invoice.pdf"},
		{"notes.md", []byte("# hi\n"), "notes.md"},
	} {
		res, err := Process(tc.filename, tc.data, 1<<20)
		if err != nil {
			t.Fatalf("Process(%q): %v", tc.filename, err)
		}
		if res.Name != tc.want {
			t.Errorf("Process(%q) name = %q, want %q", tc.filename, res.Name, tc.want)
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

// An upload only ever carries what the browser produced, so anything else is
// refused there even when a tool may attach it: the client's conversion step is
// the rule, not an option. PNG included — a browser with no WebP encoder
// (Safari, every version) encodes one in WASM instead of sending the PNG
// toBlob() silently falls back to.
func TestProcessUploadStaysStrict(t *testing.T) {
	if _, err := Process("pic.png", makePNG(t, 8, 6), 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("png upload accepted: %v", err)
	}
	_, err := Process("photo.jpg", makeJPEG(t, 8, 6), 1<<20)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("jpeg upload accepted: %v", err)
	}
	if !strings.Contains(err.Error(), Accepted) || strings.Contains(err.Error(), ToolAccepted) {
		t.Fatalf("error %q should name the upload list", err)
	}
	for _, tc := range []struct {
		filename string
		data     []byte
	}{
		{"track.flac", flacHeader},
		{"song.mp3", id3Header},
		{"anim.gif", gifHeader},
		{"clip.mp4", mp4Header},
		{"movie.mkv", mkvHeader},
		{"archive.zip", zipHeader},
	} {
		if _, err := Process(tc.filename, tc.data, 1<<20); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Process(%q) = %v, want ErrUnsupported", tc.filename, err)
		}
	}
}

// IsRasterImage is the gate on create_file's WebP conversion: the five formats
// it decodes, and nothing else — ICO in particular, which the app dropped.
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

// Only PNG and WebP may go to a model as an image: WebP is what both conversion
// paths produce, and PNG is what a Safari browser stored before uploads became
// WebP-only. History is rebuilt every turn, so a mime the provider rejects would
// break that chat for good. Everything else previews in the browser but takes
// the <file> reference path.
func TestSendsAsImage(t *testing.T) {
	for mime, want := range map[string]bool{
		MimePNG: true, MimeWebP: true,
		MimeJPEG: false, MimeGIF: false, MimeBMP: false, "image/x-icon": false,
		MimePDF: false, MimeAudio: false, "": false,
	} {
		if got := SendsAsImage(mime); got != want {
			t.Errorf("SendsAsImage(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestProcessBinaryNeedsAllowList(t *testing.T) {
	junk := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	if _, err := Process("x.bin", junk, 1<<20); !errors.Is(err, ErrUnsupported) {
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

func TestProcessText(t *testing.T) {
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
		res, err := Process(tc.filename, []byte("hello\n"), 1<<20)
		if err != nil {
			t.Fatalf("Process(%q): %v", tc.filename, err)
		}
		if res.Kind != KindText || res.Mime != tc.mime || res.Name != tc.filename {
			t.Fatalf("Process(%q) = kind=%q mime=%q name=%q, want text/%s",
				tc.filename, res.Kind, res.Mime, res.Name, tc.mime)
		}
	}
	// Binary-looking text (a NUL byte) is not text.
	if _, err := Process("x.txt", []byte("a\x00b"), 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NUL byte accepted: %v", err)
	}
}

func TestProcessTooLarge(t *testing.T) {
	payload := []byte("hello world")
	if _, err := Process("x.txt", payload, 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// The cap applies to media too — nothing is downscaled to fit any more.
	if _, err := Process("pic.webp", sampleWebP(t), 64); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("image over the cap accepted: %v", err)
	}
	if _, err := Process("x.txt", payload, 1024); err != nil {
		t.Fatalf("rejected within limit: %v", err)
	}
	// maxBytes <= 0 means "only the raw ceiling".
	if _, err := Process("x.txt", payload, 0); err != nil {
		t.Fatalf("no cap: %v", err)
	}
}

func TestProcessRejectsEmpty(t *testing.T) {
	if _, err := Process("empty.txt", nil, 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported for empty file, got %v", err)
	}
}

func TestProcessSizeMatchesData(t *testing.T) {
	for _, tc := range []struct {
		filename string
		data     []byte
	}{
		{"pic.webp", sampleWebP(t)},
		{"notes.md", []byte("# hi\n")},
		{"memo.webm", webmHeader},
	} {
		res, err := Process(tc.filename, tc.data, 1<<20)
		if err != nil {
			t.Fatalf("process %q: %v", tc.filename, err)
		}
		if res.Size != int64(len(res.Data)) {
			t.Fatalf("%s: size %d != len(data) %d", tc.filename, res.Size, len(res.Data))
		}
	}
}

func TestAudioFormat(t *testing.T) {
	if got := AudioFormat(MimeAudio); got != "webm" {
		t.Fatalf("AudioFormat(%q) = %q, want webm", MimeAudio, got)
	}
	// Anything else — a recording a tool fetched — is "" so the agent tool
	// refuses it in-band instead of guessing a format the provider would misread.
	for _, mime := range []string{"audio/wav", "audio/mpeg", "audio/ogg", "", "image/png"} {
		if got := AudioFormat(mime); got != "" {
			t.Fatalf("AudioFormat(%q) = %q, want empty", mime, got)
		}
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
