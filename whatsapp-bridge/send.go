package main

import (
	"context"
	"database/sql"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient       string `json:"recipient"`
	Message         string `json:"message"`
	MediaPath       string `json:"media_path,omitempty"`
	QuotedMessageID string `json:"quoted_message_id,omitempty"`
	QuotedSenderJID string `json:"quoted_sender_jid,omitempty"`
	QuotedContent   string `json:"quoted_content,omitempty"`
	// Mentions lists users to @-mention (phone numbers or JIDs). The message
	// text must contain a matching "@<number>" token for each entry, or the
	// mention won't render on recipients' devices.
	Mentions []string `json:"mentions,omitempty"`
}

// MarkReadRequest is the request body for the /api/mark-read endpoint.
type MarkReadRequest struct {
	MessageIDs []string `json:"message_ids"`
	ChatJID    string   `json:"chat_jid"`
	SenderJID  string   `json:"sender_jid,omitempty"`
	Timestamp  string   `json:"timestamp,omitempty"`
}

// ReactRequest is the request body for the /api/react endpoint.
type ReactRequest struct {
	Recipient string  `json:"recipient"`  // chat JID
	MessageID string  `json:"message_id"` // ID of the message being reacted to
	FromMe    bool    `json:"from_me"`    // whether the reacted-to message was sent by us
	SenderJID string  `json:"sender_jid"` // full JID of the reacted-to message's sender
	Emoji     *string `json:"emoji"`      // reaction emoji; empty string removes the reaction
}

// classifyMediaPath maps a file extension to (whatsmeow upload type, MIME
// type, persist-side category). Single source of truth for the upload path
// (which needs the whatsmeow.MediaType + MIME) and the SQLite persist path
// (which stores the short category string).
func classifyMediaPath(mediaPath string) (whatsmeow.MediaType, string, string) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(mediaPath), "."))
	switch ext {
	case "jpg", "jpeg":
		return whatsmeow.MediaImage, "image/jpeg", "image"
	case "png":
		return whatsmeow.MediaImage, "image/png", "image"
	case "gif":
		return whatsmeow.MediaImage, "image/gif", "image"
	case "webp":
		return whatsmeow.MediaImage, "image/webp", "image"
	case "ogg":
		return whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "audio"
	case "mp4":
		return whatsmeow.MediaVideo, "video/mp4", "video"
	case "avi":
		return whatsmeow.MediaVideo, "video/avi", "video"
	case "mov":
		return whatsmeow.MediaVideo, "video/quicktime", "video"
	default:
		if m := mime.TypeByExtension("." + ext); m != "" {
			return whatsmeow.MediaDocument, m, "document"
		}
		return whatsmeow.MediaDocument, "application/octet-stream", "document"
	}
}

// outboundMediaRow derives the columns sendWhatsAppMessage persists for an
// outbound message: the media category (via classifyMediaPath, so it can't
// drift from the upload-side classification), the filename, and the upload
// metadata downloadMedia needs to redownload it later. Returns zero values
// for a plain text message (mediaPath == ""). Split out as its own function
// so it's unit-testable without a live whatsmeow.Client.
func outboundMediaRow(mediaPath string, upload whatsmeow.UploadResponse) (mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) {
	if mediaPath == "" {
		return "", "", "", nil, nil, nil, 0
	}
	_, _, mediaType = classifyMediaPath(mediaPath)
	filename = filepath.Base(mediaPath)
	return mediaType, filename, upload.URL, upload.MediaKey, upload.FileSHA256, upload.FileEncSHA256, upload.FileLength
}

// resolveRecipientJID parses a phone number or JID string and resolves PN -> LID
// for personal chats before sending.
func resolveRecipientJID(client *whatsmeow.Client, recipient string) (types.JID, error) {
	var recipientJID types.JID
	var err error

	if strings.Contains(recipient, "@") {
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return types.JID{}, fmt.Errorf("Error parsing JID: %v", err)
		}
	} else {
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	// For personal chats, resolve phone number JID to LID (Linked Identity).
	// WhatsApp is migrating to LID-based addressing; messages sent to the
	// phone JID silently fail for migrated contacts.
	if recipientJID.Server == types.DefaultUserServer {
		ctx := context.Background()
		lid, lidErr := client.Store.LIDs.GetLIDForPN(ctx, recipientJID)
		if lidErr == nil && !lid.IsEmpty() {
			fmt.Printf("Resolved %s -> %s (LID)\n", recipientJID, lid)
			recipientJID = lid
		} else {
			// Cache miss or cache error — ask the WhatsApp server.
			if lidErr != nil {
				fmt.Printf("Warning: LID cache lookup failed for %s: %v, falling back to server\n", recipientJID, lidErr)
			}
			info, infoErr := client.GetUserInfo(ctx, []types.JID{recipientJID})
			if infoErr != nil {
				fmt.Printf("Warning: server LID lookup failed for %s: %v\n", recipientJID, infoErr)
			} else if userInfo, ok := info[recipientJID]; ok && !userInfo.LID.IsEmpty() {
				fmt.Printf("Resolved %s -> %s (LID via server)\n", recipientJID, userInfo.LID)
				recipientJID = userInfo.LID
			}
		}
	}

	return recipientJID, nil
}

// resolveMentionJIDs maps mention entries (phone numbers or JIDs) to the JID
// strings WhatsApp expects in ContextInfo.MentionedJID. Phone-number entries
// contribute both the phone JID and, when known, its LID form so the mention
// renders regardless of the group's addressing mode.
func resolveMentionJIDs(client *whatsmeow.Client, mentions []string) []string {
	var resolved []string
	for _, mention := range mentions {
		var jid types.JID
		if strings.Contains(mention, "@") {
			parsed, err := types.ParseJID(mention)
			if err != nil {
				fmt.Printf("Warning: skipping unparseable mention %q: %v\n", mention, err)
				continue
			}
			jid = parsed
		} else {
			jid = types.JID{User: mention, Server: types.DefaultUserServer}
		}
		resolved = append(resolved, jid.String())
		if jid.Server == types.DefaultUserServer {
			if lid, err := client.Store.LIDs.GetLIDForPN(context.Background(), jid); err == nil && !lid.IsEmpty() {
				resolved = append(resolved, lid.String())
			}
		}
	}
	return resolved
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string, quotedMsgID string, quotedSenderJID string, quotedContent string, mentions []string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	mentionedJIDs := resolveMentionJIDs(client, mentions)

	var settingsLookupJID types.JID
	var err error

	if strings.Contains(recipient, "@") {
		settingsLookupJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		settingsLookupJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	// Capture pre-LID-resolution JID for SQLite storage.
	// handleMessage uses resolveLIDChat to map LID→phone for incoming events;
	// for outbound we keep the pre-resolution form so the chat stays unified
	// under @s.whatsapp.net (matches what list_chats / list_messages expect).
	storageJID := settingsLookupJID

	recipientJID, err := resolveRecipientJID(client, recipient)
	if err != nil {
		return false, err.Error()
	}

	msg := &waProto.Message{}

	// Populated below when mediaPath != "", then reused both to build the
	// outgoing proto and to persist media metadata further down.
	var upload whatsmeow.UploadResponse

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		mediaType, mimeType, _ := classifyMediaPath(mediaPath)

		// Upload media to WhatsApp servers
		upload, err = client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		// Don't log the struct itself — UploadResponse carries the MediaKey
		// and a signed CDN URL with authentication query parameters.
		fmt.Printf("Media uploaded (%d bytes)\n", upload.FileLength)

		// downloadMedia reconstructs DirectPath from the stored URL instead
		// of persisting it directly (no column for it). extractDirectPathFromURL
		// keeps the URL's query string on purpose (the CDN auth tokens); strip
		// it from both sides before comparing so this only fires on an actual
		// mismatch, not the query string that's expected to differ.
		derivedPath, _, _ := strings.Cut(extractDirectPathFromURL(upload.URL), "?")
		directPath, _, _ := strings.Cut(upload.DirectPath, "?")
		if derivedPath != directPath {
			fmt.Printf("Warning: upload path does not match derived download path; redownload of this attachment may fail\n")
		}

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &upload.URL,
				DirectPath:    &upload.DirectPath,
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    &upload.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &upload.URL,
				DirectPath:    &upload.DirectPath,
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    &upload.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &upload.URL,
				DirectPath:    &upload.DirectPath,
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    &upload.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				// outboundFileName, not a manual split on "/": the document
				// filename travels to the recipient, and on Windows the path
				// is already backslash-normalised, so the naive split leaks
				// the whole absolute path. See media_path.go.
				Title:         proto.String(outboundFileName(mediaPath)),
				FileName:      proto.String(outboundFileName(mediaPath)),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &upload.URL,
				DirectPath:    &upload.DirectPath,
				MediaKey:      upload.MediaKey,
				FileEncSHA256: upload.FileEncSHA256,
				FileSHA256:    upload.FileSHA256,
				FileLength:    &upload.FileLength,
			}
		default:
			// Unreachable today (classifyMediaPath only returns the four
			// types above), but fail loudly rather than send an empty proto
			// while still persisting media metadata for it.
			return false, fmt.Sprintf("Unsupported media type for %s", mediaPath)
		}
	} else if quotedMsgID != "" || len(mentionedJIDs) > 0 {
		// Quoted reply and/or mentions: use ExtendedTextMessage so we can
		// attach ContextInfo. Only text quoting is supported; quoting media
		// messages is not exposed because the quoted preview on the
		// recipient's device requires the original media's key/URL, which is
		// not available to the API caller.
		ctx := &waProto.ContextInfo{}
		if quotedMsgID != "" {
			ctx.StanzaID = proto.String(quotedMsgID)
			ctx.Participant = proto.String(quotedSenderJID)
			ctx.QuotedMessage = &waProto.Message{Conversation: proto.String(quotedContent)}
		}
		ctx.MentionedJID = mentionedJIDs
		msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{
			Text:        proto.String(message),
			ContextInfo: ctx,
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Mentions in media captions live on the media message's own ContextInfo.
	if len(mentionedJIDs) > 0 {
		switch {
		case msg.ImageMessage != nil:
			msg.ImageMessage.ContextInfo = &waProto.ContextInfo{MentionedJID: mentionedJIDs}
		case msg.VideoMessage != nil:
			msg.VideoMessage.ContextInfo = &waProto.ContextInfo{MentionedJID: mentionedJIDs}
		case msg.DocumentMessage != nil:
			msg.DocumentMessage.ContextInfo = &waProto.ContextInfo{MentionedJID: mentionedJIDs}
		}
	}

	// Normalize @lid recipients to phone JID before the lookup. Chats are
	// persisted under @s.whatsapp.net (handleMessage normalizes via
	// resolveLIDChat); without this step, an API caller passing an @lid
	// recipient would silently miss the disappearing-message settings row.
	settings, err := messageStore.GetChatEphemeralSettings(resolveUserJID(client, settingsLookupJID, types.EmptyJID).String())
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Sprintf("Error loading chat settings: %v", err)
	}
	if err == nil {
		applyChatEphemeralSettings(msg, settings)
	}

	// Send message
	resp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	// whatsmeow does not re-emit events.Message for messages this client
	// itself just sent, so without an explicit StoreMessage call here
	// list_messages / get_last_interaction never see our own outbound
	// traffic until WhatsApp's multi-device sync echoes them back.
	if messageStore != nil && client.Store != nil && client.Store.ID != nil {
		// Normalize @lid recipients to phone JID so outbound rows land in
		// the same chat row as inbound (which handleMessage normalizes via
		// resolveLIDChat). Otherwise sending to an @lid input would
		// fragment the chat under a separate jid.
		persistJID := resolveUserJID(client, storageJID, types.EmptyJID)
		chatJID := persistJID.String()
		senderUser := client.Store.ID.User
		timestamp := resp.Timestamp
		if timestamp.IsZero() {
			timestamp = time.Now()
		}

		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := outboundMediaRow(mediaPath, upload)

		// Pass empty name so StoreChat preserves any existing resolved
		// contact/group name; we don't have one available here and
		// must not clobber names from inbound handling or history sync.
		if chatErr := messageStore.StoreChat(chatJID, "", timestamp); chatErr != nil {
			fmt.Printf("Warning: failed to store outbound chat metadata: %v\n", chatErr)
		}
		if storeErr := messageStore.StoreMessage(
			resp.ID, chatJID, senderUser, message, timestamp, true,
			mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, quotedMsgID,
		); storeErr != nil {
			fmt.Printf("Warning: failed to persist outbound message: %v\n", storeErr)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}
