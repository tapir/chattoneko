package tools

import (
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

// mp3Bytes opens with an ID3 tag, which is what Go's content sniffer needs to
// label the payload audio/mpeg (a bare frame header would do as well).
var mp3Bytes = []byte("ID3\x04\x00\x00\x00\x00\x00\x00\x00\x00")

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
	srv, recorded := speechServer(t, mp3Bytes)
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
	if string(f.data) != string(mp3Bytes) {
		t.Errorf("stored bytes = %q, want the provider's audio", f.data)
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
	srv, _ := speechServer(t, mp3Bytes)
	out, isErr := callSpecialist(t, &fakeFileStore{}, agentConfig(t, srv.URL, config.ModelsConfig{}),
		"speak", `{"text":"hi"}`, mcphub.CallMeta{ChatID: agentChat, MessageID: agentMsg})
	if !isErr {
		t.Fatalf("want an in-band refusal, got %q", out)
	}
	if !strings.Contains(out, "speech") {
		t.Fatalf("refusal = %q, want it to name the missing setting", out)
	}
}
