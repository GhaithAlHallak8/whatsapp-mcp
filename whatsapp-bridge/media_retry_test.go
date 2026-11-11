package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type retryHarness struct {
	ms        *MessageStore
	receipts  []*types.MessageInfo
	downloads int
	expired   bool
	path      string
}

func newRetryHarness(t *testing.T, expiredErr error) *retryHarness {
	t.Helper()
	ms := newTestMessageStore(t)
	ms.db.SetMaxOpenConns(1)
	if err := ensureMediaRetriesTable(ms.db); err != nil {
		t.Fatal(err)
	}
	h := &retryHarness{ms: ms, expired: true, path: filepath.Join(t.TempDir(), "voice.ogg")}
	if err := os.WriteFile(h.path, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	origDownload, origSend, origDecrypt := downloadMediaForMessage, sendMediaRetryReceipt, decryptMediaRetry
	origWait := mediaRetryWait
	downloadMediaForMessage = func(_ *whatsmeow.Client, _ *MessageStore, _ string, _ string) (bool, string, string, string, error) {
		h.downloads++
		if h.expired {
			return false, "", "", "", fmt.Errorf("failed to download media: %w", expiredErr)
		}
		return true, "audio", "voice.ogg", h.path, nil
	}
	sendMediaRetryReceipt = func(_ *whatsmeow.Client, info *types.MessageInfo, _ []byte) error {
		h.receipts = append(h.receipts, info)
		return nil
	}
	t.Cleanup(func() {
		downloadMediaForMessage, sendMediaRetryReceipt, decryptMediaRetry = origDownload, origSend, origDecrypt
		mediaRetryWait = origWait
	})
	return h
}

func (h *retryHarness) phoneAnswers(result waMmsRetry.MediaRetryNotification_ResultType) {
	decryptMediaRetry = func(*events.MediaRetry, []byte) (*waMmsRetry.MediaRetryNotification, error) {
		return &waMmsRetry.MediaRetryNotification{Result: result.Enum(), DirectPath: proto.String("/v/new-path.enc?oh=1")}, nil
	}
}

func (h *retryHarness) answerWhenAsked(t *testing.T, id string) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if status, _, _ := h.ms.MediaRetryState(id, phonePN.String()); status == mediaRetryPending {
				h.expired = false
				handleMediaRetry(nil, h.ms, &events.MediaRetry{MessageID: id, ChatID: phonePN}, func(string, ...interface{}) {})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return done
}

func TestExpiredMediaIsRecoveredFromPhone(t *testing.T) {
	for name, expiredErr := range map[string]error{
		"404": whatsmeow.ErrMediaDownloadFailedWith404,
		"410": whatsmeow.ErrMediaDownloadFailedWith410,
	} {
		t.Run(name, func(t *testing.T) {
			h := newRetryHarness(t, expiredErr)
			storeAudio(t, h.ms, "m1", "", time.Now().Add(-90*24*time.Hour))
			h.phoneAnswers(waMmsRetry.MediaRetryNotification_SUCCESS)
			done := h.answerWhenAsked(t, "m1")

			success, mediaType, _, path, err := downloadMediaWithRecovery(nil, h.ms, "m1", phonePN.String())
			<-done
			if err != nil || !success || mediaType != "audio" || path != h.path {
				t.Fatalf("recovery = %v %q %q %v", success, mediaType, path, err)
			}
			if len(h.receipts) != 1 || h.receipts[0].ID != "m1" || h.receipts[0].Chat != phonePN {
				t.Fatalf("receipts = %+v", h.receipts)
			}
			var url string
			if err := h.ms.db.QueryRow("SELECT url FROM messages WHERE id = 'm1'").Scan(&url); err != nil || url != mediaRetryHost+"/v/new-path.enc?oh=1" {
				t.Fatalf("url = %q err = %v", url, err)
			}
			if got := extractDirectPathFromURL(url); got != "/v/new-path.enc?oh=1" {
				t.Fatalf("direct path = %q", got)
			}
			if status, _, _ := h.ms.MediaRetryState("m1", phonePN.String()); status != mediaRetryRecovered {
				t.Fatalf("status = %q", status)
			}
		})
	}
}

func TestExpiredMediaGoneFromPhone(t *testing.T) {
	h := newRetryHarness(t, whatsmeow.ErrMediaDownloadFailedWith410)
	storeAudio(t, h.ms, "m1", "", time.Now())
	h.phoneAnswers(waMmsRetry.MediaRetryNotification_NOT_FOUND)
	done := h.answerWhenAsked(t, "m1")
	_, _, _, _, err := downloadMediaWithRecovery(nil, h.ms, "m1", phonePN.String())
	<-done
	if !errors.Is(err, errMediaGone) {
		t.Fatalf("err = %v", err)
	}
	h.expired = true
	if _, _, _, _, err := downloadMediaWithRecovery(nil, h.ms, "m1", phonePN.String()); !errors.Is(err, errMediaGone) {
		t.Fatalf("second err = %v", err)
	}
	if len(h.receipts) != 1 {
		t.Fatalf("phone asked %d times for media it already said is gone", len(h.receipts))
	}
}

func TestExpiredMediaPhoneOfflineDoesNotSpam(t *testing.T) {
	h := newRetryHarness(t, whatsmeow.ErrMediaDownloadFailedWith404)
	mediaRetryWait = 20 * time.Millisecond
	storeAudio(t, h.ms, "m1", "", time.Now())
	for i := 0; i < 3; i++ {
		if _, _, _, _, err := downloadMediaWithRecovery(nil, h.ms, "m1", phonePN.String()); !errors.Is(err, errMediaRetryRequested) {
			t.Fatalf("call %d err = %v", i, err)
		}
	}
	if len(h.receipts) != 1 {
		t.Fatalf("receipts = %d, want 1 within the cooldown", len(h.receipts))
	}
}

func TestNonExpiryDownloadErrorsDoNotAskPhone(t *testing.T) {
	h := newRetryHarness(t, whatsmeow.ErrMediaDownloadFailedWith403)
	storeAudio(t, h.ms, "m1", "", time.Now())
	if _, _, _, _, err := downloadMediaWithRecovery(nil, h.ms, "m1", phonePN.String()); err == nil || errors.Is(err, errMediaRetryRequested) {
		t.Fatalf("err = %v", err)
	}
	if len(h.receipts) != 0 {
		t.Fatalf("receipts = %d", len(h.receipts))
	}
}

func TestMediaRetryMessageInfoForGroups(t *testing.T) {
	ms := newTestMessageStore(t)
	group := "120363000000000000@g.us"
	now := time.Now()
	if err := ms.StoreChat(group, "G", now); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("g1", group, "15551234567", "", now, false, "audio", "a.ogg", "https://x.net/a", []byte("k"), nil, nil, 1, ""); err != nil {
		t.Fatal(err)
	}
	info, key, err := mediaRetryMessageInfo(ms, "g1", group)
	if err != nil || string(key) != "k" || !info.IsGroup || info.Sender.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("info = %+v key = %q err = %v", info, key, err)
	}
	if err := ms.StoreMessage("g2", group, "", "", now, false, "audio", "a.ogg", "https://x.net/a", []byte("k"), nil, nil, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mediaRetryMessageInfo(ms, "g2", group); err == nil {
		t.Fatal("group message without a sender was accepted")
	}
}

func TestDownloadRequestCanSkipPhoneRecovery(t *testing.T) {
	h := newRetryHarness(t, whatsmeow.ErrMediaDownloadFailedWith410)
	storeAudio(t, h.ms, "m1", "", time.Now())
	skip := false
	_, _, _, _, err := downloadMediaForRequest(nil, h.ms, DownloadMediaRequest{MessageID: "m1", ChatJID: phonePN.String(), Recover: &skip})
	if !errors.Is(err, errMediaExpired) || len(h.receipts) != 0 {
		t.Fatalf("err = %v, phone asked %d times", err, len(h.receipts))
	}
	h.expired = false
	success, _, _, path, err := downloadMediaForRequest(nil, h.ms, DownloadMediaRequest{MessageID: "m1", ChatJID: phonePN.String(), Recover: &skip})
	if err != nil || !success || path != h.path {
		t.Fatalf("download = %v %q %v", success, path, err)
	}
}
