package main

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestHandleHistorySync_StoresQuotedMessageID(t *testing.T) {
	reply := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text:        proto.String("They need you for the reel after"),
		ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("quoted-001")},
	}}
	cases := map[string]*waProto.Message{
		"plain":     reply,
		"ephemeral": {EphemeralMessage: &waProto.FutureProofMessage{Message: reply}},
	}
	for name, message := range cases {
		t.Run(name, func(t *testing.T) {
			ms := newTestMessageStore(t)
			client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
			chatJID := phonePN.String()
			handleHistorySync(client, ms, &events.HistorySync{Data: &waProto.HistorySync{
				SyncType: waProto.HistorySync_RECENT.Enum(),
				Conversations: []*waProto.Conversation{{
					ID: proto.String(chatJID),
					Messages: []*waProto.HistorySyncMsg{
						{Message: &waProto.WebMessageInfo{
							Key:              &waCommon.MessageKey{ID: proto.String("reply-001"), FromMe: proto.Bool(false)},
							MessageTimestamp: proto.Uint64(1700000100),
							Message:          message,
						}},
						{Message: &waProto.WebMessageInfo{
							Key:              &waCommon.MessageKey{ID: proto.String("plain-001"), FromMe: proto.Bool(false)},
							MessageTimestamp: proto.Uint64(1700000000),
							Message:          &waProto.Message{Conversation: proto.String("no quote")},
						}},
					},
				}},
			}}, testLogger())

			var quoted, plain *string
			if err := ms.db.QueryRow("SELECT quoted_message_id FROM messages WHERE id = 'reply-001'").Scan(&quoted); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT quoted_message_id FROM messages WHERE id = 'plain-001'").Scan(&plain); err != nil {
				t.Fatal(err)
			}
			if quoted == nil || *quoted != "quoted-001" {
				t.Fatalf("quoted_message_id = %v, want quoted-001", quoted)
			}
			if plain != nil {
				t.Fatalf("plain message got quoted_message_id %q", *plain)
			}
		})
	}
}
