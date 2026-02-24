package parser

import (
	"strings"
	"time"

	"relay-agent/internal/repository"
	"relay-agent/internal/util"
)

// Parser handles Postfix log parsing with zero-allocation design.
// It tracks in-progress emails and outputs completed Email records.
type Parser struct {
	// Track in-progress emails by queue_id (sharded for reduced lock contention)
	pending *ShardedPendingMap

	// Output channel for completed emails
	outputChan chan *repository.Email

	// Provider detection (gmail.com -> Gmail, etc.)
	providerMap map[string]string

	// Current year for timestamp parsing (Postfix logs don't include year)
	currentYear int

	// Cleanup configuration
	maxPendingAge time.Duration
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
}

// NewParser creates a new Postfix log parser.
// outputChan receives completed Email records ready for storage.
func NewParser(outputChan chan *repository.Email) *Parser {
	p := &Parser{
		pending:       NewShardedPendingMap(), // Sharded map for reduced lock contention
		outputChan:    outputChan,
		currentYear:   time.Now().Year(),
		maxPendingAge: 30 * time.Minute, // Default: clean up entries older than 30 minutes
		stopCleanup:   make(chan struct{}),
	}

	// Initialize provider map for fast domain-to-provider lookup
	p.providerMap = map[string]string{
		"gmail.com":      "Gmail",
		"googlemail.com": "Gmail",
		"outlook.com":    "Outlook",
		"hotmail.com":    "Outlook",
		"live.com":       "Outlook",
		"yahoo.com":      "Yahoo",
		"ymail.com":      "Yahoo",
		"icloud.com":     "Apple",
		"me.com":         "Apple",
		"mac.com":        "Apple",
		"aol.com":        "AOL",
	}

	return p
}

// ParseLine parses a single Postfix log line and updates email state.
// Returns error if the line cannot be parsed.
// This is the main entry point for log processing.
// Zero-allocation hot path: no regex, no string splits, pure index-based parsing.
func (p *Parser) ParseLine(line string) error {
	if len(line) == 0 {
		return nil // Skip empty lines
	}

	// Extract process type without regex - find "postfix/" then read until "["
	// Format: "... postfix/smtp[12345]: ABC123: ..."
	processType := extractProcessType(line)
	if processType == "" {
		return nil // Not a postfix log line we care about
	}

	// Extract queue ID - find it after the process info (after the closing bracket and ]: )
	// Format: "postfix/smtpd[12345]: ABC123: ..."
	bracketIdx := strings.Index(line, "]: ")
	if bracketIdx < 0 {
		return nil
	}

	// Start after "]: "
	queueStart := bracketIdx + 3
	if queueStart >= len(line) {
		return nil
	}

	// Find the next colon (marks end of queue ID)
	colonIdx := strings.IndexByte(line[queueStart:], ':')
	if colonIdx < 0 {
		return nil
	}

	queueID := line[queueStart : queueStart+colonIdx]
	// Validate queue ID (should be alphanumeric, typically uppercase hex)
	if len(queueID) == 0 {
		return nil
	}

	// Get or create LogEntry for this queue ID (using sharded map)
	entry, _ := p.pending.GetOrCreate(queueID, func() *repository.LogEntry {
		e := repository.GetLogEntry()
		e.QueueID = queueID
		return e
	})

	// Parse timestamp (format: "Dec 21 10:15:30" or "Dec  1 09:05:01")
	// Use fast parsing - avoid time.Parse allocation
	if entry.Timestamp.IsZero() {
		ts := p.parseTimestamp(line)
		if !ts.IsZero() {
			entry.Timestamp = ts
		}
	}

	// Dispatch to appropriate handler based on process type
	switch processType {
	case ProcessSMTPD:
		p.processSmtpd(entry, line)
	case ProcessCleanup:
		p.processCleanup(entry, line)
	case ProcessQMGR:
		p.processQmgr(entry, line)
	case ProcessSMTP, ProcessLocal, ProcessVirtual:
		p.processSmtp(entry, line)
	}

	return nil
}

// processSmtpd handles incoming SMTP connection logs.
// Extracts: client hostname, IP address, SASL username
// Example: "client=mail.example.com[192.0.2.1], sasl_username=user@domain.com"
// Zero-allocation: uses string index operations instead of regex.
func (p *Parser) processSmtpd(entry *repository.LogEntry, line string) {
	entry.LogType = ProcessSMTPD

	// Extract client information: "client=hostname[ip]"
	if clientIdx := strings.Index(line, "client="); clientIdx >= 0 {
		rest := line[clientIdx+7:] // skip "client="
		bracketStart := strings.IndexByte(rest, '[')
		bracketEnd := strings.IndexByte(rest, ']')
		if bracketStart > 0 && bracketEnd > bracketStart {
			clientHost := rest[:bracketStart]
			clientIP := rest[bracketStart+1 : bracketEnd]

			// Skip reinjection smtpd records (localhost = internal filter reinjection)
			if clientIP == "127.0.0.1" || clientHost == "localhost" {
				return
			}
		}
	}

	// Extract SASL username: "sasl_username=value"
	if saslUser := extractBetween(line, "sasl_username=", ','); saslUser != "" {
		entry.Sender = saslUser
		entry.ReceivedAt = entry.Timestamp
		entry.Action = ActionReceived
	} else if saslUser := extractBetween(line, "sasl_username=", ' '); saslUser != "" {
		entry.Sender = saslUser
		entry.ReceivedAt = entry.Timestamp
		entry.Action = ActionReceived
	}
}

// processCleanup handles message cleanup logs.
// Extracts: message-id, X-Mailgateway-Queue-ID header
// Example: "message-id=<abc@example.com>"
// Example: "header X-Mailgateway-Queue-ID: MGW-20241221-101532-ABC123"
// Zero-allocation: uses string index operations instead of regex.
func (p *Parser) processCleanup(entry *repository.LogEntry, line string) {
	entry.LogType = ProcessCleanup

	// Extract message-id: "message-id=<...>"
	if strings.Contains(line, "message-id=") {
		entry.Action = ActionMessageInfo
	}

	// Extract X-Mailgateway-Queue-ID header (critical for tracking)
	const mgwPrefix = "X-Mailgateway-Queue-ID: "
	if idx := strings.Index(line, mgwPrefix); idx >= 0 {
		rest := line[idx+len(mgwPrefix):]
		// Queue ID ends at space or end of line
		endIdx := strings.IndexByte(rest, ' ')
		if endIdx > 0 {
			entry.MailgatewayQueueID = rest[:endIdx]
		} else if len(rest) > 0 {
			entry.MailgatewayQueueID = rest
		}
		entry.Action = ActionHeaderInfo
	}
}

// processQmgr handles queue manager logs.
// Extracts: from address, message size, nrcpt (number of recipients)
// Also detects "removed" action (email processing complete)
// Example: "from=<sender@example.com>, size=1234, nrcpt=1"
// Example: "removed"
// Zero-allocation: uses string index operations instead of regex.
func (p *Parser) processQmgr(entry *repository.LogEntry, line string) {
	entry.LogType = ProcessQMGR

	// Check if message was removed from queue (completion signal)
	// Look for ": removed" at end of line (faster than regex)
	if strings.HasSuffix(line, "removed") {
		entry.Action = ActionRemoved
		p.finalize(entry)
		return
	}

	// Extract sender address: "from=<sender@example.com>"
	if sender, ok := extractAngleBracket(line, "from="); ok {
		if sender != "" { // from=<> means null sender (bounce)
			entry.Sender = sender
		}
		entry.Action = ActionFromInfo
	}

	// Extract message size: "size=1234"
	if sizeStr := extractBetween(line, "size=", ','); sizeStr != "" {
		entry.Size = parseIntFast(sizeStr)
	}
}

// processSmtp handles outgoing SMTP delivery logs.
// Extracts: recipient, relay info, delay, DSN, status, status message
// This is where we know if email was sent/deferred/bounced
// Example: "to=<user@gmail.com>, relay=gmail-smtp-in.l.google.com[142.250.153.27]:25, delay=2.1, delays=0.01/0.01/1.5/0.58, dsn=2.0.0, status=sent (250 2.0.0 OK)"
// Zero-allocation: uses string index operations instead of regex.
func (p *Parser) processSmtp(entry *repository.LogEntry, line string) {
	entry.LogType = ProcessSMTP
	entry.Action = ActionDelivery

	// Extract relay information: "relay=hostname[ip]:port"
	if relayIdx := strings.Index(line, "relay="); relayIdx >= 0 {
		rest := line[relayIdx+6:] // skip "relay="
		// Find end of relay field (comma or space)
		endIdx := strings.IndexAny(rest, ", ")
		if endIdx < 0 {
			endIdx = len(rest)
		}
		relayField := rest[:endIdx]

		// Parse hostname[ip]:port
		bracketStart := strings.IndexByte(relayField, '[')
		if bracketStart > 0 {
			relayHost := relayField[:bracketStart]
			bracketEnd := strings.IndexByte(relayField[bracketStart:], ']')
			relayIP := ""
			if bracketEnd > 0 {
				relayIP = relayField[bracketStart+1 : bracketStart+bracketEnd]
			}

			// Skip internal filter hops
			if relayIP == "127.0.0.1" || relayHost == "127.0.0.1" {
				return
			}
			entry.RelayHost = relayHost
			entry.RelayIP = relayIP
		} else if relayField != "none" {
			if relayField == "127.0.0.1" {
				return
			}
			entry.RelayHost = relayField
		}
	}

	// Extract recipient: "to=<user@gmail.com>"
	if recipient, ok := extractAngleBracket(line, "to="); ok && recipient != "" {
		entry.Recipient = recipient
		entry.RecipientDomain = p.extractDomain(recipient)
		entry.Provider = p.detectProvider(entry.RecipientDomain)
	}

	// Extract delay: "delay=2.1"
	if delayStr := extractBetween(line, "delay=", ','); delayStr != "" {
		entry.DeliveryTimeMs = p.parseDelay(delayStr)
	}

	// Extract DSN: "dsn=2.0.0"
	if dsn := extractBetween(line, "dsn=", ','); dsn != "" {
		entry.DSN = dsn
	}

	// Extract status: "status=sent" / "status=deferred" / "status=bounced"
	if statusIdx := strings.Index(line, "status="); statusIdx >= 0 {
		rest := line[statusIdx+7:] // skip "status="
		// Status word ends at space or end of line
		endIdx := strings.IndexByte(rest, ' ')
		if endIdx > 0 {
			entry.Status = rest[:endIdx]
		} else if len(rest) > 0 {
			entry.Status = rest
		}
		entry.DeliveredAt = entry.Timestamp
		entry.Attempts++

		// Extract status message: text in final parentheses after "status=word "
		if endIdx > 0 && endIdx+1 < len(rest) {
			msgPart := rest[endIdx+1:]
			if len(msgPart) > 1 && msgPart[0] == '(' {
				// Find the matching closing paren (handle nested parens)
				if lastParen := strings.LastIndexByte(msgPart, ')'); lastParen > 0 {
					entry.StatusMessage = msgPart[1:lastParen]
				}
			}
		}
	}

	// If this is a final status (sent or bounced), finalize
	// Deferred means will retry, so don't finalize yet
	if entry.Status == StatusSent || entry.Status == StatusBounced {
		p.finalize(entry)
	}
}

// finalize converts a LogEntry to an Email and sends it to the output channel.
// Removes the entry from pending map and returns LogEntry to pool.
// This is called when an email reaches final state (sent/bounced/removed).
func (p *Parser) finalize(entry *repository.LogEntry) {
	// Remove from pending map (using sharded map)
	p.pending.Delete(entry.QueueID)

	// Only create Email if we have meaningful data
	if entry.Recipient == "" || entry.Status == "" {
		repository.PutLogEntry(entry)
		return
	}

	// Get Email from pool (zero allocation)
	email := repository.GetEmail()

	// Copy data from LogEntry to Email
	email.QueueID = entry.QueueID
	email.MailgatewayQueueID = entry.MailgatewayQueueID
	email.Sender = entry.Sender
	email.Recipient = entry.Recipient
	email.RecipientDomain = entry.RecipientDomain
	email.Provider = entry.Provider
	email.Size = entry.Size
	email.Status = entry.Status
	email.DSN = entry.DSN
	email.StatusMessage = entry.StatusMessage
	email.RelayHost = entry.RelayHost
	email.RelayIP = entry.RelayIP
	email.ReceivedAt = entry.ReceivedAt
	email.DeliveredAt = entry.DeliveredAt
	email.DeliveryTimeMs = entry.DeliveryTimeMs
	email.Attempts = entry.Attempts
	email.CreatedAt = util.NowTurkey()
	email.WebhookSent = false

	// Send to output channel (non-blocking - caller must handle full channel)
	select {
	case p.outputChan <- email:
		// Sent successfully
	default:
		// Channel full - put email back to pool to avoid leak
		repository.PutEmail(email)
	}

	// Return LogEntry to pool
	repository.PutLogEntry(entry)
}

// detectProvider maps an email domain to a known provider name.
// Uses pre-populated provider map for fast lookup.
// Falls back to capitalizing the domain name for unknown providers.
// Zero-allocation for known providers (99%+ of cases).
func (p *Parser) detectProvider(domain string) string {
	if domain == "" {
		return ""
	}

	// Fast path: known provider (zero-alloc)
	if provider, ok := p.providerMap[domain]; ok {
		return provider
	}

	// Slow path: capitalize domain name (e.g., "company.com" -> "Company")
	dotIdx := strings.IndexByte(domain, '.')
	if dotIdx <= 0 {
		return domain
	}

	// Direct []byte manipulation - single allocation for result string only
	domainPart := domain[:dotIdx]
	if len(domainPart) == 0 {
		return domain
	}

	// Allocate exact-size byte slice
	result := make([]byte, len(domainPart))
	copy(result, domainPart)
	if result[0] >= 'a' && result[0] <= 'z' {
		result[0] -= 32
	}
	return string(result)
}

// extractDomain extracts the domain from an email address.
// Example: "user@gmail.com" -> "gmail.com"
// Zero-allocation implementation using string slicing.
func (p *Parser) extractDomain(email string) string {
	atIdx := strings.IndexByte(email, '@')
	// Check: @ exists, has content before it, has content after it
	if atIdx <= 0 || atIdx == len(email)-1 {
		return ""
	}
	return email[atIdx+1:]
}

// parseTimestamp parses Postfix timestamp format efficiently.
// Format: "Dec 21 10:15:30" or "Dec  1 09:05:01" (single-digit day has space padding)
// Postfix logs don't include year, so we use current year.
// This is a fast path to avoid time.Parse allocation.
func (p *Parser) parseTimestamp(line string) time.Time {
	// Expected format at start of line: "Dec 21 10:15:30"
	if len(line) < 15 {
		return time.Time{}
	}

	// Fast month parsing (first 3 chars)
	var month time.Month
	switch line[0:3] {
	case "Jan":
		month = time.January
	case "Feb":
		month = time.February
	case "Mar":
		month = time.March
	case "Apr":
		month = time.April
	case "May":
		month = time.May
	case "Jun":
		month = time.June
	case "Jul":
		month = time.July
	case "Aug":
		month = time.August
	case "Sep":
		month = time.September
	case "Oct":
		month = time.October
	case "Nov":
		month = time.November
	case "Dec":
		month = time.December
	default:
		return time.Time{}
	}

	// Parse day (positions 4-5, may have leading space)
	dayStart := 4
	if line[4] == ' ' {
		dayStart = 5 // Skip leading space for single-digit days
	}
	day := 0
	for i := dayStart; i < 6 && i < len(line); i++ {
		if line[i] >= '0' && line[i] <= '9' {
			day = day*10 + int(line[i]-'0')
		}
	}

	// Parse time "HH:MM:SS" starting at position 7
	if len(line) < 15 {
		return time.Time{}
	}
	timeStr := line[7:15] // "10:15:30"

	hour := int(timeStr[0]-'0')*10 + int(timeStr[1]-'0')
	minute := int(timeStr[3]-'0')*10 + int(timeStr[4]-'0')
	second := int(timeStr[6]-'0')*10 + int(timeStr[7]-'0')

	// Use UTC location so MongoDB stores the local time value as-is
	// (Postfix logs are already in Turkey time, we don't want MongoDB to convert)
	return time.Date(p.currentYear, month, day, hour, minute, second, 0, time.UTC)
}

// parseDelay converts delay string (e.g., "2.1") to milliseconds.
// Zero-allocation integer parsing.
func (p *Parser) parseDelay(delayStr string) int64 {
	if len(delayStr) == 0 {
		return 0
	}

	// Find decimal point
	dotIdx := strings.IndexByte(delayStr, '.')
	if dotIdx < 0 {
		// No decimal point - integer seconds
		seconds := int64(0)
		for i := 0; i < len(delayStr); i++ {
			if delayStr[i] >= '0' && delayStr[i] <= '9' {
				seconds = seconds*10 + int64(delayStr[i]-'0')
			}
		}
		return seconds * 1000
	}

	// Parse seconds (before decimal)
	seconds := int64(0)
	for i := 0; i < dotIdx; i++ {
		if delayStr[i] >= '0' && delayStr[i] <= '9' {
			seconds = seconds*10 + int64(delayStr[i]-'0')
		}
	}

	// Parse milliseconds (after decimal, up to 3 digits)
	millis := int64(0)
	multiplier := int64(100) // First digit after decimal = hundreds of ms
	for i := dotIdx + 1; i < len(delayStr) && i < dotIdx+4; i++ {
		if delayStr[i] >= '0' && delayStr[i] <= '9' {
			millis += int64(delayStr[i]-'0') * multiplier
			multiplier /= 10
		}
	}

	return seconds*1000 + millis
}

// Flush processes all pending emails and sends them to output channel.
// Call this when shutting down or at periodic intervals to handle incomplete emails.
// Returns the number of emails flushed.
func (p *Parser) Flush() int {
	// Get all entries and clear the map atomically (using sharded map)
	entries := p.pending.GetAllAndClear()

	// Now process them without holding any locks
	count := 0
	for _, entry := range entries {
		// Only flush entries that have meaningful data
		if entry.Recipient != "" || entry.Sender != "" {
			// Create Email directly here to avoid finalize's lock issue
			email := repository.GetEmail()
			email.QueueID = entry.QueueID
			email.MailgatewayQueueID = entry.MailgatewayQueueID
			email.Sender = entry.Sender
			email.Recipient = entry.Recipient
			email.RecipientDomain = entry.RecipientDomain
			email.Provider = entry.Provider
			email.Size = entry.Size
			email.Status = entry.Status
			email.DSN = entry.DSN
			email.StatusMessage = entry.StatusMessage
			email.RelayHost = entry.RelayHost
			email.RelayIP = entry.RelayIP
			email.ReceivedAt = entry.ReceivedAt
			email.DeliveredAt = entry.DeliveredAt
			email.DeliveryTimeMs = entry.DeliveryTimeMs
			email.Attempts = entry.Attempts
			email.CreatedAt = util.NowTurkey()
			email.WebhookSent = false

			// Send to output channel (non-blocking)
			select {
			case p.outputChan <- email:
				count++
			default:
				// Channel full - put email back to pool
				repository.PutEmail(email)
			}
		}

		// Return LogEntry to pool
		repository.PutLogEntry(entry)
	}

	return count
}

// PendingCount returns the number of emails currently being tracked.
// Useful for monitoring and debugging.
func (p *Parser) PendingCount() int {
	return p.pending.Count()
}

// StartCleanup starts a background goroutine that periodically cleans up
// stale entries from the pending map. This prevents memory leaks from
// emails that never reach a final state.
func (p *Parser) StartCleanup(interval time.Duration) {
	p.cleanupTicker = time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-p.stopCleanup:
				p.cleanupTicker.Stop()
				return
			case <-p.cleanupTicker.C:
				p.cleanupStaleEntries()
			}
		}
	}()
}

// StopCleanup stops the background cleanup goroutine.
func (p *Parser) StopCleanup() {
	if p.stopCleanup != nil {
		close(p.stopCleanup)
	}
}

// extractProcessType extracts the postfix process type from a log line
// without regex. Returns "" if not a postfix log line.
// Zero-allocation: returns a substring slice of the input.
// Example: "Dec 21 10:15:30 host postfix/smtp[12345]: ..." -> "smtp"
func extractProcessType(line string) string {
	// Find "postfix/" marker
	const marker = "postfix/"
	idx := strings.Index(line, marker)
	if idx < 0 {
		return ""
	}

	// Start after "postfix/"
	start := idx + len(marker)
	if start >= len(line) {
		return ""
	}

	// Find "[" that ends the process name
	end := strings.IndexByte(line[start:], '[')
	if end < 0 || end == 0 {
		return ""
	}

	return line[start : start+end]
}

// extractBetween extracts substring between prefix marker and suffix byte.
// Zero-allocation: returns a substring slice.
// Returns "" if not found.
func extractBetween(line, prefix string, suffix byte) string {
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return ""
	}
	start := idx + len(prefix)
	if start >= len(line) {
		return ""
	}
	end := strings.IndexByte(line[start:], suffix)
	if end < 0 {
		return line[start:]
	}
	return line[start : start+end]
}

// extractAngleBracket extracts content between < and > after a prefix.
// Zero-allocation: returns a substring slice.
// Example: extractAngleBracket("from=<user@test.com>", "from=") -> "user@test.com"
func extractAngleBracket(line, prefix string) (string, bool) {
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return "", false
	}
	start := idx + len(prefix)
	if start >= len(line) || line[start] != '<' {
		return "", false
	}
	start++ // skip '<'
	end := strings.IndexByte(line[start:], '>')
	if end < 0 {
		return "", false
	}
	return line[start : start+end], true
}

// parseIntFast parses a positive integer from a string without allocation.
// Returns 0 for empty or non-numeric strings.
func parseIntFast(s string) int64 {
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int64(s[i]-'0')
		} else {
			break
		}
	}
	return n
}

// cleanupStaleEntries removes entries older than maxPendingAge from pending map.
// This is called periodically by the cleanup goroutine.
func (p *Parser) cleanupStaleEntries() {
	cutoff := time.Now().Add(-p.maxPendingAge)

	// Use sharded map's CollectStale method for efficient cleanup
	staleEntries := p.pending.CollectStale(func(entry *repository.LogEntry) bool {
		// If timestamp is zero, use a heuristic (consider it old)
		return entry.Timestamp.IsZero() || entry.Timestamp.Before(cutoff)
	})

	// Process stale entries outside any locks
	cleanedCount := 0
	for _, entry := range staleEntries {
		// Only create Email if we have meaningful data
		if entry.Recipient != "" && entry.Status != "" {
			email := repository.GetEmail()
			email.QueueID = entry.QueueID
			email.MailgatewayQueueID = entry.MailgatewayQueueID
			email.Sender = entry.Sender
			email.Recipient = entry.Recipient
			email.RecipientDomain = entry.RecipientDomain
			email.Provider = entry.Provider
			email.Size = entry.Size
			email.Status = entry.Status
			email.DSN = entry.DSN
			email.StatusMessage = entry.StatusMessage
			email.RelayHost = entry.RelayHost
			email.RelayIP = entry.RelayIP
			email.ReceivedAt = entry.ReceivedAt
			email.DeliveredAt = entry.DeliveredAt
			email.DeliveryTimeMs = entry.DeliveryTimeMs
			email.Attempts = entry.Attempts
			email.CreatedAt = util.NowTurkey()
			email.WebhookSent = false

			select {
			case p.outputChan <- email:
				cleanedCount++
			default:
				repository.PutEmail(email)
			}
		}

		// Return LogEntry to pool
		repository.PutLogEntry(entry)
	}

	// Log if we cleaned anything (but only if significant)
	_ = cleanedCount // Could log this if needed
}
