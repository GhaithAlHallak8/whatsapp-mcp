package transcribe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func audioFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "note.ogg")
	if err := os.WriteFile(path, []byte("OggS-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestElevenLabsSendsDocumentedRequest(t *testing.T) {
	var gotKey, gotModel, gotLang, gotFile string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("xi-api-key")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotModel = r.FormValue("model_id")
		gotLang = r.FormValue("language_code")
		file, _, err := r.FormFile("file")
		if err == nil {
			buf := make([]byte, 32)
			n, _ := file.Read(buf)
			gotFile = string(buf[:n])
		}
		_, _ = w.Write([]byte(`{"language_code":"ar","language_probability":0.9,"text":"  مرحبا \n بالعالم ","words":[]}`))
	}))
	defer server.Close()

	engine, err := FromEnv(env(map[string]string{
		"WHATSAPP_TRANSCRIPTION_PROVIDER": "elevenlabs",
		"ELEVENLABS_API_KEY":              "key-123",
		"WHATSAPP_TRANSCRIPTION_URL":      server.URL,
		"WHATSAPP_TRANSCRIPTION_LANGUAGE": "ar",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text, err := engine.Transcribe(context.Background(), audioFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if text != "مرحبا بالعالم" {
		t.Fatalf("text = %q", text)
	}
	if gotKey != "key-123" || gotModel != "scribe_v2" || gotLang != "ar" || gotFile != "OggS-audio" {
		t.Fatalf("request = key %q model %q lang %q file %q", gotKey, gotModel, gotLang, gotFile)
	}
	if Prefix(engine) != "[transcript (elevenlabs scribe_v2)] " {
		t.Fatalf("prefix = %q", Prefix(engine))
	}
}

func TestOpenAICompatibleSendsBearerAndOmitsAutoLanguage(t *testing.T) {
	var gotAuth, gotModel string
	var hasLanguage bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		_, hasLanguage = r.MultipartForm.Value["language"]
		_, _ = w.Write([]byte(`{"text":"hello there"}`))
	}))
	defer server.Close()

	engine, err := FromEnv(env(map[string]string{
		"WHATSAPP_TRANSCRIPTION_PROVIDER": "openai_compatible",
		"WHATSAPP_TRANSCRIPTION_URL":      server.URL,
		"WHATSAPP_TRANSCRIPTION_MODEL":    "parakeet",
		"WHATSAPP_TRANSCRIPTION_LANGUAGE": "auto",
		"WHATSAPP_TRANSCRIPTION_API_KEY":  "tok",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text, err := engine.Transcribe(context.Background(), audioFile(t))
	if err != nil || text != "hello there" {
		t.Fatalf("text = %q err = %v", text, err)
	}
	if gotAuth != "Bearer tok" || gotModel != "parakeet" || hasLanguage {
		t.Fatalf("request = auth %q model %q language sent %v", gotAuth, gotModel, hasLanguage)
	}
	if Prefix(engine) != "[transcript (openai_compatible parakeet)] " {
		t.Fatalf("prefix = %q", Prefix(engine))
	}
}

func TestHTTPEngineErrorsHideResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("secret detail"))
	}))
	defer server.Close()
	engine := &ElevenLabs{URL: server.URL, Model: "scribe_v2", APIKey: "k", Client: newHTTPClient()}
	_, err := engine.Transcribe(context.Background(), audioFile(t))
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPEngineDoesNotFollowRedirects(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	engine := &OpenAICompatible{URL: server.URL, Model: "m", Client: newHTTPClient()}
	if _, err := engine.Transcribe(context.Background(), audioFile(t)); err == nil || hit {
		t.Fatalf("err = %v, redirect followed = %v", err, hit)
	}
}

func TestWhisperDecodesThenRecognises(t *testing.T) {
	var calls []string
	engine := &Whisper{Model: "/models/ggml-large-v3-turbo.bin", Language: "auto", Run: func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, name)
		if name == "whisper-cli" {
			return []byte(" hello\n world \n"), nil, nil
		}
		return nil, nil, nil
	}}
	text, err := engine.Transcribe(context.Background(), audioFile(t))
	if err != nil || text != "hello world" {
		t.Fatalf("text = %q err = %v", text, err)
	}
	if strings.Join(calls, ",") != "ffmpeg,whisper-cli" {
		t.Fatalf("calls = %v", calls)
	}
	if Prefix(engine) != "[transcript (whisper ggml-large-v3-turbo)] " {
		t.Fatalf("prefix = %q", Prefix(engine))
	}
}

func TestWhisperReportsDecodeFailure(t *testing.T) {
	engine := &Whisper{Model: "m.bin", Language: "auto", Run: func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
		return nil, []byte("bad stream"), errors.New("exit 1")
	}}
	if _, err := engine.Transcribe(context.Background(), audioFile(t)); err == nil || !strings.Contains(err.Error(), "bad stream") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromEnvRejectsBadConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown provider":   {"WHATSAPP_TRANSCRIPTION_PROVIDER": "nope"},
		"whisper no model":   {},
		"openai no model":    {"WHATSAPP_TRANSCRIPTION_PROVIDER": "openai_compatible", "WHATSAPP_TRANSCRIPTION_URL": "http://127.0.0.1:1/x"},
		"openai url creds":   {"WHATSAPP_TRANSCRIPTION_PROVIDER": "openai_compatible", "WHATSAPP_TRANSCRIPTION_MODEL": "m", "WHATSAPP_TRANSCRIPTION_URL": "http://u:p@host/x"},
		"elevenlabs no key":  {"WHATSAPP_TRANSCRIPTION_PROVIDER": "elevenlabs"},
		"elevenlabs bad url": {"WHATSAPP_TRANSCRIPTION_PROVIDER": "elevenlabs", "ELEVENLABS_API_KEY": "k", "WHATSAPP_TRANSCRIPTION_URL": "ftp://host"},
	}
	for name, values := range cases {
		if _, err := FromEnv(env(values)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
