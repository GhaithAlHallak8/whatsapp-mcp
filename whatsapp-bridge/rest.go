package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Start a REST API server to expose the WhatsApp client functionality.
//
// Auth: every handler is wrapped in withAuth, which enforces both a
// bearer-token check and a Host-header allow-list (loopback only). See
// auth.go for the rationale.
//
// Outbound media: req.MediaPath in /api/send is validated against
// allowedMediaRoots before sendWhatsAppMessage ever sees it. See
// media_path.go.
func newRESTMux(client *whatsmeow.Client, messageStore *MessageStore, port int, token string, allowedMediaRoots []string, connections ...*connectionManager) *http.ServeMux {
	allowedHosts := buildAllowedHosts(port)
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return withAuth(token, allowedHosts, h)
	}
	mux := http.NewServeMux()
	if len(connections) > 0 {
		connections[0].register(mux, auth)
	}

	// On-demand history sync endpoint (see history_ondemand.go)
	registerHistoryEndpoint(mux, auth, client, messageStore)

	// Health check endpoint
	mux.HandleFunc("/api/health", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := map[string]interface{}{
			"status":    "ok",
			"connected": client.IsConnected(),
			"timestamp": time.Now().Unix(),
		}
		if len(connections) > 0 {
			snapshot := connections[0].snapshot()
			status["connected"] = snapshot.Connected
			status["state"] = snapshot.State
			status["paired"] = snapshot.Paired
			status["message"] = snapshot.Message
			status["lastError"] = snapshot.LastError
		}
		if !status["connected"].(bool) {
			status["status"] = "disconnected"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(status)
	}))

	// Handler for sending messages
	mux.HandleFunc("/api/send", auth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		fmt.Printf("→ /api/send from=%q user_agent=%q\n", r.RemoteAddr, r.UserAgent())

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		// Validate and canonicalize media_path against the configured roots
		// before reading. This prevents the bridge from being used as a
		// generic file-read primitive (e.g. media_path=/Users/x/.ssh/id_rsa).
		resolvedMediaPath := req.MediaPath
		if req.MediaPath != "" {
			canonical, mpErr := validateMediaPath(req.MediaPath, allowedMediaRoots)
			if mpErr != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(SendMessageResponse{
					Success: false,
					Message: fmt.Sprintf("media_path rejected: %v", mpErr),
				})
				return
			}
			resolvedMediaPath = canonical
		}

		// Avoid logging req.Message verbatim — it's user content and may
		// contain secrets the user pasted into a chat.
		fmt.Printf("→ /api/send recipient=%q message_len=%d has_media=%v\n",
			req.Recipient, len(req.Message), resolvedMediaPath != "")

		// Send the message
		success, message := sendWhatsAppMessage(client, messageStore, req.Recipient, req.Message, resolvedMediaPath, req.QuotedMessageID, req.QuotedSenderJID, req.QuotedContent, req.Mentions)
		fmt.Printf("← /api/send success=%v status=%q\n", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		_ = json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	}))

	// Handler for explicitly sending read receipts for selected messages.
	mux.HandleFunc("/api/mark-read", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.ChatJID == "" || len(req.MessageIDs) == 0 {
			http.Error(w, "chat_jid and message_ids are required", http.StatusBadRequest)
			return
		}

		messageIDs := make([]types.MessageID, len(req.MessageIDs))
		for i, id := range req.MessageIDs {
			if strings.TrimSpace(id) == "" {
				http.Error(w, "message_ids must not contain empty values", http.StatusBadRequest)
				return
			}
			messageIDs[i] = types.MessageID(id)
		}

		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			http.Error(w, "Invalid chat_jid", http.StatusBadRequest)
			return
		}

		senderJID := types.EmptyJID
		if req.SenderJID != "" {
			if strings.Contains(req.SenderJID, "@") {
				senderJID, err = types.ParseJID(req.SenderJID)
			} else {
				senderJID = types.NewJID(strings.TrimSpace(req.SenderJID), types.DefaultUserServer)
			}
			if err != nil || senderJID.User == "" || senderJID.Server == "" {
				http.Error(w, "Invalid sender_jid", http.StatusBadRequest)
				return
			}
		} else if chatJID.Server == types.GroupServer {
			http.Error(w, "sender_jid is required for group read receipts", http.StatusBadRequest)
			return
		}

		readAt := time.Now()
		if req.Timestamp != "" {
			readAt, err = time.Parse(time.RFC3339, req.Timestamp)
			if err != nil {
				http.Error(w, "timestamp must be RFC 3339", http.StatusBadRequest)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if !client.IsConnected() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(SendMessageResponse{
				Success: false,
				Message: "WhatsApp client is not connected. Please wait for reconnection.",
			})
			return
		}

		// Validate against the storage (phone-form) chat JID before any
		// external side effect. LID rewrite happens only for the receipt.
		if err := messageStore.ValidateInboundMarkRead(req.ChatJID, req.SenderJID, req.MessageIDs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// MCP storage normalizes chats/senders to phone JIDs; MarkRead routes
		// the receipt `to`/`participant` as given, so resolve PN -> LID the
		// same way sendWhatsAppMessage does or migrated contacts silently fail.
		chatJID, err = resolveRecipientJID(client, req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			http.Error(w, "Invalid chat_jid", http.StatusBadRequest)
			return
		}
		if req.SenderJID != "" {
			senderJID, err = resolveRecipientJID(client, req.SenderJID)
			if err != nil || senderJID.User == "" || senderJID.Server == "" {
				http.Error(w, "Invalid sender_jid", http.StatusBadRequest)
				return
			}
		}

		if err := client.MarkRead(context.Background(), messageIDs, readAt, chatJID, senderJID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: err.Error()})
			return
		}

		// Advance the local read marker immediately so list_chats unread
		// clears without waiting for the self-read receipt round-trip.
		localReadAt := readAt
		if ts, ok, tsErr := messageStore.MaxMessageTimestamp(req.ChatJID, req.MessageIDs); tsErr == nil && ok {
			localReadAt = ts
		}
		if err := messageStore.MarkChatRead(req.ChatJID, localReadAt); err != nil {
			// Receipt already sent; log but still report success to the caller.
			fmt.Printf("Warning: failed to persist local read marker for %s: %v\n", req.ChatJID, err)
		}

		_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: true, Message: "Messages marked as read"})
	}))

	// Handler for sending (or removing) emoji reactions
	mux.HandleFunc("/api/react", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req ReactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Recipient == "" || req.MessageID == "" || req.Emoji == nil {
			http.Error(w, "recipient, message_id, and emoji are required", http.StatusBadRequest)
			return
		}
		chatJID, err := types.ParseJID(req.Recipient)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid recipient JID: %v", err), http.StatusBadRequest)
			return
		}
		var senderJID types.JID
		switch {
		case req.FromMe:
			if client.Store.ID == nil {
				http.Error(w, "Not logged in", http.StatusServiceUnavailable)
				return
			}
			senderJID = *client.Store.ID
		case req.SenderJID != "":
			if senderJID, err = types.ParseJID(req.SenderJID); err != nil {
				http.Error(w, fmt.Sprintf("Invalid sender_jid: %v", err), http.StatusBadRequest)
				return
			}
			if senderJID.User == "" || senderJID.Server == "" {
				http.Error(w, "Invalid sender_jid", http.StatusBadRequest)
				return
			}
		default:
			if chatJID.Server == types.GroupServer {
				http.Error(w, "sender_jid is required for group reactions when from_me is false", http.StatusBadRequest)
				return
			}
			senderJID = chatJID
		}
		msg := client.BuildReaction(chatJID, senderJID, req.MessageID, *req.Emoji)
		w.Header().Set("Content-Type", "application/json")
		if _, err := client.SendMessage(context.Background(), chatJID, msg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))

	// Handler for downloading media
	mux.HandleFunc("/api/download", auth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Check if connected
		if !client.IsConnected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: "WhatsApp client is not connected. Please wait for reconnection.",
			})
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Log download request for debugging
		fmt.Printf("📥 Download request: message_id=%s chat_jid=%s\n", req.MessageID, req.ChatJID)

		// Download the media
		success, mediaType, filename, path, err := downloadMediaForRequest(client, messageStore, req)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if errors.Is(err, errMediaRetryRequested) || errors.Is(err, errMediaGone) || errors.Is(err, errMediaExpired) {
			status := "retry_requested"
			if errors.Is(err, errMediaGone) {
				status = "gone"
			} else if errors.Is(err, errMediaExpired) {
				status = "expired"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error(), "status": status})
			return
		}
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	}))

	// Handler for sending typing indicator
	mux.HandleFunc("/api/transcribe/backfill", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if autoTranscribe == nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "Auto-transcription is off. Set WHATSAPP_AUTO_TRANSCRIBE to voice or audio."})
			return
		}
		var req struct {
			After   string `json:"after"`
			Before  string `json:"before"`
			ChatJID string `json:"chat_jid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		after, err := time.Parse(time.RFC3339, req.After)
		if err != nil {
			http.Error(w, "after must be an RFC 3339 timestamp", http.StatusBadRequest)
			return
		}
		before := time.Now()
		if req.Before != "" {
			if before, err = time.Parse(time.RFC3339, req.Before); err != nil {
				http.Error(w, "before must be an RFC 3339 timestamp", http.StatusBadRequest)
				return
			}
		}
		refs, err := messageStore.UntranscribedAudio(after, before, req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "Failed to list audio messages"})
			return
		}
		go autoTranscribe.backfill(refs, func(messageID, chatJID string) (string, error) {
			success, _, _, path, err := downloadMediaWithRecovery(client, messageStore, messageID, chatJID)
			if err != nil {
				return "", err
			}
			if !success {
				return "", fmt.Errorf("download failed")
			}
			return path, nil
		})
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "candidates": len(refs)})
	}))

	mux.HandleFunc("/api/typing", auth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req struct {
			Recipient string `json:"recipient"`
			IsTyping  bool   `json:"is_typing"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		// Create JID for recipient
		var recipientJID types.JID
		var err error

		// Check if recipient is a JID
		if strings.Contains(req.Recipient, "@") {
			recipientJID, err = types.ParseJID(req.Recipient)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"message": fmt.Sprintf("Error parsing JID: %v", err),
				})
				return
			}
		} else {
			// Create JID from phone number
			recipientJID = types.JID{
				User:   req.Recipient,
				Server: "s.whatsapp.net",
			}
		}

		// Determine the chat presence state
		var state types.ChatPresence
		if req.IsTyping {
			state = types.ChatPresenceComposing
		} else {
			state = types.ChatPresencePaused
		}

		// Send the chat presence update
		err = client.SendChatPresence(context.Background(), recipientJID, state, types.ChatPresenceMediaText)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Send response
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("Failed to send typing indicator: %v", err),
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": fmt.Sprintf("Typing indicator set to %v", req.IsTyping),
			})
		}
	}))

	return mux
}

// Requests use the current client after a revoked device is replaced. Existing
// in-flight requests can finish against the already disconnected previous client.
func newManagedRESTMux(client *whatsmeow.Client, messageStore *MessageStore, port int, token string, allowedMediaRoots []string, connection *connectionManager) http.Handler {
	var cacheMu sync.Mutex
	cachedClient := client
	cachedMux := newRESTMux(client, messageStore, port, token, allowedMediaRoots, connection)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := connection.client.Load()
		cacheMu.Lock()
		if current != cachedClient {
			cachedClient = current
			cachedMux = newRESTMux(current, messageStore, port, token, allowedMediaRoots, connection)
		}
		handler := cachedMux
		cacheMu.Unlock()
		handler.ServeHTTP(w, r)
	})
}

func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int, token string, allowedMediaRoots []string, connection *connectionManager) {
	handler := newManagedRESTMux(client, messageStore, port, token, allowedMediaRoots, connection)

	// Start the server with proper timeouts. Bind to loopback so the bridge is
	// not reachable from the LAN; MCP clients talk to it over localhost.
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Create server with timeouts for stability
	server := &http.Server{
		Addr:         serverAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second, // Longer for media downloads
		IdleTimeout:  120 * time.Second,
		Handler:      handler,
	}

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}
