package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"whatsapp-client/internal/transcribe"
)

const (
	autoTranscribeOff     = "off"
	autoTranscribeVoice   = "voice"
	autoTranscribeAudio   = "audio"
	transcribeQueueSize   = 256
	transcribeMaxAttempts = 3
)

type autoTranscribeConfig struct {
	mode       string
	maxSeconds uint32
	allow      map[string]bool
	deny       map[string]bool
}

type transcriptionJob struct {
	messageID string
	chatJID   string
	path      string
}

type autoTranscriber struct {
	cfg        autoTranscribeConfig
	engine     transcribe.Engine
	store      *MessageStore
	jobs       chan transcriptionJob
	retryDelay time.Duration
	logf       func(format string, args ...interface{})
}

var autoTranscribe *autoTranscriber

func parseJIDSet(raw string) map[string]bool {
	set := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			set[item] = true
		}
	}
	return set
}

func loadAutoTranscribeConfig(getenv func(string) string) (autoTranscribeConfig, error) {
	cfg := autoTranscribeConfig{mode: strings.ToLower(strings.TrimSpace(getenv("WHATSAPP_AUTO_TRANSCRIBE")))}
	switch cfg.mode {
	case "", "false", "0":
		cfg.mode = autoTranscribeOff
	case autoTranscribeOff, autoTranscribeVoice, autoTranscribeAudio:
	default:
		return cfg, fmt.Errorf("WHATSAPP_AUTO_TRANSCRIBE must be off, voice or audio")
	}
	if raw := strings.TrimSpace(getenv("WHATSAPP_AUTO_TRANSCRIBE_MAX_SECONDS")); raw != "" {
		seconds, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return cfg, fmt.Errorf("WHATSAPP_AUTO_TRANSCRIBE_MAX_SECONDS must be a whole number of seconds")
		}
		cfg.maxSeconds = uint32(seconds)
	}
	cfg.allow = parseJIDSet(getenv("WHATSAPP_AUTO_TRANSCRIBE_CHATS"))
	cfg.deny = parseJIDSet(getenv("WHATSAPP_AUTO_TRANSCRIBE_EXCLUDE_CHATS"))
	return cfg, nil
}

func newAutoTranscriber(cfg autoTranscribeConfig, engine transcribe.Engine, store *MessageStore) *autoTranscriber {
	return &autoTranscriber{
		cfg:        cfg,
		engine:     engine,
		store:      store,
		jobs:       make(chan transcriptionJob, transcribeQueueSize),
		retryDelay: 5 * time.Second,
		logf:       func(format string, args ...interface{}) { fmt.Printf(format+"\n", args...) },
	}
}

func startAutoTranscriber(getenv func(string) string, store *MessageStore) (*autoTranscriber, error) {
	cfg, err := loadAutoTranscribeConfig(getenv)
	if err != nil || cfg.mode == autoTranscribeOff {
		return nil, err
	}
	engine, err := transcribe.FromEnv(getenv)
	if err != nil {
		return nil, fmt.Errorf("auto-transcribe is on but the engine is not configured: %w", err)
	}
	t := newAutoTranscriber(cfg, engine, store)
	go t.run()
	return t, nil
}

func (t *autoTranscriber) chatAllowed(chatJID string) bool {
	if t.cfg.deny[chatJID] {
		return false
	}
	return len(t.cfg.allow) == 0 || t.cfg.allow[chatJID]
}

func (t *autoTranscriber) wants(chatJID string, voiceNote bool, path string) bool {
	if t == nil || !t.chatAllowed(chatJID) {
		return false
	}
	if t.cfg.mode == autoTranscribeVoice && !voiceNote {
		return false
	}
	if t.cfg.maxSeconds > 0 {
		if data, err := os.ReadFile(path); err == nil {
			if seconds, _, err := analyzeOggOpus(data); err == nil && seconds > t.cfg.maxSeconds {
				return false
			}
		}
	}
	return true
}

func (t *autoTranscriber) offer(messageID, chatJID, path string, voiceNote bool) bool {
	if !t.wants(chatJID, voiceNote, path) {
		return false
	}
	select {
	case t.jobs <- transcriptionJob{messageID: messageID, chatJID: chatJID, path: path}:
		return true
	default:
		t.logf("Transcription queue full, skipping message %s", messageID)
		return false
	}
}

func (t *autoTranscriber) run() {
	for job := range t.jobs {
		t.process(job)
	}
}

func (t *autoTranscriber) process(job transcriptionJob) {
	if empty, err := t.store.AudioContentEmpty(job.messageID, job.chatJID); err != nil || !empty {
		return
	}
	var text string
	var err error
	for attempt := 1; attempt <= transcribeMaxAttempts; attempt++ {
		text, err = t.engine.Transcribe(context.Background(), job.path)
		if err == nil {
			break
		}
		t.logf("Transcription attempt %d/%d failed for message %s: %v", attempt, transcribeMaxAttempts, job.messageID, err)
		if attempt < transcribeMaxAttempts {
			time.Sleep(t.retryDelay * time.Duration(attempt))
		}
	}
	if err != nil {
		return
	}
	stored, err := t.store.StoreTranscript(job.messageID, job.chatJID, transcribe.Prefix(t.engine)+text)
	if err != nil {
		t.logf("Failed to store transcript for message %s: %v", job.messageID, err)
		return
	}
	if stored {
		t.logf("Transcribed voice message %s", job.messageID)
	}
}

type audioMessageRef struct {
	ID      string
	ChatJID string
}

func (store *MessageStore) AudioContentEmpty(id, chatJID string) (bool, error) {
	var count int
	err := store.db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE id = ? AND chat_jid = ? AND media_type = 'audio' AND (content IS NULL OR content = '')",
		id, chatJID,
	).Scan(&count)
	return count > 0, err
}

func (store *MessageStore) StoreTranscript(id, chatJID, content string) (bool, error) {
	result, err := store.db.Exec(
		"UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ? AND media_type = 'audio' AND (content IS NULL OR content = '')",
		content, id, chatJID,
	)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (store *MessageStore) UntranscribedAudio(after, before time.Time, chatJID string) ([]audioMessageRef, error) {
	query := "SELECT id, chat_jid FROM messages WHERE media_type = 'audio' AND (content IS NULL OR content = '') AND deleted_at IS NULL AND timestamp >= ? AND timestamp <= ?"
	args := []interface{}{after, before}
	if chatJID != "" {
		query += " AND chat_jid = ?"
		args = append(args, chatJID)
	}
	rows, err := store.db.Query(query+" ORDER BY timestamp DESC", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []audioMessageRef
	for rows.Next() {
		var ref audioMessageRef
		if err := rows.Scan(&ref.ID, &ref.ChatJID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (t *autoTranscriber) backfill(refs []audioMessageRef, download func(messageID, chatJID string) (string, error)) {
	for _, ref := range refs {
		if !t.chatAllowed(ref.ChatJID) {
			continue
		}
		path, err := download(ref.ID, ref.ChatJID)
		if err != nil {
			t.logf("Backfill could not fetch audio for message %s: %v", ref.ID, err)
			continue
		}
		if !t.wants(ref.ChatJID, true, path) {
			continue
		}
		t.jobs <- transcriptionJob{messageID: ref.ID, chatJID: ref.ChatJID, path: path}
	}
}
