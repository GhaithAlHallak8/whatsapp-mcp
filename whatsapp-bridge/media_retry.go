package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	mediaRetryPending   = "pending"
	mediaRetryRecovered = "recovered"
	mediaRetryGone      = "gone"
	mediaRetryFailed    = "failed"

	mediaRetryHost = "https://mmg.whatsapp.net"

	mediaRetryRequestedMessage = "This media has expired on WhatsApp's servers. A fresh copy was requested from your phone; keep the phone online and try again shortly."
	mediaRetryGoneMessage      = "This media has expired on WhatsApp's servers and the phone no longer has the file, so it cannot be recovered."
)

var (
	mediaRetryWait     = 20 * time.Second
	mediaRetryCooldown = 60 * time.Second

	errMediaRetryRequested = errors.New(mediaRetryRequestedMessage)
	errMediaGone           = errors.New(mediaRetryGoneMessage)

	mediaRetryWaitersMu sync.Mutex
	mediaRetryWaiters   = map[string][]chan string{}
)

var sendMediaRetryReceipt = func(client *whatsmeow.Client, info *types.MessageInfo, mediaKey []byte) error {
	return client.SendMediaRetryReceipt(context.Background(), info, mediaKey)
}

var decryptMediaRetry = whatsmeow.DecryptMediaRetryNotification

func isMediaExpired(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

func ensureMediaRetriesTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS media_retries (
		message_id TEXT,
		chat_jid TEXT,
		requested_at TIMESTAMP,
		status TEXT,
		PRIMARY KEY (message_id, chat_jid)
	)`)
	return err
}

func (store *MessageStore) MediaRetryState(messageID, chatJID string) (string, time.Time, error) {
	var status string
	var requestedAt time.Time
	err := store.db.QueryRow(
		"SELECT status, requested_at FROM media_retries WHERE message_id = ? AND chat_jid = ?", messageID, chatJID,
	).Scan(&status, &requestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return status, requestedAt, err
}

func (store *MessageStore) RecordMediaRetryRequest(messageID, chatJID string, at time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO media_retries (message_id, chat_jid, requested_at, status) VALUES (?, ?, ?, ?)
		 ON CONFLICT(message_id, chat_jid) DO UPDATE SET requested_at = excluded.requested_at, status = excluded.status`,
		messageID, chatJID, at, mediaRetryPending,
	)
	return err
}

func (store *MessageStore) SetMediaRetryStatus(messageID, chatJID, status string) error {
	_, err := store.db.Exec(
		"UPDATE media_retries SET status = ? WHERE message_id = ? AND chat_jid = ?", status, messageID, chatJID,
	)
	return err
}

func (store *MessageStore) mediaRetryChatFor(messageID string, candidates ...string) (string, error) {
	for _, chatJID := range candidates {
		if chatJID == "" {
			continue
		}
		status, _, err := store.MediaRetryState(messageID, chatJID)
		if err != nil {
			return "", err
		}
		if status != "" {
			return chatJID, nil
		}
	}
	var chatJID string
	err := store.db.QueryRow(
		"SELECT chat_jid FROM media_retries WHERE message_id = ? AND status = ? ORDER BY requested_at DESC LIMIT 1",
		messageID, mediaRetryPending,
	).Scan(&chatJID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return chatJID, err
}

func mediaRetryMessageInfo(store *MessageStore, messageID, chatJID string) (*types.MessageInfo, []byte, error) {
	var sender string
	var isFromMe bool
	var timestamp time.Time
	var mediaKey []byte
	err := store.db.QueryRow(
		"SELECT COALESCE(sender, ''), is_from_me, timestamp, media_key FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chatJID,
	).Scan(&sender, &isFromMe, &timestamp, &mediaKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find message: %v", err)
	}
	if len(mediaKey) == 0 {
		return nil, nil, errors.New("message has no media key")
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid chat JID: %v", err)
	}
	info := &types.MessageInfo{ID: messageID, Timestamp: timestamp}
	info.Chat = chat
	info.IsFromMe = isFromMe
	info.IsGroup = chat.Server == types.GroupServer
	switch {
	case strings.Contains(sender, "@"):
		if parsed, parseErr := types.ParseJID(sender); parseErr == nil {
			info.Sender = parsed
		}
	case info.IsGroup && sender != "":
		info.Sender = types.NewJID(sender, types.DefaultUserServer)
	case !info.IsGroup:
		info.Sender = chat
	}
	if info.IsGroup && !isFromMe && info.Sender.IsEmpty() {
		return nil, nil, errors.New("group message has no stored sender, so the phone cannot be asked for it")
	}
	return info, mediaKey, nil
}

func mediaRetryKey(messageID, chatJID string) string { return chatJID + "\x00" + messageID }

func addMediaRetryWaiter(messageID, chatJID string) chan string {
	ch := make(chan string, 1)
	key := mediaRetryKey(messageID, chatJID)
	mediaRetryWaitersMu.Lock()
	mediaRetryWaiters[key] = append(mediaRetryWaiters[key], ch)
	mediaRetryWaitersMu.Unlock()
	return ch
}

func removeMediaRetryWaiter(messageID, chatJID string, ch chan string) {
	key := mediaRetryKey(messageID, chatJID)
	mediaRetryWaitersMu.Lock()
	defer mediaRetryWaitersMu.Unlock()
	waiters := mediaRetryWaiters[key]
	for i, waiter := range waiters {
		if waiter == ch {
			mediaRetryWaiters[key] = append(waiters[:i], waiters[i+1:]...)
			break
		}
	}
	if len(mediaRetryWaiters[key]) == 0 {
		delete(mediaRetryWaiters, key)
	}
}

func notifyMediaRetryWaiters(messageID, chatJID, status string) {
	key := mediaRetryKey(messageID, chatJID)
	mediaRetryWaitersMu.Lock()
	waiters := mediaRetryWaiters[key]
	delete(mediaRetryWaiters, key)
	mediaRetryWaitersMu.Unlock()
	for _, waiter := range waiters {
		waiter <- status
	}
}

func recoverExpiredMedia(client *whatsmeow.Client, store *MessageStore, messageID, chatJID string) error {
	status, requestedAt, err := store.MediaRetryState(messageID, chatJID)
	if err != nil {
		return fmt.Errorf("failed to read media retry state: %v", err)
	}
	if status == mediaRetryGone {
		return errMediaGone
	}
	waiter := addMediaRetryWaiter(messageID, chatJID)
	defer removeMediaRetryWaiter(messageID, chatJID, waiter)

	if status != mediaRetryPending || time.Since(requestedAt) > mediaRetryCooldown {
		info, mediaKey, err := mediaRetryMessageInfo(store, messageID, chatJID)
		if err != nil {
			return err
		}
		if err := sendMediaRetryReceipt(client, info, mediaKey); err != nil {
			return fmt.Errorf("failed to ask the phone for the media: %v", err)
		}
		if err := store.RecordMediaRetryRequest(messageID, chatJID, time.Now()); err != nil {
			return fmt.Errorf("failed to record media retry: %v", err)
		}
	}

	select {
	case result := <-waiter:
		switch result {
		case mediaRetryRecovered:
			return nil
		case mediaRetryGone:
			return errMediaGone
		}
		return errors.New("the phone could not re-upload this media; try again later")
	case <-time.After(mediaRetryWait):
		return errMediaRetryRequested
	}
}

func handleMediaRetry(client *whatsmeow.Client, store *MessageStore, evt *events.MediaRetry, logf func(format string, args ...interface{})) {
	chatJID, err := store.mediaRetryChatFor(evt.MessageID, evt.ChatID.String(), evt.ChatID.ToNonAD().String())
	if err != nil || chatJID == "" {
		return
	}
	finish := func(status string) {
		if err := store.SetMediaRetryStatus(evt.MessageID, chatJID, status); err != nil {
			logf("Failed to record media retry result for %s: %v", evt.MessageID, err)
		}
		notifyMediaRetryWaiters(evt.MessageID, chatJID, status)
	}
	if evt.Error != nil {
		finish(mediaRetryGone)
		return
	}
	var mediaKey []byte
	if err := store.db.QueryRow("SELECT media_key FROM messages WHERE id = ? AND chat_jid = ?", evt.MessageID, chatJID).Scan(&mediaKey); err != nil {
		finish(mediaRetryFailed)
		return
	}
	notification, err := decryptMediaRetry(evt, mediaKey)
	if err != nil {
		logf("Failed to decrypt media retry for %s: %v", evt.MessageID, err)
		finish(mediaRetryFailed)
		return
	}
	switch notification.GetResult() {
	case waMmsRetry.MediaRetryNotification_SUCCESS:
	case waMmsRetry.MediaRetryNotification_NOT_FOUND:
		finish(mediaRetryGone)
		return
	default:
		finish(mediaRetryFailed)
		return
	}
	directPath := notification.GetDirectPath()
	if directPath == "" {
		finish(mediaRetryFailed)
		return
	}
	if _, err := store.db.Exec("UPDATE messages SET url = ? WHERE id = ? AND chat_jid = ?", mediaRetryHost+directPath, evt.MessageID, chatJID); err != nil {
		logf("Failed to store refreshed media path for %s: %v", evt.MessageID, err)
		finish(mediaRetryFailed)
		return
	}
	success, mediaType, _, path, err := downloadMediaForMessage(client, store, evt.MessageID, chatJID)
	if err != nil || !success {
		logf("Download after media retry failed for %s: %v", evt.MessageID, err)
		finish(mediaRetryFailed)
		return
	}
	logf("Recovered expired media for message %s", evt.MessageID)
	if mediaType == "audio" {
		autoTranscribe.offer(evt.MessageID, chatJID, path, true)
	}
	finish(mediaRetryRecovered)
}

func downloadMediaWithRecovery(client *whatsmeow.Client, store *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	success, mediaType, filename, path, err := downloadMediaForMessage(client, store, messageID, chatJID)
	if err == nil || !isMediaExpired(err) {
		return success, mediaType, filename, path, err
	}
	if recoverErr := recoverExpiredMedia(client, store, messageID, chatJID); recoverErr != nil {
		return false, "", "", "", recoverErr
	}
	return downloadMediaForMessage(client, store, messageID, chatJID)
}
