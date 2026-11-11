package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Whether to forward messages sent by self via webhook.
// Defaults to true. Override with env FORWARD_SELF=false.
var forwardSelfMessages = getEnvBool("FORWARD_SELF", true)

// autoDownloadMediaEnabled reports whether incoming media is downloaded
// automatically, including images for webhooks. A false value leaves media to
// on-demand /api/download calls. Read per call so tests can toggle it.
func autoDownloadMediaEnabled() bool {
	return getEnvBool("WHATSAPP_AUTO_DOWNLOAD_MEDIA", true)
}

// CLI flag: request a full history sync at pair time.
// Only meaningful on a fresh pair (whatsapp.db deleted). See the usage block
// near NewClient for the full rationale and caveats.
var fullHistoryPairFlag = flag.Bool("full-history-pair", false,
	"Request full history at pair time (only effective when re-pairing; no-op for existing sessions)")

const whatsmeowDBPath = "store/whatsapp.db"

// getEnvBool reads a boolean env var with a default.
// Accepts: 1/true/yes/on and 0/false/no/off (case-insensitive)
func getEnvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// resolveDeviceName returns the operator-configured linked-device label from
// WHATSAPP_DEVICE_NAME, trimmed of surrounding whitespace. An empty or unset
// value returns "", which callers treat as "keep the whatsmeow default".
func resolveDeviceName() string {
	return strings.TrimSpace(os.Getenv("WHATSAPP_DEVICE_NAME"))
}

func webhookStartupMessage(forwardSelf bool) string {
	if !webhooksEnabled() {
		return "WEBHOOK_ENABLED=false: outbound webhooks disabled"
	}
	if forwardSelf {
		return "FORWARD_SELF enabled: forwarding self messages to webhook"
	}
	return "FORWARD_SELF disabled: self messages will NOT be forwarded"
}

// pairingQROutcome reports how a pairing QR channel ended.
type pairingQROutcome int

const (
	// pairingQRChannelClosed means the channel drained without a verdict.
	pairingQRChannelClosed pairingQROutcome = iota
	// pairingQRSucceeded means the phone completed the handshake.
	pairingQRSucceeded
	// pairingQRTimedOut means the server ran out of codes to offer.
	pairingQRTimedOut
)

// renderPairingQRCodes drains a pairing QR channel and writes every code the
// server offers to w, delegating the actual rendering to renderQR.
//
// Every code has to be rendered, not just the first one. Since WhatsApp's 2026
// companion-registration change the server answers a scan with a
// companion_reg_refresh notification; whatsmeow then rotates the ADV secret
// and emits a fresh code, and only that rotated code can still complete the
// handshake. Showing just the first code leaves a stale one on screen, so
// every later scan is validated against a secret the server has already
// dropped and the phone reports "check your connection" while pairing never
// completes.
func renderPairingQRCodes(qrChan <-chan whatsmeow.QRChannelItem, w io.Writer, renderQR func(code string, w io.Writer)) pairingQROutcome {
	codes := 0
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			codes++
			// Terminal output is best-effort; keep draining pairing events if a write fails.
			if codes == 1 {
				_, _ = fmt.Fprintln(w, "\nScan this QR code with your WhatsApp app:")
			} else {
				_, _ = fmt.Fprintf(w, "\nQR code refreshed (#%d) - scan this one instead:\n", codes)
			}
			renderQR(evt.Code, w)
			_, _ = fmt.Fprintln(w, "\nWaiting for QR code scan...")
		case "success":
			return pairingQRSucceeded
		case "timeout":
			return pairingQRTimedOut
		}
	}
	return pairingQRChannelClosed
}

func main() {
	flag.Parse()

	// Set up logger with DEBUG level for more detailed logging
	logger := waLog.Stdout("Client", "DEBUG", true)
	logger.Infof("Starting WhatsApp client...")

	logger.Infof("%s", webhookStartupMessage(forwardSelfMessages))

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist. Owner-only (0700),
	// same reasoning as NewMessageStore.
	if err := ensureOwnerOnlyDirectory("store"); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Optionally request a full history sync at pair time.
	//
	// whatsmeow's default DeviceProps has RequireFullSync=false, which asks the
	// primary device for "recent" history only (typically ~3 months, decided by
	// the phone). Setting RequireFullSync=true with a large FullSyncDaysLimit
	// flips the handshake to request full-history mode. The phone still decides
	// the actual cap — iPad companion is documented at ~1 year max
	// (https://wabetainfo.com/...). Only meaningful at pair time: for an
	// already-paired session (whatsapp.db present), this is a no-op because no
	// new pair handshake fires.
	//
	// Enable by passing --full-history-pair on the command line BEFORE deleting
	// whatsapp.db and re-scanning the QR code. The flag defaults to false so
	// normal launchd-managed restarts don't accidentally trigger a huge sync.
	if *fullHistoryPairFlag {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
		logger.Infof("--full-history-pair enabled: requesting full history (days=3650, sizeMb=102400)")
	}

	// Set the linked-device label shown in WhatsApp's "Linked Devices" list.
	// whatsmeow's built-in default is the literal string "whatsmeow", which is
	// opaque to end users who then see an unfamiliar name attached to their
	// account. WHATSAPP_DEVICE_NAME lets an operator show a recognisable label
	// (e.g. a product or company name) instead. Empty/unset keeps the whatsmeow
	// default. This only takes effect at pair time — an already-paired session
	// (whatsapp.db present) keeps the name captured when the QR was scanned; to
	// change it, re-pair. The platform icon (DeviceProps.PlatformType) is left
	// at whatsmeow's default on purpose: this is a labelling convenience, not a
	// way to impersonate an official WhatsApp client.
	if name := resolveDeviceName(); name != "" {
		store.DeviceProps.Os = proto.String(name)
		logger.Infof("Linked-device name set to %q (WHATSAPP_DEVICE_NAME)", name)
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer func() { _ = messageStore.Close() }()

	autoTranscribe, err = startAutoTranscriber(os.Getenv, messageStore)
	if err != nil {
		logger.Errorf("Auto-transcription disabled: %v", err)
	} else if autoTranscribe != nil {
		logger.Infof("Auto-transcribing %s messages with %s", autoTranscribe.cfg.mode, autoTranscribe.engine.Source())
	}

	if err := messageStore.MigrateLegacyLIDChatsToPhoneJIDs("store/whatsapp.db", logger); err != nil {
		logger.Errorf("Failed to migrate legacy LID chat rows: %v", err)
		return
	}

	if err := messageStore.MigrateLegacyLIDSendersToPhones("store/whatsapp.db", logger); err != nil {
		logger.Errorf("Failed to migrate legacy LID sender rows: %v", err)
		return
	}

	// Resolve the REST API port. Pure env parsing with no dependency on the
	// WhatsApp connection, so it's safe to do this early alongside the token
	// load below — and failing fast here means we don't run a QR-pairing
	// flow only to error out on an invalid port afterwards.
	port := 8080
	if p := os.Getenv("WHATSAPP_BRIDGE_PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			logger.Errorf("Invalid WHATSAPP_BRIDGE_PORT=%q, must be 1-65535", p)
			return
		}
		port = v
	}

	// Load (or generate on first run) the bearer token used to authenticate
	// REST callers, and attach it to outbound webhook POSTs so the hub's
	// fail-closed inbound-auth middleware accepts them (see auth.go and
	// webhook.go). This MUST happen before the event handler below is
	// registered: WhatsApp can deliver messages — including a burst of
	// history-sync backlog — as soon as the connection succeeds, and any
	// message handled before this assignment would go out with no bridge
	// token attached.
	bridgeToken, fresh, tokErr := loadOrCreateBridgeToken()
	if tokErr != nil {
		logger.Errorf("Failed to initialize bridge token: %v", tokErr)
		return
	}
	webhookAuthToken = bridgeToken

	// Print the one-time setup banner immediately, before attempting to
	// connect/pair. loadOrCreateBridgeToken() already persisted the token to
	// disk as soon as it generated one; if the banner instead waited until
	// after a successful connection (as it used to), a QR-pairing timeout or
	// early exit would leave a token on disk that was never shown to the
	// user — and loadOrCreateBridgeToken() would report fresh=false on every
	// later run, so the banner would never get a second chance to print it.
	if fresh {
		printTokenBanner(bridgeToken, port)
	}

	connection := newConnectionManager(client.Store.ID != nil)
	client.EnableAutoReconnect = false // The controller handles retries, including sustained keepalive failures.
	connection.client.Store(client)
	connection.connect = func(ctx context.Context) error { return connection.client.Load().ConnectContext(ctx) }
	connection.disconnect = func() { connection.client.Load().Disconnect() }
	connection.qrChannel = func(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
		return connection.client.Load().GetQRChannel(ctx)
	}
	var registerEvents func(*whatsmeow.Client)
	connection.preparePairing = func() { connection.replaceDeletedClient(container.NewDevice, registerEvents) }
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		connection.renderQR = func(code string) { qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stdout) }
	}

	// Setup event handling for messages and history sync
	eventHandler := func(client *whatsmeow.Client, evt interface{}) {
		if connection.client.Load() != client {
			return
		}
		connection.handleEvent(evt)
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.UndecryptableMessage:
			// The first (failed) delivery carries the original send-time. WhatsApp
			// re-sends after our retry receipt, but that copy's `t` is the resend
			// time — so stash the original now and reuse it in handleMessage.
			rememberOriginalTimestamp(v.Info.ID, v.Info.Timestamp)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Receipt:
			// Persist read state so consumers can distinguish genuine unread
			// from "latest message is inbound". Only our own reads count.
			if isSelfReadReceipt(v) {
				chatJID := resolveLIDChat(client, v.Chat, v.SenderAlt, v.RecipientAlt, v.IsFromMe).String()
				// Prefer the acknowledged messages' timestamps over the
				// receipt event time so out-of-order delivery cannot advance
				// the marker past an unread message.
				readAt := v.Timestamp
				ids := make([]string, len(v.MessageIDs))
				for i, id := range v.MessageIDs {
					ids[i] = string(id)
				}
				if ts, ok, err := messageStore.MaxMessageTimestamp(chatJID, ids); err != nil {
					logger.Warnf("Failed to look up read receipt message times for %s: %v", chatJID, err)
				} else if ok {
					readAt = ts
				}
				if err := messageStore.MarkChatRead(chatJID, readAt); err != nil {
					logger.Warnf("Failed to mark chat %s read: %v", chatJID, err)
				}
			}

		case *events.GroupInfo:
			if v.Ephemeral != nil {
				expiration := uint32(0)
				if v.Ephemeral.IsEphemeral {
					expiration = v.Ephemeral.DisappearingTimer
				}
				if err := messageStore.UpdateChatEphemeralSettings(v.JID.String(), expiration, v.Timestamp.Unix()); err != nil {
					logger.Warnf("Failed to store group ephemeral settings for %s: %v", v.JID, err)
				}
			}

		case *events.CallOffer:
			// 1:1 incoming call. call_type defaults to "voice"; CallOffer
			// doesn't expose Media directly (it's buried in the binary Data
			// node). Group calls come through CallOfferNotice instead, which
			// DOES expose Media cleanly.
			handleCallOffer(client, messageStore, v.BasicCallMeta, "voice", false, logger)

		case *events.CallOfferNotice:
			// Group calls. v.Media is "audio" or "video"; normalize to our
			// "voice"/"video" convention.
			callType := "voice"
			if v.Media == "video" {
				callType = "video"
			}
			isGroup := v.Type == "group" || !v.BasicCallMeta.GroupJID.IsEmpty()
			handleCallOffer(client, messageStore, v.BasicCallMeta, callType, isGroup, logger)

		case *events.CallAccept:
			if err := messageStore.MarkCallAnswered(v.CallID, callChatJID(v.BasicCallMeta)); err != nil {
				logger.Warnf("Failed to mark call answered: %v", err)
			} else {
				logger.Infof("Call answered: id=%s", v.CallID)
			}

		case *events.CallReject:
			if err := messageStore.MarkCallRejected(v.CallID, callChatJID(v.BasicCallMeta)); err != nil {
				logger.Warnf("Failed to mark call rejected: %v", err)
			} else {
				logger.Infof("Call rejected: id=%s", v.CallID)
			}

		case *events.CallTerminate:
			if err := messageStore.MarkCallTerminated(v.CallID, callChatJID(v.BasicCallMeta), v.Reason, v.Timestamp); err != nil {
				logger.Warnf("Failed to mark call terminated: %v", err)
			} else {
				logger.Infof("Call terminated: id=%s reason=%q", v.CallID, v.Reason)
			}

		case *events.Connected:
			logger.Infof("✓ Successfully connected to WhatsApp servers")

		case *events.LoggedOut:
			logger.Warnf("⚠️  Device logged out, please scan QR code to log in again")

		case *events.ClientOutdated:
			logger.Errorf("❌ Client outdated - please update whatsmeow library")
		}
	}
	registerEvents = func(client *whatsmeow.Client) {
		client.AddEventHandler(func(evt interface{}) { eventHandler(client, evt) })
	}
	registerEvents(client)

	// Resolve the allow-listed roots that media_path values in /api/send must
	// live under. See media_path.go for the rationale.
	allowedMediaRoots, mrErr := resolveMediaRoots()
	if mrErr != nil {
		logger.Errorf("Failed to resolve media roots: %v", mrErr)
		return
	}
	logger.Infof("Allowed media roots: %v", allowedMediaRoots)

	startRESTServer(client, messageStore, port, bridgeToken, allowedMediaRoots, connection)
	connection.start()
	defer connection.close()

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// The deferred controller close cancels attempts and disconnects the client.
}
