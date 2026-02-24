package parser

import "regexp"

// Pre-compiled regex patterns for parsing Postfix logs.
// These are compiled once at package initialization for maximum performance.
var (
	// Timestamp pattern: "Dec 21 10:15:30" or "Dec  1 09:05:01" (note: single-digit day has extra space)
	timestampRegex = regexp.MustCompile(`^(\w{3}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2})`)

	// Queue ID pattern: alphanumeric ID followed by colon (e.g., "QUEUE123:", "ABC123DEF:")
	queueIDRegex = regexp.MustCompile(`\s([A-F0-9]+):`)

	// Client connection info: client=hostname[ip_address]
	// Captures hostname and IP separately
	clientRegex = regexp.MustCompile(`client=([^\[]+)\[([^\]]+)\]`)

	// SASL username: sasl_username=username
	saslRegex = regexp.MustCompile(`sasl_username=([^\s,]+)`)

	// Message-ID: message-id=<id@domain.com>
	messageIDRegex = regexp.MustCompile(`message-id=<([^>]+)>`)

	// X-Mailgateway-Queue-ID header value: captures the MGW queue ID
	// Matches: "header X-Mailgateway-Queue-ID: MGW-20241221-101532-ABC123"
	mailgatewayQueueIDRegex = regexp.MustCompile(`header X-Mailgateway-Queue-ID:\s+([^\s]+)`)

	// From address: from=<sender@example.com> or from=<>
	fromRegex = regexp.MustCompile(`from=<([^>]*)>`)

	// Message size in bytes: size=1234
	sizeRegex = regexp.MustCompile(`size=(\d+)`)

	// Number of recipients: nrcpt=1
	nrcptRegex = regexp.MustCompile(`nrcpt=(\d+)`)

	// Recipient address: to=<user@domain.com>
	recipientRegex = regexp.MustCompile(`to=<([^>]+)>`)

	// Relay information: relay=hostname[ip]:port or relay=none
	// Captures: hostname, IP (optional), port (optional)
	relayRegex = regexp.MustCompile(`relay=([^\s,\[]+)(?:\[([^\]]+)\])?(?::(\d+))?`)

	// Delay values: delay=2.1, delays=0.1/0.01/1/1
	delayRegex  = regexp.MustCompile(`delay=([\d.]+)`)
	delaysRegex = regexp.MustCompile(`delays=([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)`)

	// DSN (Delivery Status Notification) code: dsn=2.0.0, dsn=4.7.1, dsn=5.1.1
	dsnRegex = regexp.MustCompile(`dsn=([0-9.]+)`)

	// Status: status=sent, status=deferred, status=bounced
	statusRegex = regexp.MustCompile(`status=(\w+)`)

	// Status message in parentheses: (250 2.0.0 OK), (connect timeout), (User unknown)
	// Note: Uses greedy match to handle nested parentheses in error messages
	// Example: (host mx.yandex.ru[...] said: 550 5.7.1 ... (in reply to end of DATA command))
	statusMessageRegex = regexp.MustCompile(`status=\w+\s+\((.+)\)$`)

	// Removed from queue action
	removedRegex = regexp.MustCompile(`:\s+removed\s*$`)

	// Process name extraction: postfix/smtpd, postfix/cleanup, postfix/smtp, postfix/qmgr
	processRegex = regexp.MustCompile(`postfix/(\w+)\[`)

	// SASL method: sasl_method=PLAIN, sasl_method=LOGIN
	saslMethodRegex = regexp.MustCompile(`sasl_method=([^\s,]+)`)
)

// Log process types (postfix daemon names)
const (
	ProcessSMTPD   = "smtpd"   // Incoming SMTP server
	ProcessCleanup = "cleanup" // Message cleanup and header processing
	ProcessSMTP    = "smtp"    // Outgoing SMTP client
	ProcessQMGR    = "qmgr"    // Queue manager
	ProcessLocal   = "local"   // Local delivery
	ProcessVirtual = "virtual" // Virtual delivery
	ProcessBounce  = "bounce"  // Bounce message handler
	ProcessPickup  = "pickup"  // Pickup from maildrop
	ProcessError   = "error"   // Error handler
)

// Delivery status values
const (
	StatusSent     = "sent"
	StatusDeferred = "deferred"
	StatusBounced  = "bounced"
	StatusExpired  = "expired"
)

// Log action types (what happened to the message)
const (
	ActionReceived    = "received"     // Message received from client
	ActionQueued      = "queued"       // Message added to queue
	ActionDelivery    = "delivery"     // Delivery attempt (sent/deferred/bounced)
	ActionRemoved     = "removed"      // Message removed from queue
	ActionMessageInfo = "message_info" // Message-ID logged
	ActionHeaderInfo  = "header_info"  // Header information logged
	ActionFromInfo    = "from_info"    // Sender information logged
)

// DSN class prefixes for categorization
const (
	DSNSuccess  = "2" // 2.x.x - Success
	DSNTempFail = "4" // 4.x.x - Temporary failure
	DSNPermFail = "5" // 5.x.x - Permanent failure
)

// Common header names to track
const (
	HeaderMessageID        = "message-id"
	HeaderMailgatewayQueue = "X-Mailgateway-Queue-ID"
	HeaderFrom             = "From"
	HeaderTo               = "To"
	HeaderSubject          = "Subject"
)

// SASL authentication methods
const (
	SASLMethodPLAIN = "PLAIN"
	SASLMethodLOGIN = "LOGIN"
)

// GetDSNClass returns the DSN class (2, 4, or 5) from a DSN code
// Returns empty string if invalid format
func GetDSNClass(dsn string) string {
	if len(dsn) == 0 {
		return ""
	}
	class := string(dsn[0])
	if class == DSNSuccess || class == DSNTempFail || class == DSNPermFail {
		return class
	}
	return ""
}

// IsTemporaryFailure checks if a DSN code indicates a temporary failure (4.x.x)
func IsTemporaryFailure(dsn string) bool {
	return GetDSNClass(dsn) == DSNTempFail
}

// IsPermanentFailure checks if a DSN code indicates a permanent failure (5.x.x)
func IsPermanentFailure(dsn string) bool {
	return GetDSNClass(dsn) == DSNPermFail
}

// IsSuccess checks if a DSN code indicates success (2.x.x)
func IsSuccess(dsn string) bool {
	return GetDSNClass(dsn) == DSNSuccess
}
