package attach

import (
	"errors"
	"strings"
	"testing"
)

// junk is a payload no text heuristic accepts, which is all a binary needs to
// be here: classification reads the extension, and the only bytes it looks at
// are IsText's.
var junk = []byte{0x00, 0x01, 0x02, 0xff, 0xfe}

// ---- classification: the extension decides, the bytes are converted ----

func TestClassifyMediaConverts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		filename    string
		wantKind    string
		wantMime    string
		wantName    string
		wantExt     string
		wantConvert string
	}{
		{"png", "diagram.png", KindImage, MimeJPEG, "diagram.jpg", ".png", ConvertImage},
		{"tga", "sprite.tga", KindImage, MimeJPEG, "sprite.jpg", ".tga", ConvertImage},
		{"mp3", "memo.mp3", KindFile, MimeMP3, "memo.mp3", ".mp3", ConvertAudio},
		{"video", "clip.mp4", KindFile, MimeMP3, "clip.mp3", ".mp4", ConvertAudio},
		{"pdf", "invoice.pdf", KindFile, MimePDF, "invoice.pdf", "", ConvertNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Classify(tc.filename, junk, 1<<20)
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
// recording arrived as, it lands as a .jpg or an .mp3. Text and PDF keep theirs.
// The payload is irrelevant — that is the whole point of classifying by name —
// so every case uses text bytes.
func TestClassifyNameFollowsConversion(t *testing.T) {
	for _, tc := range []struct {
		filename string
		want     string
	}{
		{"photo.jpeg", "photo.jpg"},
		{"a.b.c.WEBP", "a.b.c.jpg"},
		{"memo.webm", "memo.mp3"},
		{"invoice.pdf", "invoice.pdf"},
		{"notes.md", "notes.md"},
	} {
		res, err := Classify(tc.filename, []byte("# hi\n"), 1<<20)
		if err != nil {
			t.Fatalf("Classify(%q): %v", tc.filename, err)
		}
		if res.Name != tc.want {
			t.Errorf("Classify(%q) name = %q, want %q", tc.filename, res.Name, tc.want)
		}
	}
}

// An upload is either on the extension list or text — nothing else is stored,
// whatever its bytes are. Formats no build of ffmpeg here decodes (tiff, ico,
// avif) are refused by the name rather than by a failed conversion.
func TestClassifyRejectsUnknown(t *testing.T) {
	for _, filename := range []string{"scan.tiff", "sticker.ico", "photo.avif", "archive.zip", "notes"} {
		if _, err := Classify(filename, junk, 1<<20); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Classify(%q) = %v, want ErrUnsupported", filename, err)
		}
	}
	_, err := Classify("archive.zip", junk, 1<<20)
	if !strings.Contains(err.Error(), Accepted) {
		t.Fatalf("error %q should name the accepted list", err)
	}
	// A name that lies is the conversion's to reject, not the classifier's.
	if _, err := Classify("liar.png", junk, 1<<20); err != nil {
		t.Errorf("Classify(liar.png) = %v, want the conversion to be the judge", err)
	}
}

// The tool path keeps what an upload refuses: a file the model fetched reaches
// the user whatever it is, verbatim and under the mime the attachment handler
// serves every non-audio binary as anyway. Media and text take the same road an
// upload gives them.
func TestClassifyAnyKeepsUnknownAsDownload(t *testing.T) {
	for _, name := range []string{"archive.zip", "app.exe", "data", "scan.tiff"} {
		res, err := ClassifyAny(name, junk, 1<<20)
		if err != nil {
			t.Fatalf("ClassifyAny(%q): %v", name, err)
		}
		if res.Kind != KindFile || res.Mime != MimeBinary || res.Name != name || res.Convert != ConvertNone {
			t.Errorf("ClassifyAny(%q) = %+v", name, res)
		}
	}
	if res, err := ClassifyAny("photo.jpg", junk, 1<<20); err != nil ||
		res.Convert != ConvertImage || res.Mime != MimeJPEG || res.Name != "photo.jpg" {
		t.Errorf("ClassifyAny(photo.jpg) = %+v, %v", res, err)
	}
	if res, err := ClassifyAny("notes.md", []byte("# hi\n"), 1<<20); err != nil || res.Kind != KindText {
		t.Errorf("ClassifyAny(notes.md) = %+v, %v", res, err)
	}
	// The two refusals that survive: an empty file is nobody's to keep, and the
	// stored cap still applies to bytes stored verbatim.
	if _, err := ClassifyAny("empty.zip", nil, 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Errorf("empty file kept as a download: %v", err)
	}
	if _, err := ClassifyAny("big.zip", junk, 2); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize download accepted: %v", err)
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
	// to the bytes that arrive: a 40 MB shot that converts to a 2 MB JPEG is the
	// caller's to accept, and the raw ceiling is what bounds the upload.
	if _, err := Classify("pic.png", payload, 4); err != nil {
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

// Only JPEG may go to a model as an image. Everything else previews in the
// browser but takes the <file> reference path. History is rebuilt every
// turn, so a mime the provider rejects would break that chat permanently.
func TestSendsAsImage(t *testing.T) {
	for mime, want := range map[string]bool{
		MimeJPEG: true,
		// A row stored before every picture became a JPEG, and anything a
		// browser can render but a provider may not.
		"image/png": false, "image/webp": false, "image/gif": false,
		"image/bmp": false, "image/x-icon": false,
		MimePDF: false, MimeMP3: false, MimeBinary: false, "": false,
	} {
		if got := SendsAsImage(mime); got != want {
			t.Errorf("SendsAsImage(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestType(t *testing.T) {
	cases := []struct{ kind, mime, want string }{
		{KindImage, MimeJPEG, "image"},
		{KindImage, "image/png", "image"}, // a row from before every picture was a JPEG
		{KindText, "text/markdown", "text"},
		{KindText, "application/json", "text"}, // the kind decides, not the mime
		{KindFile, MimeMP3, "audio"},
		{KindFile, "audio/ogg", "audio"},
		{KindFile, MimePDF, "document"},
		{KindFile, MimeBinary, "file"},
		{KindFile, "video/mp4", "file"},
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

// ExtForMime gives an extension-less download the suffix a served Content-Type
// implies — which is now what decides the file, so a body served as image/jpeg
// becomes a picture only because this hands it a ".jpg". Parameters are
// stripped, and a type the table does not know invents nothing.
func TestExtForMime(t *testing.T) {
	cases := []struct{ ctype, want string }{
		{"application/json", ".json"},
		{"application/json; charset=utf-8", ".json"},
		{" TEXT/HTML ", ".html"},
		{"text/markdown", ".md"},
		{"text/yaml", ".yaml"},
		{"image/png", ".png"},
		{MimeJPEG, ".jpg"},
		{"image/webp", ".webp"},
		{"image/x-tga", ".tga"},
		{"audio/mpeg", ".mp3"},
		{"audio/x-wav", ".wav"},
		{"audio/ogg", ".ogg"},
		{"audio/mp4", ".m4a"},
		{"video/mp4", ".mp4"}, // a fetched video is on the audio list: its soundtrack survives
		{"video/x-matroska", ".mkv"},
		{MimePDF, ".pdf"},
		{"image/avif", ""},             // not on the list: no suffix is invented
		{"application/vnd.custom", ""}, // unknown: no invented suffix
		{"", ""},
		{"text/plain", ""},
	}
	for _, tc := range cases {
		if got := ExtForMime(tc.ctype); got != tc.want {
			t.Errorf("ExtForMime(%q) = %q, want %q", tc.ctype, got, tc.want)
		}
	}
	// Every text mime textExts knows has a suffix here, and that suffix maps
	// back to the same mime: a fetched file must not be named for one type and
	// stored as another.
	for ext, mime := range textExts {
		if got := ExtForMime(mime); got == "" {
			t.Errorf("%q (%s) has no suffix", mime, ext)
		} else if textExts[strings.TrimPrefix(got, ".")] != mime {
			t.Errorf("%q -> %q -> a different mime", mime, got)
		}
	}
	// And every media suffix it hands out is one Classify accepts.
	for _, ctype := range []string{"image/jpeg", "audio/x-wav", "video/mp4", "video/x-matroska"} {
		ext := strings.TrimPrefix(ExtForMime(ctype), ".")
		if !imageExts[ext] && !audioExts[ext] && ext != "pdf" {
			t.Errorf("%q -> .%s, which Classify does not accept", ctype, ext)
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
