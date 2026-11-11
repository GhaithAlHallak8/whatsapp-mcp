package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

type fakeEngine struct {
	text     string
	failures int
	calls    int
}

func (f *fakeEngine) Source() string { return "fake model" }

func (f *fakeEngine) Transcribe(context.Context, string) (string, error) {
	f.calls++
	if f.calls <= f.failures {
		return "", errors.New("engine down")
	}
	return f.text, nil
}

func testTranscriber(t *testing.T, mode string, engine *fakeEngine) (*autoTranscriber, *MessageStore) {
	t.Helper()
	ms := newTestMessageStore(t)
	tr := newAutoTranscriber(autoTranscribeConfig{mode: mode, allow: map[string]bool{}, deny: map[string]bool{}}, engine, ms)
	tr.retryDelay = 0
	tr.logf = func(string, ...interface{}) {}
	return tr, ms
}

func storeAudio(t *testing.T, ms *MessageStore, id, content string, at time.Time) {
	t.Helper()
	chat := phonePN.String()
	if err := ms.StoreChat(chat, "Test", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(id, chat, "123", content, at, false, "audio", "a.ogg", "https://example.invalid/a", []byte("k"), nil, nil, 1, ""); err != nil {
		t.Fatal(err)
	}
}

func messageContent(t *testing.T, ms *MessageStore, id string) string {
	t.Helper()
	var content string
	if err := ms.db.QueryRow("SELECT COALESCE(content, '') FROM messages WHERE id = ?", id).Scan(&content); err != nil {
		t.Fatal(err)
	}
	return content
}

func TestAutoTranscriberStoresPrefixedTranscript(t *testing.T) {
	engine := &fakeEngine{text: "hello world"}
	tr, ms := testTranscriber(t, autoTranscribeVoice, engine)
	storeAudio(t, ms, "m1", "", time.Now())
	if !tr.offer("m1", phonePN.String(), "missing.ogg", true) {
		t.Fatal("voice note was not queued")
	}
	tr.process(<-tr.jobs)
	if got := messageContent(t, ms, "m1"); got != "[transcript (fake model)] hello world" {
		t.Fatalf("content = %q", got)
	}
}

func TestAutoTranscriberNeverOverwritesContent(t *testing.T) {
	engine := &fakeEngine{text: "new"}
	tr, ms := testTranscriber(t, autoTranscribeAudio, engine)
	storeAudio(t, ms, "m1", "[transcript (whisper x)] earlier", time.Now())
	tr.process(transcriptionJob{messageID: "m1", chatJID: phonePN.String()})
	if engine.calls != 0 {
		t.Fatalf("engine called %d times for an already transcribed message", engine.calls)
	}
	if stored, err := ms.StoreTranscript("m1", phonePN.String(), "late"); err != nil || stored {
		t.Fatalf("StoreTranscript = %v, %v; want no overwrite", stored, err)
	}
	if got := messageContent(t, ms, "m1"); got != "[transcript (whisper x)] earlier" {
		t.Fatalf("content = %q", got)
	}
}

func TestAutoTranscriberRetriesAtMostTwice(t *testing.T) {
	engine := &fakeEngine{text: "ok", failures: 2}
	tr, ms := testTranscriber(t, autoTranscribeAudio, engine)
	storeAudio(t, ms, "m1", "", time.Now())
	tr.process(transcriptionJob{messageID: "m1", chatJID: phonePN.String()})
	if engine.calls != 3 || messageContent(t, ms, "m1") == "" {
		t.Fatalf("calls = %d content = %q", engine.calls, messageContent(t, ms, "m1"))
	}

	failing := &fakeEngine{failures: 10}
	tr2, ms2 := testTranscriber(t, autoTranscribeAudio, failing)
	storeAudio(t, ms2, "m2", "", time.Now())
	tr2.process(transcriptionJob{messageID: "m2", chatJID: phonePN.String()})
	if failing.calls != 3 || messageContent(t, ms2, "m2") != "" {
		t.Fatalf("calls = %d content = %q", failing.calls, messageContent(t, ms2, "m2"))
	}
}

func TestAutoTranscriberFilters(t *testing.T) {
	chat := phonePN.String()
	var off *autoTranscriber
	if off.offer("m", chat, "x", true) {
		t.Fatal("nil transcriber queued a job")
	}
	voice, _ := testTranscriber(t, autoTranscribeVoice, &fakeEngine{})
	if voice.offer("m", chat, "x", false) {
		t.Fatal("voice mode queued non-voice audio")
	}
	audio, _ := testTranscriber(t, autoTranscribeAudio, &fakeEngine{})
	if !audio.offer("m", chat, "x", false) {
		t.Fatal("audio mode skipped audio")
	}
	audio.cfg.deny[chat] = true
	if audio.offer("m", chat, "x", true) {
		t.Fatal("denied chat was queued")
	}
	allow, _ := testTranscriber(t, autoTranscribeAudio, &fakeEngine{})
	allow.cfg.allow["other@g.us"] = true
	if allow.offer("m", chat, "x", true) {
		t.Fatal("chat outside the allow list was queued")
	}
}

func TestLoadAutoTranscribeConfig(t *testing.T) {
	get := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	cfg, err := loadAutoTranscribeConfig(get(nil))
	if err != nil || cfg.mode != autoTranscribeOff {
		t.Fatalf("default = %+v, %v", cfg, err)
	}
	cfg, err = loadAutoTranscribeConfig(get(map[string]string{
		"WHATSAPP_AUTO_TRANSCRIBE":               "Voice",
		"WHATSAPP_AUTO_TRANSCRIBE_MAX_SECONDS":   "120",
		"WHATSAPP_AUTO_TRANSCRIBE_EXCLUDE_CHATS": "a@g.us, b@g.us",
	}))
	if err != nil || cfg.mode != autoTranscribeVoice || cfg.maxSeconds != 120 || !cfg.deny["b@g.us"] {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	if _, err := loadAutoTranscribeConfig(get(map[string]string{"WHATSAPP_AUTO_TRANSCRIBE": "everything"})); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestBackfillQueuesOnlyUntranscribedAudioInRange(t *testing.T) {
	engine := &fakeEngine{text: "old note"}
	tr, ms := testTranscriber(t, autoTranscribeVoice, engine)
	now := time.Now()
	storeAudio(t, ms, "in-range", "", now.Add(-time.Hour))
	storeAudio(t, ms, "done", "[transcript (x y)] hi", now.Add(-time.Hour))
	storeAudio(t, ms, "too-old", "", now.Add(-48*time.Hour))
	refs, err := ms.UntranscribedAudio(now.Add(-24*time.Hour), now, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != "in-range" {
		t.Fatalf("refs = %+v", refs)
	}
	tr.backfill(refs, func(string, string) (string, error) { return "x.ogg", nil })
	tr.process(<-tr.jobs)
	if got := messageContent(t, ms, "in-range"); got != "[transcript (fake model)] old note" {
		t.Fatalf("content = %q", got)
	}
}

func TestHandleMessageQueuesDownloadedVoiceNote(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	path := filepath.Join(t.TempDir(), "voice.ogg")
	stubAutomaticDownloads(t, path, false)
	ms := newTestMessageStore(t)
	tr := newAutoTranscriber(autoTranscribeConfig{mode: autoTranscribeVoice, allow: map[string]bool{}, deny: map[string]bool{}}, &fakeEngine{text: "hi"}, ms)
	autoTranscribe = tr
	t.Cleanup(func() { autoTranscribe = nil })

	msg := mediaControlMessage("document", phonePN)
	msg.Message = &waProto.Message{AudioMessage: &waProto.AudioMessage{
		URL:      proto.String("https://example.invalid/audio"),
		MediaKey: []byte("test-media-key"),
		PTT:      proto.Bool(true),
	}}
	handleMessage(newTestClient(&mockLIDStore{}), ms, msg, testLogger())

	select {
	case job := <-tr.jobs:
		if job.messageID != msg.Info.ID || job.path != path {
			t.Fatalf("job = %+v", job)
		}
	default:
		t.Fatal("downloaded voice note was not queued for transcription")
	}
}
