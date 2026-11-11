package main

import (
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
)

// unwrapMessageContent normalizes raw history-sync envelopes using the same
// whatsmeow parser used for live events. Already-unwrapped messages are unchanged.
func unwrapMessageContent(msg *waProto.Message) *waProto.Message {
	return (&events.Message{RawMessage: msg}).UnwrapRaw().Message
}

// Extract text content from a message.
func extractTextContent(msg *waProto.Message) string {
	msg = unwrapMessageContent(msg)
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// Captions on media messages — surface them as searchable content
	// alongside the media itself. Audio messages don't carry captions.
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}

	// Shared contact cards (vCards) carry no URL/MediaKey — the vCard text is
	// embedded directly in the message rather than fetched from the CDN — so
	// without this branch extractMediaInfo also returns "" for them and the
	// message is silently dropped at the "no content and no media" gate in
	// handleMessage: shared contacts were vanishing entirely.
	if contact := msg.GetContactMessage(); contact != nil {
		if body := formatContactContent(contact.GetDisplayName(), contact.GetVcard()); body != "" {
			return "📇 " + body
		}
	}
	if contacts := msg.GetContactsArrayMessage(); contacts != nil {
		if list := contacts.GetContacts(); len(list) > 0 {
			names := make([]string, 0, len(list))
			for _, c := range list {
				if body := formatContactContent(c.GetDisplayName(), c.GetVcard()); body != "" {
					names = append(names, body)
				}
			}
			if len(names) > 0 {
				return fmt.Sprintf("📇 %d contacts shared: %s", len(names), strings.Join(names, "; "))
			}
		}
	}

	// WhatsApp Business templates arrive hydrated — body lives in
	// HydratedTemplate.HydratedContentText. Without this branch every
	// template-sent message (e.g. WABA Connect Hrms_* notifications)
	// returns "" and the row is silently skipped at the storage gate.
	if tpl := msg.GetTemplateMessage(); tpl != nil {
		if h := tpl.GetHydratedTemplate(); h != nil {
			if t := h.GetHydratedContentText(); t != "" {
				return t
			}
		}
	}
	if btn := msg.GetButtonsMessage(); btn != nil {
		if t := btn.GetContentText(); t != "" {
			return t
		}
		if t := btn.GetText(); t != "" {
			return t
		}
	}
	if ia := msg.GetInteractiveMessage(); ia != nil {
		if body := ia.GetBody(); body != nil {
			if t := body.GetText(); t != "" {
				return t
			}
		}
	}
	if lst := msg.GetListMessage(); lst != nil {
		if t := lst.GetDescription(); t != "" {
			return t
		}
	}
	if br := msg.GetButtonsResponseMessage(); br != nil {
		if t := br.GetSelectedDisplayText(); t != "" {
			return t
		}
	}
	if tbr := msg.GetTemplateButtonReplyMessage(); tbr != nil {
		if t := tbr.GetSelectedDisplayText(); t != "" {
			return t
		}
	}

	return ""
}

// formatContactContent renders a shared contact card as searchable text:
// the display name plus every phone number in the vCard body. All numbers are
// kept (not just the first) because the vCard is the only copy we ever get —
// there is no CDN payload to re-download later. Returns "" when there is
// nothing usable (no display name and no TEL line).
func formatContactContent(displayName, vcard string) string {
	phones := extractVCardPhones(vcard)
	if displayName == "" && len(phones) == 0 {
		return ""
	}
	if len(phones) > 0 {
		return fmt.Sprintf("%s (%s)", displayName, strings.Join(phones, ", "))
	}
	return displayName
}

// extractVCardPhones returns the values of all TEL lines in a vCard blob,
// e.g. "TEL;type=CELL;waid=6281234567890:+62 812-3456-7890" -> "+62 812-3456-7890".
// iPhone-exported vCards wrap properties in groups ("item1.TEL;...:+62 ..."),
// so the property name is compared after stripping any group prefix.
func extractVCardPhones(vcard string) []string {
	var phones []string
	// vCard continuation lines remove the newline and one leading whitespace.
	// Unfold before splitting properties so TEL values cannot be truncated.
	vcard = strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "").Replace(vcard)
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		prop := line
		if i := strings.IndexAny(prop, ";:"); i != -1 {
			prop = prop[:i]
		}
		if dot := strings.LastIndex(prop, "."); dot != -1 {
			prop = prop[dot+1:]
		}
		if !strings.EqualFold(prop, "TEL") {
			continue
		}
		if idx := strings.LastIndex(line, ":"); idx != -1 {
			if phone := strings.TrimSpace(line[idx+1:]); phone != "" {
				phones = append(phones, phone)
			}
		}
	}
	return phones
}

// Extract quoted message info from ContextInfo
func extractQuotedMessageInfo(msg *waProto.Message) (quotedMessageId string, quotedSender string, quotedContent string) {
	if msg == nil {
		return "", "", ""
	}

	var contextInfo *waProto.ContextInfo

	// Check all message types that can have ContextInfo
	if extText := msg.GetExtendedTextMessage(); extText != nil {
		contextInfo = extText.GetContextInfo()
	} else if img := msg.GetImageMessage(); img != nil {
		contextInfo = img.GetContextInfo()
	} else if vid := msg.GetVideoMessage(); vid != nil {
		contextInfo = vid.GetContextInfo()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		contextInfo = doc.GetContextInfo()
	} else if aud := msg.GetAudioMessage(); aud != nil {
		contextInfo = aud.GetContextInfo()
	} else if contact := msg.GetContactMessage(); contact != nil {
		contextInfo = contact.GetContextInfo()
	} else if contacts := msg.GetContactsArrayMessage(); contacts != nil {
		contextInfo = contacts.GetContextInfo()
	}

	if contextInfo == nil {
		return "", "", ""
	}

	// Extract quoted message ID (StanzaID)
	if contextInfo.StanzaID != nil {
		quotedMessageId = *contextInfo.StanzaID
	}

	// Extract quoted sender (Participant)
	if contextInfo.Participant != nil {
		quotedSender = *contextInfo.Participant
	}

	// Extract quoted message content
	if quotedMsg := contextInfo.QuotedMessage; quotedMsg != nil {
		quotedContent = extractTextContent(quotedMsg)
	}

	return quotedMessageId, quotedSender, quotedContent
}

// extractMentionedJIDs returns native WhatsApp mention targets from ContextInfo.
func extractMentionedJIDs(msg *waProto.Message) []string {
	if msg == nil {
		return nil
	}

	var contextInfo *waProto.ContextInfo
	if extText := msg.GetExtendedTextMessage(); extText != nil {
		contextInfo = extText.GetContextInfo()
	} else if img := msg.GetImageMessage(); img != nil {
		contextInfo = img.GetContextInfo()
	} else if vid := msg.GetVideoMessage(); vid != nil {
		contextInfo = vid.GetContextInfo()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		contextInfo = doc.GetContextInfo()
	} else if aud := msg.GetAudioMessage(); aud != nil {
		contextInfo = aud.GetContextInfo()
	} else if contact := msg.GetContactMessage(); contact != nil {
		contextInfo = contact.GetContextInfo()
	} else if contacts := msg.GetContactsArrayMessage(); contacts != nil {
		contextInfo = contacts.GetContextInfo()
	}

	if contextInfo == nil || len(contextInfo.MentionedJID) == 0 {
		return nil
	}

	return append([]string(nil), contextInfo.MentionedJID...)
}

// Extract media info from a message. Filenames embed the message ID so that
// two messages arriving in the same second do not collide on a single file.
func extractMediaInfo(msg *waProto.Message, msgTimestamp time.Time, msgID string) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	msg = unwrapMessageContent(msg)
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Use message timestamp for filename, fallback to current time if zero
	ts := msgTimestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	tsStr := ts.Format("20060102_150405")
	suffix := tsStr
	if msgID != "" {
		suffix = tsStr + "_" + msgID
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + suffix + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + suffix + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + suffix + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + suffix
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	// Sticker message: WebP image, no caption, same URL+MediaKey+SHA shape as other media.
	// On the wire stickers surface as type="media" with an <enc mediatype="sticker"> payload, e.g.:
	//   <message id="..." type="media">
	//     <enc mediatype="sticker" type="msg" v="2"><!-- 660 bytes --></enc>
	//   </message>
	if stk := msg.GetStickerMessage(); stk != nil {
		return "sticker", "sticker_" + suffix + ".webp",
			stk.GetURL(), stk.GetMediaKey(), stk.GetFileSHA256(), stk.GetFileEncSHA256(), stk.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}
