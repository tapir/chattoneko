// Package media turns an uploaded picture or recording into the bytes the
// database stores: a PNG for every image, a mono MP3 for every audio file. It
// runs ffmpeg — the static build ffmpeg/ produces — over two temp files, because
// conversion needs a named input (TGA has no magic bytes, and an MP4 whose moov
// box sits at the end demuxes to nothing from a pipe) and a named output (an MP3
// written to stdout loses its Xing header).
//
// Both files are removed before the call returns, whatever it returned: nothing
// the app serves ever lives on the filesystem, only in the database.
package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"chattoneko/internal/attach"
)

// EnvBinary overrides the ffmpeg to run. The default is a plain PATH lookup:
// the image ships the ffmpeg build at /usr/local/bin/ffmpeg, a dev machine uses
// whatever ffmpeg it has.
const EnvBinary = "CHATTO_FFMPEG"

var ffmpegBin = func() string {
	if p := strings.TrimSpace(os.Getenv(EnvBinary)); p != "" {
		return p
	}
	return "ffmpeg"
}()

// Binary is the ffmpeg Prepare runs: $CHATTO_FFMPEG when set,
// otherwise a plain "ffmpeg" PATH lookup.
func Binary() string { return ffmpegBin }

// workDir is its own directory under TMPDIR so Sweep can empty it without
// touching anything else that lives there. Read per call rather than cached at
// init, so a test can point it at a private directory with t.Setenv.
func workDir() string { return filepath.Join(os.TempDir(), "chattoneko-media") }

// Sweep empties workDir. main calls it at startup, which is the whole janitor:
// a conversion killed mid-run (OOM, SIGKILL, deploy) never ran its defers, and
// this directory is ours alone.
func Sweep() error {
	dir := workDir()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

// ffmpeg/cli.md verbatim. One thread and no stdin keep a conversion from
// outliving the request that asked for it.
var (
	common = []string{"-nostdin", "-v", "error", "-y", "-threads", "1", "-filter_threads", "1", "-filter_complex_threads", "1"}
	// Caps the long side at 1920, or 1080 for a portrait picture, and never
	// upscales — hence clamping both dimensions, not just the width.
	scale = "scale=w='min(iw,if(gte(iw,ih),1920,1080))'" +
		":h='min(ih,if(gte(iw,ih),1920,1080))':force_original_aspect_ratio=decrease"
	// Bounds the decode side of a picture whose header claims absurd dimensions.
	maxPixels = []string{"-max_pixels", "33177600"}
)

// Prepare returns the bytes to store for one classified file: the conversion its
// verdict asks for, refused when the result outruns maxBytes. Every path into
// the database — an upload, create_file, speak — goes through this, so one set of
// invocations produces every attachment the app holds and a file stored under a
// media mime is always the shape that mime says.
func Prepare(ctx context.Context, f *attach.File, data []byte, quantize bool, maxBytes int64) ([]byte, error) {
	var err error
	switch f.Convert {
	case attach.ConvertImage:
		data, err = toPNG(ctx, f.Ext, data, quantize)
	case attach.ConvertAudio:
		data, err = toMP3(ctx, f.Ext, data)
	}
	if err != nil {
		return nil, err
	}
	// The stored cap is measured on what lands in the database, which for media
	// only exists now.
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, attach.ErrTooLarge
	}
	return data, nil
}

// toPNG converts one picture to a PNG — a 256-colour indexed one when quantize
// is the server's image_quantization setting. inExt is the input's own suffix,
// dot included, and must survive onto the temp file's name: ffmpeg has no TGA
// parser at all, so ".tga" is the only thing that reaches that decoder. An
// animated GIF, WebP or APNG keeps its first frame.
func toPNG(ctx context.Context, inExt string, data []byte, quantize bool) ([]byte, error) {
	post := []string{"-map", "0:V:0", "-vf", scale}
	if quantize {
		post = []string{"-lavfi", fmt.Sprintf(
			"[0:V]%s,trim=end_frame=1,palettegen[p];[0:V]%s[t];[t][p]paletteuse", scale, scale)}
	}
	return run(ctx, inExt, ".png", data, append(common, maxPixels...),
		append(post, "-frames:v", "1"))
}

// toMP3 converts one recording — or a video container's soundtrack, the picture
// is discarded — to a mono 22050 Hz 32 kbps MP3, the shape a transcription
// model resamples to anyway.
func toMP3(ctx context.Context, inExt string, data []byte) ([]byte, error) {
	return run(ctx, inExt, ".mp3", data, common,
		[]string{"-map", "0:a:0", "-map_metadata", "-1", "-fflags", "+bitexact",
			"-ac", "1", "-ar", "22050", "-b:a", "32k"})
}

// run stages the input under its own extension, converts it to a file staged
// under the output's, and hands back that file's bytes. pre goes before "-i",
// post between the input and the output.
func run(ctx context.Context, inExt, outExt string, data []byte, pre, post []string) ([]byte, error) {
	dir := workDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// CreateTemp replaces the "*" and keeps what follows it, so the suffix
	// lands on the name.
	in, err := os.CreateTemp(dir, "conv-*"+inExt)
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	out, err := os.CreateTemp(dir, "conv-*"+outExt)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	// Reserved for its name only: ffmpeg writes it, so the descriptor has to be
	// out of the way before the process starts.
	outName := out.Name()
	_ = out.Close()
	defer os.Remove(outName)

	if _, err := in.Write(data); err != nil {
		_ = in.Close()
		return nil, err
	}
	if err := in.Close(); err != nil {
		return nil, err
	}

	args := make([]string, 0, len(pre)+len(post)+4)
	args = append(args, pre...)
	args = append(args, "-i", in.Name())
	args = append(args, post...)
	args = append(args, outName)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpegBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// ffmpeg's own one-line diagnosis is the useful half, and a file that is
		// not what its name claimed is an unsupported media type: the name picks
		// the conversion, the bytes decide whether it works.
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", attach.ErrUnsupported, msg)
		}
		return nil, fmt.Errorf("%w: %v", attach.ErrUnsupported, err)
	}
	return readOut(outName)
}

// readOut returns the converted bytes. ffmpeg's output is bounded by its input
// in practice, but the input's cap is not the output's, so the raw ceiling is
// re-checked here rather than after the bytes are in memory.
func readOut(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case fi.Size() == 0:
		return nil, fmt.Errorf("%w: no %s came out", attach.ErrUnsupported, filepath.Ext(name))
	case fi.Size() > attach.MaxRawUploadBytes:
		return nil, attach.ErrTooLarge
	}
	return io.ReadAll(f)
}

// firstLine is ffmpeg's own diagnosis, which is what the user is told: the
// first stderr line, with its "[png @ 0x5587…]" component tag dropped (the
// address is noise) and a length cap.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "[") {
		if j := strings.Index(s, "] "); j >= 0 {
			s = s[j+2:]
		}
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
