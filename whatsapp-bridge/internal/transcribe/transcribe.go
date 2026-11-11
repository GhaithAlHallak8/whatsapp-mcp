package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	Marker               = "[transcript"
	ProviderWhisper      = "whisper_cpp"
	ProviderOpenAI       = "openai_compatible"
	ProviderElevenLabs   = "elevenlabs"
	DefaultElevenLabsURL = "https://api.elevenlabs.io/v1/speech-to-text"
	DefaultElevenModel   = "scribe_v2"
	requestTimeout       = 300 * time.Second
	decodeTimeout        = 60 * time.Second
	maxResponseBytes     = 8 << 20
)

type Engine interface {
	Transcribe(ctx context.Context, path string) (string, error)
	Source() string
}

func Prefix(e Engine) string {
	return fmt.Sprintf("%s (%s)] ", Marker, e.Source())
}

func Normalise(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func FromEnv(getenv func(string) string) (Engine, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }
	provider := get("WHATSAPP_TRANSCRIPTION_PROVIDER")
	if provider == "" {
		provider = ProviderWhisper
	}
	language := get("WHATSAPP_TRANSCRIPTION_LANGUAGE")
	if language == "auto" {
		language = ""
	}
	switch provider {
	case ProviderWhisper:
		model := get("WHISPER_MODEL")
		if model == "" {
			return nil, errors.New("set WHISPER_MODEL to a whisper.cpp model file")
		}
		if strings.HasPrefix(model, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				model = filepath.Join(home, model[2:])
			}
		}
		if info, err := os.Stat(model); err != nil || info.IsDir() {
			return nil, fmt.Errorf("WHISPER_MODEL does not point at a file: %s", model)
		}
		lang := get("WHISPER_LANGUAGE")
		if lang == "" {
			lang = "auto"
		}
		return &Whisper{Model: model, Language: lang, Run: runCommand}, nil
	case ProviderOpenAI:
		model := get("WHATSAPP_TRANSCRIPTION_MODEL")
		if model == "" {
			return nil, errors.New("set WHATSAPP_TRANSCRIPTION_MODEL to the model ID served by your endpoint")
		}
		endpoint, err := validateEndpoint(get("WHATSAPP_TRANSCRIPTION_URL"))
		if err != nil {
			return nil, err
		}
		return &OpenAICompatible{URL: endpoint, Model: model, Language: language, APIKey: get("WHATSAPP_TRANSCRIPTION_API_KEY"), Client: newHTTPClient()}, nil
	case ProviderElevenLabs:
		key := get("ELEVENLABS_API_KEY")
		if key == "" {
			return nil, errors.New("set ELEVENLABS_API_KEY to use the elevenlabs transcription provider")
		}
		model := get("WHATSAPP_TRANSCRIPTION_MODEL")
		if model == "" {
			model = DefaultElevenModel
		}
		endpoint := get("WHATSAPP_TRANSCRIPTION_URL")
		if endpoint == "" {
			endpoint = DefaultElevenLabsURL
		}
		endpoint, err := validateEndpoint(endpoint)
		if err != nil {
			return nil, err
		}
		return &ElevenLabs{URL: endpoint, Model: model, Language: language, APIKey: key, Client: newHTTPClient()}, nil
	}
	return nil, fmt.Errorf("WHATSAPP_TRANSCRIPTION_PROVIDER must be %s, %s or %s", ProviderWhisper, ProviderOpenAI, ProviderElevenLabs)
}

func validateEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("set WHATSAPP_TRANSCRIPTION_URL to a full HTTP(S) transcription endpoint without credentials")
	}
	return raw, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: requestTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type CommandRunner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Whisper struct {
	Model    string
	Language string
	Run      CommandRunner
}

func (w *Whisper) Source() string {
	base := filepath.Base(w.Model)
	return "whisper " + strings.TrimSuffix(base, filepath.Ext(base))
}

func (w *Whisper) Transcribe(ctx context.Context, path string) (string, error) {
	workDir, err := os.MkdirTemp("", "whatsapp-transcribe-")
	if err != nil {
		return "", fmt.Errorf("could not create work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	wav := filepath.Join(workDir, "audio.wav")

	decodeCtx, cancel := context.WithTimeout(ctx, decodeTimeout)
	_, stderr, err := w.Run(decodeCtx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", path, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav)
	cancel()
	if err != nil {
		return "", fmt.Errorf("could not decode the audio: %s", detail(stderr, err))
	}

	runCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	stdout, stderr, err := w.Run(runCtx, "whisper-cli", "-m", w.Model, "-l", w.Language, "-nt", "-f", wav)
	cancel()
	if err != nil {
		return "", fmt.Errorf("transcription failed: %s", detail(stderr, err))
	}
	text := Normalise(string(stdout))
	if text == "" {
		return "", errors.New("transcription produced no text")
	}
	return text, nil
}

func detail(stderr []byte, err error) string {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		return err.Error()
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

type OpenAICompatible struct {
	URL      string
	Model    string
	Language string
	APIKey   string
	Client   *http.Client
}

func (o *OpenAICompatible) Source() string { return ProviderOpenAI + " " + o.Model }

func (o *OpenAICompatible) Transcribe(ctx context.Context, path string) (string, error) {
	fields := map[string]string{"model": o.Model, "response_format": "json"}
	if o.Language != "" {
		fields["language"] = o.Language
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	return postAudio(ctx, o.Client, o.URL, path, fields, headers)
}

type ElevenLabs struct {
	URL      string
	Model    string
	Language string
	APIKey   string
	Client   *http.Client
}

func (e *ElevenLabs) Source() string { return ProviderElevenLabs + " " + e.Model }

func (e *ElevenLabs) Transcribe(ctx context.Context, path string) (string, error) {
	fields := map[string]string{"model_id": e.Model, "tag_audio_events": "false"}
	if e.Language != "" {
		fields["language_code"] = e.Language
	}
	return postAudio(ctx, e.Client, e.URL, path, fields, map[string]string{"xi-api-key": e.APIKey})
}

func postAudio(ctx context.Context, client *http.Client, endpoint, path string, fields, headers map[string]string) (string, error) {
	audio, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("audio file not found: %s", path)
	}
	defer func() { _ = audio.Close() }()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return "", errors.New("could not build transcription request")
		}
	}
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", errors.New("could not build transcription request")
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", errors.New("could not read the audio file")
	}
	if err := writer.Close(); err != nil {
		return "", errors.New("could not build transcription request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", errors.New("could not build transcription request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("transcription request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("transcription endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return "", errors.New("transcription endpoint returned invalid JSON")
	}
	text := Normalise(payload.Text)
	if text == "" {
		return "", errors.New("transcription endpoint returned no non-empty text field")
	}
	return text, nil
}
