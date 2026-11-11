package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// resolveLIDChat resolves a LID-based chat JID to its phone-based equivalent
// so that incoming and outgoing messages are stored under the same chat entry.
// The senderAlt/recipientAlt fields carry the phone JID on live messages;
// for history sync these will be empty and the function falls back to the
// whatsmeow LID store (populated during live message handling).
func resolveLIDChat(client *whatsmeow.Client, chat, senderAlt, recipientAlt types.JID, isFromMe bool) types.JID {
	if chat.Server != types.HiddenUserServer {
		return chat
	}

	// For incoming DMs the phone JID is in SenderAlt;
	// for outgoing DMs it is in RecipientAlt.
	var alt types.JID
	if !isFromMe && !senderAlt.IsEmpty() && senderAlt.Server == types.DefaultUserServer {
		alt = senderAlt.ToNonAD()
	} else if isFromMe && !recipientAlt.IsEmpty() && recipientAlt.Server == types.DefaultUserServer {
		alt = recipientAlt.ToNonAD()
	}

	if !alt.IsEmpty() {
		fmt.Printf("Resolved LID chat %s -> %s (from message alt)\n", chat, alt)
		return alt
	}

	// Fallback: query the whatsmeow LID-PN mapping store. Guarded like
	// resolveUserJID so a client without a store (tests, early startup)
	// degrades to the unresolved LID instead of a nil dereference.
	if client != nil && client.Store != nil && client.Store.LIDs != nil {
		pn, err := client.Store.LIDs.GetPNForLID(context.Background(), chat)
		if err == nil && !pn.IsEmpty() {
			fmt.Printf("Resolved LID chat %s -> %s (from LID store)\n", chat, pn.ToNonAD())
			return pn.ToNonAD()
		}
	}

	fmt.Printf("Warning: could not resolve LID chat %s to phone JID\n", chat)
	return chat
}

// isSelfReadReceipt reports whether a receipt means WE read the chat (on this
// or another device), as opposed to another user reading our outgoing message.
// A DM self-read arrives as read-self; a group self-read arrives as a plain
// read whose participant is us (IsFromMe). See types.ReceiptType docs.
func isSelfReadReceipt(receipt *events.Receipt) bool {
	return receipt.Type == types.ReceiptTypeReadSelf ||
		(receipt.Type == types.ReceiptTypeRead && receipt.IsFromMe)
}

// resolveUserJID resolves a single user JID (sender or participant) to its
// phone-based equivalent. Unlike resolveLIDChat it takes a single hint alt
// JID (either SenderAlt for the peer in a DM or the user's own phone JID
// for outgoing messages) so it can never accidentally substitute the
// recipient's identity for the sender's. Falls back to the whatsmeow
// LID-PN store, then returns the original JID if no mapping is known.
func resolveUserJID(client *whatsmeow.Client, j, alt types.JID) types.JID {
	j = j.ToNonAD()
	if j.Server != types.HiddenUserServer {
		return j
	}
	if !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if client != nil && client.Store != nil && client.Store.LIDs != nil {
		if pn, err := client.Store.LIDs.GetPNForLID(context.Background(), j); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return j
}

// senderAltForMessage returns the best phone-JID hint for the sender of a
// message: SenderAlt for incoming, the user's own phone JID for outgoing.
// Falls through to EmptyJID if no hint is available, in which case
// resolveUserJID will fall back to the LID store.
func senderAltForMessage(client *whatsmeow.Client, info types.MessageInfo) types.JID {
	if info.IsFromMe {
		if client != nil && client.Store != nil && client.Store.ID != nil {
			return client.Store.ID.ToNonAD()
		}
		return types.EmptyJID
	}
	if !info.SenderAlt.IsEmpty() && info.SenderAlt.Server == types.DefaultUserServer {
		return info.SenderAlt.ToNonAD()
	}
	return types.EmptyJID
}

// Handle regular incoming messages with media support
// origMsgTime remembers the true send-time of messages that first arrived
// undecryptable (e.g. after an offline gap, when our session lacked the sender
// key). WhatsApp re-sends such messages after a retry receipt, but the re-sent
// copy carries a fresh `t` (the resend time) rather than the original send time.
// The first (undecryptable) delivery *does* carry the original `t`, so we cache
// it here and reuse it when the decrypted retry finally lands — otherwise those
// messages get stored with reconnect-time and corrupt recency ordering.
var (
	origMsgTimeMu sync.Mutex
	origMsgTime   = make(map[string]time.Time)
)

// rememberOriginalTimestamp records the earliest timestamp seen for a message ID.
// A resend's `t` is always >= the original, so the earliest is the true one.
func rememberOriginalTimestamp(id string, ts time.Time) {
	if id == "" || ts.IsZero() {
		return
	}
	origMsgTimeMu.Lock()
	defer origMsgTimeMu.Unlock()
	if existing, ok := origMsgTime[id]; !ok || ts.Before(existing) {
		origMsgTime[id] = ts
	}
	// Soft cap so a burst of never-retried undecryptable messages can't grow this
	// map unbounded. Entries are normally consumed on successful decrypt.
	if len(origMsgTime) > 5000 {
		for k := range origMsgTime {
			delete(origMsgTime, k)
			if len(origMsgTime) <= 4000 {
				break
			}
		}
	}
}

// takeOriginalTimestamp returns and removes the cached original timestamp for id.
func takeOriginalTimestamp(id string) (time.Time, bool) {
	origMsgTimeMu.Lock()
	defer origMsgTimeMu.Unlock()
	ts, ok := origMsgTime[id]
	if ok {
		delete(origMsgTime, id)
	}
	return ts, ok
}

func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Resolve LID-based chats to phone-based JIDs so that incoming
	// and outgoing messages land in the same chat entry.
	resolvedChat := resolveLIDChat(client, msg.Info.Chat, msg.Info.SenderAlt, msg.Info.RecipientAlt, msg.Info.IsFromMe)
	chatJID := resolvedChat.String()
	// Resolve the *sender* with a sender-specific alt so that outgoing-from-self
	// messages don't get tagged with the recipient's phone number, and incoming
	// messages from LID-only peers get rewritten to their phone user-part when
	// the LID store has a mapping.
	resolvedSender := resolveUserJID(client, msg.Info.Sender, senderAltForMessage(client, msg.Info))
	sender := resolvedSender.User

	// Get appropriate chat name (pass resolved JID so contact lookup works)
	name := GetChatName(client, messageStore, resolvedChat, chatJID, nil, sender, logger)

	// If contact resolution fails (common for LIDs), PushName is often the best available display name.
	// Only apply for direct messages (not groups) and only when the stored name is the numeric JID user.
	if !msg.Info.IsFromMe && msg.Info.Chat.Server != "g.us" && strings.TrimSpace(msg.Info.PushName) != "" {
		pushName := strings.TrimSpace(msg.Info.PushName)
		if name == "" || name == msg.Info.Chat.User {
			logger.Infof("Updating chat name from PushName for %s: %s -> %s", chatJID, name, pushName)
			name = pushName
		}
	}

	// Recover the true send-time if this message first arrived undecryptable and is
	// now landing via a retry-resend (whose stanza `t` is the resend time, not the
	// original). See origMsgTime / rememberOriginalTimestamp.
	msgTimestamp := msg.Info.Timestamp
	if orig, ok := takeOriginalTimestamp(msg.Info.ID); ok && orig.Before(msgTimestamp) {
		logger.Infof("Using original pre-retry timestamp for %s: %s (resend `t` was %s)",
			msg.Info.ID, orig.Format(time.RFC3339), msgTimestamp.Format(time.RFC3339))
		msgTimestamp = orig
	}

	// last_message_time is advanced only when a message row is actually stored
	// (see StoreChat below). Bumping it here for every stanza let reactions
	// and protocol messages float a chat to the top of list_chats — and, for
	// inbound reactions, flag it unread — without any new message to show.
	updateChatEphemeralSettingsFromProtocolMessage(messageStore, chatJID, msg.Message, msg.Info.Timestamp.Unix(), logger)
	handleMessageRevoke(messageStore, msg.Message, chatJID, msg.Info.Timestamp.Unix(), logger)

	// Backfill ephemeral state from any regular message's ContextInfo.
	// EPHEMERAL_SETTING ProtocolMessages and GroupInfo events only fire on
	// changes, so chats whose disappearing timer was set before the bridge
	// started (or before this code shipped) would otherwise stay invisible
	// to outgoing-message logic.
	if backfill := extractChatEphemeralFromMessage(msg.Message); backfill.SettingTimestamp != 0 {
		if err := messageStore.UpdateChatEphemeralSettings(chatJID, backfill.Expiration, backfill.SettingTimestamp); err != nil {
			logger.Warnf("Failed to backfill ephemeral settings for %s: %v", chatJID, err)
		}
	}

	// Status updates are stored but never forwarded or auto-downloaded.
	isStatus := msg.Info.Chat == types.StatusBroadcastJID
	shouldForward := !isStatus && webhooksEnabled() && (forwardSelfMessages || !msg.Info.IsFromMe)

	// Reactions arrive as their own message stanza rather than message content.
	// Persist them in the messages table as media_type="reaction", with the
	// emoji in `content` and the reacted-to message ID in `filename`, then
	// return — a reaction is not a normal content message. An empty emoji is a
	// valid event meaning "reaction removed"; we store it (so consumers see the
	// removal) rather than dropping it.
	if reaction := msg.Message.GetReactionMessage(); reaction != nil {
		reactedToID := ""
		if key := reaction.GetKey(); key != nil {
			reactedToID = key.GetID()
		}
		if reactedToID != "" {
			emoji := reaction.GetText()
			if err := messageStore.StoreChatName(chatJID, name); err != nil {
				logger.Warnf("Failed to store chat for reaction: %v", err)
			}
			if err := messageStore.StoreMessage(
				msg.Info.ID, chatJID, sender, emoji,
				msgTimestamp, msg.Info.IsFromMe,
				"reaction", reactedToID, "", nil, nil, nil, 0, "",
			); err != nil {
				logger.Warnf("Failed to store reaction: %v", err)
			}
			if shouldForward {
				SendReactionWebhook(sender, chatJID, msg.Info.IsFromMe, msg.Info.ID, reactedToID, emoji)
			}
		}
		return
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info - pass message timestamp + ID for unique filenames.
	// Must be the same (retry-corrected) timestamp we store below: downloadMedia
	// rebuilds the on-disk filename from the stored timestamp.
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message, msgTimestamp, msg.Info.ID)

	// Extract quoted message info
	quotedMessageId, quotedSender, quotedContent := extractQuotedMessageInfo(msg.Message)
	mentionedJIDs := extractMentionedJIDs(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Advance the chat's last_message_time now that we know a message row will
	// be written, then store the message first so that downloadMedia (which
	// queries the DB by message ID) can find the row when called synchronously.
	err := messageStore.StoreChat(chatJID, name, msgTimestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msgTimestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
		quotedMessageId,
	)
	stored := err == nil
	if !stored {
		// The row is what /api/download, get_message_context and mark-read key
		// on; without it the message exists only downstream. Still forward the
		// webhook so the text isn't lost, but say so loudly and skip media
		// downloads, which look the row up by ID and would only fail again.
		logger.Errorf("Failed to store message %s in %s (webhook still sent, media not downloaded): %v",
			msg.Info.ID, chatJID, err)
	}

	var quotedIsFromMe *bool
	if quotedMessageId != "" {
		var lookupErr error
		quotedIsFromMe, lookupErr = messageStore.GetMessageIsFromMe(quotedMessageId, chatJID)
		if lookupErr != nil {
			logger.Warnf("Failed to resolve quoted message origin: %v", lookupErr)
		}
	}

	// The opt-out covers every implicit download, including webhook images.
	shouldDownload := !isStatus && autoDownloadMediaEnabled()

	// Forwarded images download synchronously to include bytes in the webhook.
	// Other media downloads asynchronously for caching when downloads are enabled.
	var imageDownloadPath string
	imageMimeType := msg.Message.GetImageMessage().GetMimetype()
	if mediaType == "image" && url != "" && len(mediaKey) > 0 && shouldForward && stored && shouldDownload {
		logger.Infof("Downloading image media for message %s (synchronous)", msg.Info.ID)
		success, _, _, dlPath, dlErr := downloadMediaForMessage(client, messageStore, msg.Info.ID, chatJID)
		if success && dlErr == nil {
			imageDownloadPath = dlPath
			// Detect MIME type by sniffing the actual file bytes rather than
			// trusting the generated filename extension (always .jpg).
			if f, openErr := os.Open(dlPath); openErr == nil {
				buf := make([]byte, 512)
				if n, readErr := f.Read(buf); readErr == nil || n > 0 {
					imageMimeType = http.DetectContentType(buf[:n])
				}
				_ = f.Close()
			}
			if imageMimeType == "" {
				imageMimeType = "application/octet-stream"
			}
			logger.Infof("✅ Image downloaded: %s (%s)", dlPath, imageMimeType)
		} else {
			logger.Warnf("❌ Image download failed: %v", dlErr)
			// Fall back to async download so media is cached for future MCP tool calls
			scheduleMediaDownload(func() {
				_, _, _, _, _ = downloadMediaForMessage(client, messageStore, msg.Info.ID, chatJID)
			})
		}
	} else if mediaType != "" && url != "" && len(mediaKey) > 0 && stored && shouldDownload {
		// Media that is not included in a webhook payload: async download for caching.
		logger.Infof("Auto-downloading %s media for message %s", mediaType, msg.Info.ID)
		scheduleMediaDownload(func() {
			success, _, _, downloadPath, err := downloadMediaForMessage(client, messageStore, msg.Info.ID, chatJID)
			if success && err == nil {
				logger.Infof("✅ Auto-downloaded media: %s", downloadPath)
			} else {
				logger.Warnf("❌ Auto-download failed: %v", err)
			}
		})
	}

	// Send webhook for incoming messages.
	// Forward self-messages when FORWARD_SELF=true.
	// Always forward image messages (even without a text caption) so the AI vision
	// pipeline can analyse the image content.
	hasText := content != ""
	hasImage := mediaType == "image"

	if shouldForward && (hasText || hasImage) {
		if hasImage {
			SendWebhookWithMedia(
				sender, content, chatJID, msg.Info.IsFromMe,
				quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs,
				msg.Info.ID, mediaType, imageMimeType, filename, imageDownloadPath,
			)
		} else {
			SendWebhookWithMessageID(sender, content, chatJID, msg.Info.IsFromMe, quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs, msg.Info.ID)
		}
	}

	if err == nil {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// isPlaceholderChatName reports whether name is one of the fallbacks GetChatName
// stores when no real display name could be resolved: the bare numeric user-part
// of the JID, or "Group <id>" for groups. Such a value must not be treated as a
// resolved name, otherwise it can never be upgraded once a better one exists.
func isPlaceholderChatName(name string, jid types.JID) bool {
	if name == "" {
		return true
	}
	return name == jid.User || name == fmt.Sprintf("Group %s", jid.User)
}

// isSelfUser reports whether user is the user-part of our own account, in
// either its phone or its LID form. Used to keep our own identity out of
// peer-facing display names.
func isSelfUser(client *whatsmeow.Client, user string) bool {
	if user == "" || client == nil || client.Store == nil {
		return false
	}
	if client.Store.ID != nil && user == client.Store.ID.ToNonAD().User {
		return true
	}
	if !client.Store.LID.IsEmpty() && user == client.Store.LID.ToNonAD().User {
		return true
	}
	return false
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a real name.
	//
	// A stored name that is merely the numeric JID user-part (or "Group <id>")
	// is one of the fallbacks further down this function, not a name anyone
	// chose. Returning it here would make that fallback permanent: the first
	// message from an unknown peer pins the number, and the chat keeps it even
	// after the contact shows up in whatsmeow's store — which is exactly what
	// happens after a re-pair, when the contact sync repopulates
	// whatsmeow_contacts but nothing ever re-reads it. Treat those as "no name
	// yet" and fall through to resolution instead.
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	// Older bridges also cached our own number as the name of outgoing-first
	// DMs. Repair those on the next event, while preserving actual self chats.
	badSelfName := (jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer) &&
		!isSelfUser(client, jid.User) && isSelfUser(client, existingName)
	if err == nil && existingName != "" && !isPlaceholderChatName(existingName, jid) && !badSelfName {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Pointer && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Pointer && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Pointer && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else {
			name = lookupLocalContactName(client, messageStore, chatJID, logger)

			if name == "" {
				if sender != "" && !isSelfUser(client, sender) {
					name = sender
				} else {
					name = jid.User
				}
			}
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

func lookupLocalContactName(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, logger waLog.Logger) string {
	if client == nil || client.Store == nil || client.Store.ID == nil || messageStore == nil || messageStore.waDB == nil {
		return ""
	}

	var localName string
	err := messageStore.waDB.QueryRow(
		`SELECT COALESCE(
			NULLIF(full_name, ''),
			NULLIF(push_name, ''),
			NULLIF(first_name, ''),
			NULLIF(business_name, ''),
			''
		) FROM whatsmeow_contacts WHERE our_jid = ? AND their_jid = ?`,
		client.Store.ID.String(),
		chatJID,
	).Scan(&localName)
	if err == nil {
		if localName != "" {
			logger.Infof("Using local contact name for %s: %s", chatJID, localName)
		}
		return localName
	}
	if err != sql.ErrNoRows && !strings.Contains(err.Error(), "no such table: whatsmeow_contacts") {
		logger.Warnf("Failed to query local contact name for %s: %v", chatJID, err)
	}
	return ""
}

// callChatJID resolves the chat JID that a call belongs to. For group calls
// this is the group JID; for 1:1 calls it's the call creator's JID — which
// stays stable across the entire lifecycle (Offer → Accept → Terminate).
//
// meta.From is NOT reliable as the chat key: for Accept events that fire
// when the user picks up on their phone, meta.From is the *accepting*
// device's JID (our own), not the other party's. Using From caused Accept
// UPDATEs to miss the row stored at Offer time, so the state machine fell
// through to "missed" when the user answered elsewhere.
//
// meta.CallCreator is populated from the stanza's call-creator attribute,
// which WhatsApp keeps consistent for every event in the call.
func callChatJID(meta types.BasicCallMeta) string {
	if !meta.GroupJID.IsEmpty() {
		return meta.GroupJID.String()
	}
	if !meta.CallCreator.IsEmpty() {
		return meta.CallCreator.ToNonAD().String()
	}
	return meta.From.ToNonAD().String()
}

// handleCallOffer stores a new call row. The isFromMe path is defensive —
// in practice WhatsApp's primary device handles outbound calls without
// notifying linked devices, so events observed here are always inbound and
// isFromMe stays false. We keep the branch anyway in case behavior changes.
func handleCallOffer(client *whatsmeow.Client, messageStore *MessageStore, meta types.BasicCallMeta, callType string, isGroup bool, logger waLog.Logger) {
	chatJID := callChatJID(meta)

	fromJID := ""
	switch {
	case !meta.CallCreator.IsEmpty():
		fromJID = meta.CallCreator.ToNonAD().String()
	case !meta.From.IsEmpty():
		fromJID = meta.From.ToNonAD().String()
	}

	isFromMe := client.Store.ID != nil && fromJID == client.Store.ID.ToNonAD().String()

	if err := messageStore.StoreCallOffer(meta.CallID, chatJID, fromJID, meta.Timestamp, isFromMe, callType, isGroup); err != nil {
		logger.Warnf("Failed to store call offer: %v", err)
		return
	}

	kind := "Call"
	if isGroup {
		kind = "Group call"
	}
	direction := "incoming"
	if isFromMe {
		direction = "outgoing"
	}
	logger.Infof("%s %s: id=%s type=%s from=%s chat=%s",
		kind, direction, meta.CallID, callType, fromJID, chatJID)
}

// historyMessageIsStorable mirrors the skip rule in handleHistorySync's store
// loop: text supported by the shared parser or media produces a
// messages row, so anything else must not influence chat ordering.
func historyMessageIsStorable(msg *waProto.HistorySyncMsg) bool {
	if msg == nil || msg.Message == nil || msg.Message.Message == nil {
		return false
	}
	m := msg.Message.Message
	if extractTextContent(m) != "" {
		return true
	}
	mediaType, _, _, _, _, _, _ := extractMediaInfo(m, time.Time{}, "")
	return mediaType != ""
}

// newestStorableHistoryTimestamp returns the timestamp of the newest message in
// a history-sync conversation that will actually be stored, and false when
// none will. It is the history-sync twin of the live-message rule in
// handleMessage: chats.last_message_time only moves for a stored row.
func newestStorableHistoryTimestamp(messages []*waProto.HistorySyncMsg) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, msg := range messages {
		if !historyMessageIsStorable(msg) {
			continue
		}
		ts := msg.Message.GetMessageTimestamp()
		if ts == 0 {
			continue
		}
		t := time.Unix(int64(ts), 0)
		if !found || t.After(newest) {
			newest, found = t, true
		}
	}
	return newest, found
}

func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	// Log every history sync event with its shape. Different sync types
	// carry different payloads; logging type/chunk/progress makes it easy
	// to reason about what arrived from WhatsApp when debugging.
	logger.Infof("Received history sync: type=%s chunk=%d progress=%d conversations=%d",
		historySync.Data.GetSyncType(),
		historySync.Data.GetChunkOrder(),
		historySync.Data.GetProgress(),
		len(historySync.Data.Conversations),
	)

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		rawChatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(rawChatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", rawChatJID, err)
			continue
		}

		// Resolve LID-based chats to phone-based JIDs.
		// History sync doesn't carry SenderAlt, so rely on the
		// LID store mapping populated during live message handling.
		resolved := resolveLIDChat(client, jid, types.EmptyJID, types.EmptyJID, false)
		chatJID := resolved.String()

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, resolved, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			ts := latestMsg.Message.GetMessageTimestamp()
			if ts == 0 {
				continue
			}
			timestamp := time.Unix(int64(ts), 0)

			// Advance last_message_time only to the newest message that will
			// actually be stored. The conversation's first entry is often a
			// protocol message (revoke, ephemeral-setting change) that the
			// store loop below skips; using its timestamp floated the chat to
			// the top of list_chats with no row behind it.
			if newest, ok := newestStorableHistoryTimestamp(messages); ok {
				_ = messageStore.StoreChat(chatJID, name, newest)
			}
			// Backfill read state only when WhatsApp explicitly reports unread
			// metadata. Sparse history-sync chunks omit UnreadCount; the
			// generated getter then returns 0 and would permanently mark the
			// chat read under the monotonic merge.
			if conversation.UnreadCount != nil &&
				conversation.GetUnreadCount() == 0 &&
				!conversation.GetMarkedAsUnread() {
				if err := messageStore.MarkChatRead(chatJID, timestamp); err != nil {
					logger.Warnf("Failed to backfill read state for %s: %v", chatJID, err)
				}
			}
			if err := messageStore.UpdateChatEphemeralSettings(
				chatJID,
				conversation.GetEphemeralExpiration(),
				conversation.GetEphemeralSettingTimestamp(),
			); err != nil {
				logger.Warnf("Failed to store history sync ephemeral settings for %s: %v", chatJID, err)
			}

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Preserve the same searchable text as the live-message path,
				// including Business templates and media captions.
				content := extractTextContent(msg.Message.Message)

				// Extract media info - pass message timestamp + ID for unique filenames
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				histMsgID := ""
				if msg.Message != nil && msg.Message.Key != nil && msg.Message.Key.ID != nil {
					histMsgID = *msg.Message.Key.ID
				}

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message, timestamp, histMsgID)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender. History-sync rows do not carry SenderAlt,
				// so any LID-based participant is resolved through the
				// whatsmeow LID store (populated during live message handling).
				// Group senders live in the top-level WebMessageInfo.participant
				// field on modern history syncs; Key.Participant is only a legacy
				// fallback. Participant is only consulted for group/broadcast
				// chats — for one-to-one (phone or LID) and newsletter chats
				// the sender is the chat itself. Same order and gating as
				// whatsmeow's ParseWebMessage.
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					var participant string
					if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer && jid.Server != types.NewsletterServer {
						participant = msg.Message.GetParticipant()
						if participant == "" {
							participant = msg.Message.Key.GetParticipant()
						}
					}
					var rawSender types.JID
					switch {
					case isFromMe && client.Store.ID != nil:
						rawSender = client.Store.ID.ToNonAD()
					case participant != "":
						if parsed, perr := types.ParseJID(participant); perr == nil {
							rawSender = parsed
						} else {
							rawSender = types.JID{User: participant}
						}
					default:
						rawSender = jid
					}
					var alt types.JID
					if isFromMe && client.Store.ID != nil {
						alt = client.Store.ID.ToNonAD()
					}
					sender = resolveUserJID(client, rawSender, alt).User
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				ts := msg.Message.GetMessageTimestamp()
				if ts == 0 {
					continue
				}
				msgTimestamp := time.Unix(int64(ts), 0)

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					msgTimestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
					"", // quoted_message_id: history sync does not carry ContextInfo
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							msgTimestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							msgTimestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}
