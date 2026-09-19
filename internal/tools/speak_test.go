package tools

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"chattoneko/internal/config"
	"chattoneko/internal/mcphub"
)

// providerAudio is a quarter second of 8 kHz mono silence: a real, decodable
// recording. mp3 is what the provider is ASKED for, but whatever container it
// sends is normalized the same, because ffmpeg probes an audio file's content
// rather than its name — so a WAV body stored under "speech.mp3" still comes out
// as the mono MP3 every other recording is. Deliberately not an mp3: the fixture
// is what proves that.
func providerAudio() []byte {
	const samples = 2000
	b := make([]byte, 44+2*samples)
	copy(b, "RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00"+
		"\x40\x1f\x00\x00\x80\x3e\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	binary.LittleEndian.PutUint32(b[40:], 2*samples)
	return b
}

// isMP3 accepts either of the two shapes an MP3 opens with: an ID3 tag or a
// bare frame sync, which is what lame writes.
func isMP3(b []byte) bool {
	return bytes.HasPrefix(b, []byte("ID3")) ||
		(len(b) > 1 && b[0] == 0xff && b[1]&0xe0 == 0xe0)
}

// speechReq is what one /audio/speech call carried.
type speechReq struct{ path, model, input, voice, format string }

// speechServer answers POST /audio/speech with fixed audio bytes and records
// the last request.
func speechServer(t *testing.T, audio []byte) (*httptest.Server, func() speechReq) {
	t.Helper()
	var mu sync.Mutex
	var got speechReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model          string `json:"model"`
			Input          string `json:"input"`
			Voice          string `json:"voice"`
			ResponseFormat string `json:"response_format"`
		}
		_ = json.Unmarshal(body, &req)
		rec := speechReq{path: r.URL.Path, model: req.Model, input: req.Input, voice: req.Voice, format: req.ResponseFormat}
		mu.Lock()
		got = rec
		mu.Unlock()
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	t.Cleanup(srv.Close)
	return srv, func() speechReq {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// TestSpeakStoresRecording: the speak tool sends the words to /audio/speech
// under the designated model and voice, asks for mp3, and stores what comes
// back as an audio attachment on the assistant message — the same shape an
// uploaded recording has, so the chat's player handles it unchanged. The model
// needs no whitelist entry: a speech designation is a free-standing id.
func TestSpeakStoresRecording(t *testing.T) {
	srv, recorded := speechServer(t, providerAudio())
	fs := &fakeFileStore{}
	cfgs := agentConfig(t, srv.URL, config.ModelsConfig{DefaultSpeechModel: "tts", SpeechVoice: "alloy"})

	out, isErr := callSpecialist(t, fs, cfgs, "speak", `{"text":"Good morning."}`,
		mcphub.CallMeta{ChatID: agentChat, MessageID: agentMsg})
	if isErr {
		t.Fatalf("call failed: %s", out)
	}
	got := recorded()
	if got.path != "/audio/speech" {
		t.Errorf("POSTed to %q, want /audio/speech", got.path)
	}
	if got.model != "tts" || got.voice != "alloy" || got.format != "mp3" {
		t.Errorf("request = model %q voice %q format %q, want tts/alloy/mp3", got.model, got.voice, got.format)
	}
	if got.input != "Good morning." {
		t.Errorf("input = %q, want the words verbatim", got.input)
	}
	if len(fs.files) != 1 {
		t.Fatalf("stored %d files, want the one recording", len(fs.files))
	}
	f := fs.files[0]
	if f.kind != "file" || f.mime != "audio/mpeg" {
		t.Errorf("stored as %s/%s, want file/audio-mpeg", f.kind, f.mime)
	}
	// Not the provider's bytes: they went through the same conversion an
	// uploaded recording does, so a container the app cannot play still lands
	// as one it can.
	if !isMP3(f.data) {
		t.Errorf("stored bytes are %x…, want an MP3", f.data[:min(4, len(f.data))])
	}
	if len(fs.links) != 1 || fs.links[0] != [2]string{f.id, agentMsg} {
		t.Errorf("links = %v, want the recording shown on %s", fs.links, agentMsg)
	}
	if !strings.Contains(out, f.filename) {
		t.Errorf("result = %q, want it to name the recording", out)
	}
	// The id is the only handle on a file the model attached to its own reply:
	// without it a follow-up "transcribe that" has nothing to pass.
	if !strings.Contains(out, `id="`+f.id+`"`) {
		t.Errorf("result = %q, want it to carry the attachment id %s", out, f.id)
	}
}

// An unconfigured speech model is an in-band refusal, so the chat model can
// relay it and finish in text rather than the generation failing.
func TestSpeakWithoutModel(t *testing.T) {
	srv, _ := speechServer(t, providerAudio())
	out, isErr := callSpecialist(t, &fakeFileStore{}, agentConfig(t, srv.URL, config.ModelsConfig{}),
		"speak", `{"text":"hi"}`, mcphub.CallMeta{ChatID: agentChat, MessageID: agentMsg})
	if !isErr {
		t.Fatalf("want an in-band refusal, got %q", out)
	}
	if !strings.Contains(out, "speech") {
		t.Fatalf("refusal = %q, want it to name the missing setting", out)
	}
}

// TestSpeakReadsStoredFile: the `id` source reads a text file already in the
// chat, so the model pays no output tokens retyping it, and refuses everything
// that is not this chat's text.
func TestSpeakReadsStoredFile(t *testing.T) {
	srv, recorded := speechServer(t, providerAudio())
	const words = "Read me aloud."
	fs := &fakeFileStore{}
	seedAttachment(fs, "mine", agentChat, "notes.md", "text", "text/markdown", []byte(words))
	seedAttachment(fs, "take", agentChat, "take.mp3", "file", "audio/mpeg", []byte("ID3"))
	seedAttachment(fs, "theirs", "chat-2", "notes.md", "text", "text/markdown", []byte(words))
	seedAttachment(fs, "blank", agentChat, "blank.txt", "text", "text/plain", []byte("  \n"))
	cfgs := agentConfig(t, srv.URL, config.ModelsConfig{DefaultSpeechModel: "tts", SpeechVoice: "alloy"})
	meta := mcphub.CallMeta{ChatID: agentChat, MessageID: agentMsg}

	out, isErr := callSpecialist(t, fs, cfgs, "speak", `{"id":"mine"}`, meta)
	if isErr {
		t.Fatalf("call failed: %s", out)
	}
	if got := recorded().input; got != words {
		t.Errorf("input = %q, want the stored file's words", got)
	}
	if len(fs.links) != 1 || fs.links[0][1] != agentMsg {
		t.Errorf("links = %v, want the recording shown on %s", fs.links, agentMsg)
	}

	for _, args := range []string{
		`{"id":"take"}`, `{"id":"theirs"}`, `{"id":"nope"}`, `{"id":"  "}`, `{"id":"blank"}`,
		`{"text":"hi","id":"mine"}`, `{}`,
	} {
		out, isErr := callSpecialist(t, fs, cfgs, "speak", args, meta)
		if !isErr {
			t.Errorf("%s: want an in-band refusal, got %q", args, out)
		}
	}
	if len(fs.links) != 1 {
		t.Errorf("a refused call stored a recording: %v", fs.links)
	}
}
