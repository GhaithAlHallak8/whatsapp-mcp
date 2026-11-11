package main

import (
	"time"

	_ "github.com/mattn/go-sqlite3"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func buildDisappearingMode() *waProto.DisappearingMode {
	return &waProto.DisappearingMode{
		Initiator: waProto.DisappearingMode_CHANGED_IN_CHAT.Enum(),
		Trigger:   waProto.DisappearingMode_CHAT_SETTING.Enum(),
	}
}

func mergeEphemeralContextInfo(existing *waProto.ContextInfo, settings ChatEphemeralSettings) *waProto.ContextInfo {
	if existing == nil {
		existing = &waProto.ContextInfo{}
	}
	existing.Expiration = proto.Uint32(settings.Expiration)
	existing.EphemeralSettingTimestamp = proto.Int64(settings.SettingTimestamp)
	existing.DisappearingMode = buildDisappearingMode()
	return existing
}

func applyChatEphemeralSettings(msg *waProto.Message, settings ChatEphemeralSettings) {
	if msg == nil || settings.Expiration == 0 || settings.SettingTimestamp == 0 {
		return
	}

	switch {
	case msg.ExtendedTextMessage != nil:
		msg.ExtendedTextMessage.ContextInfo = mergeEphemeralContextInfo(msg.ExtendedTextMessage.GetContextInfo(), settings)
	case msg.ImageMessage != nil:
		msg.ImageMessage.ContextInfo = mergeEphemeralContextInfo(msg.ImageMessage.GetContextInfo(), settings)
	case msg.AudioMessage != nil:
		msg.AudioMessage.ContextInfo = mergeEphemeralContextInfo(msg.AudioMessage.GetContextInfo(), settings)
	case msg.VideoMessage != nil:
		msg.VideoMessage.ContextInfo = mergeEphemeralContextInfo(msg.VideoMessage.GetContextInfo(), settings)
	case msg.DocumentMessage != nil:
		msg.DocumentMessage.ContextInfo = mergeEphemeralContextInfo(msg.DocumentMessage.GetContextInfo(), settings)
	case msg.Conversation != nil:
		text := msg.GetConversation()
		msg.Conversation = nil
		msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: mergeEphemeralContextInfo(nil, settings),
		}
	}
}

// extractChatEphemeralFromMessage reads the chat's ephemeral state off an
// inbound message's ContextInfo. Every regular message in an ephemeral chat
// stamps Expiration / EphemeralSettingTimestamp on the sub-message's
// ContextInfo, which lets the bridge backfill chats whose disappearing state
// was set before the bridge ever saw an EPHEMERAL_SETTING toggle or a
// fresh history sync. Returns the zero ChatEphemeralSettings when no
// ContextInfo is present (e.g. plain Conversation, ProtocolMessage).
func extractChatEphemeralFromMessage(msg *waProto.Message) ChatEphemeralSettings {
	if msg == nil {
		return ChatEphemeralSettings{}
	}
	var ctx *waProto.ContextInfo
	switch {
	case msg.ExtendedTextMessage != nil:
		ctx = msg.ExtendedTextMessage.GetContextInfo()
	case msg.ImageMessage != nil:
		ctx = msg.ImageMessage.GetContextInfo()
	case msg.AudioMessage != nil:
		ctx = msg.AudioMessage.GetContextInfo()
	case msg.VideoMessage != nil:
		ctx = msg.VideoMessage.GetContextInfo()
	case msg.DocumentMessage != nil:
		ctx = msg.DocumentMessage.GetContextInfo()
	case msg.StickerMessage != nil:
		ctx = msg.StickerMessage.GetContextInfo()
	case msg.ContactMessage != nil:
		ctx = msg.ContactMessage.GetContextInfo()
	case msg.ContactsArrayMessage != nil:
		ctx = msg.ContactsArrayMessage.GetContextInfo()
	}
	if ctx == nil {
		return ChatEphemeralSettings{}
	}
	return ChatEphemeralSettings{
		Expiration:       ctx.GetExpiration(),
		SettingTimestamp: ctx.GetEphemeralSettingTimestamp(),
	}
}

func updateChatEphemeralSettingsFromProtocolMessage(messageStore *MessageStore, chatJID string, msg *waProto.Message, eventTimestamp int64, logger waLog.Logger) {
	if msg == nil || msg.GetProtocolMessage() == nil {
		return
	}

	protoMsg := msg.GetProtocolMessage()
	if protoMsg.GetType() != waProto.ProtocolMessage_EPHEMERAL_SETTING {
		return
	}

	expiration := protoMsg.GetEphemeralExpiration()
	settingTimestamp := protoMsg.GetEphemeralSettingTimestamp()
	// Fall back to the carrier event's timestamp rather than time.Now() so a
	// late-arriving older event doesn't get stamped "newer than" subsequent
	// updates and then block them via the monotonic WHERE clause in
	// UpdateChatEphemeralSettings.
	if settingTimestamp == 0 {
		settingTimestamp = eventTimestamp
	}

	if err := messageStore.UpdateChatEphemeralSettings(chatJID, expiration, settingTimestamp); err != nil {
		logger.Warnf("Failed to update ephemeral settings for %s: %v", chatJID, err)
	}
}

// handleMessageRevoke records a "delete for everyone" event by stamping
// deleted_at on the target message row. The original content is kept on
// purpose so the local archive can still surface what was retracted.
//
// chatJID is the already-LID-normalised chat from the carrier event;
// using it (rather than Key.RemoteJID, which may carry the raw @lid
// form) keeps the UPDATE aligned with how StoreMessage wrote the row.
func handleMessageRevoke(messageStore *MessageStore, msg *waProto.Message, chatJID string, eventTimestamp int64, logger waLog.Logger) {
	if msg == nil || msg.GetProtocolMessage() == nil {
		return
	}
	protoMsg := msg.GetProtocolMessage()
	if protoMsg.GetType() != waProto.ProtocolMessage_REVOKE {
		return
	}
	key := protoMsg.GetKey()
	if key == nil {
		return
	}
	targetID := key.GetID()
	if targetID == "" {
		return
	}
	deletedAt := time.Unix(eventTimestamp, 0)
	if err := messageStore.MarkMessageDeleted(targetID, chatJID, deletedAt); err != nil {
		logger.Warnf("Failed to mark message %s in %s as deleted: %v", targetID, chatJID, err)
	}
}
