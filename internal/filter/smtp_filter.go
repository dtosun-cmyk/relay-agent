package filter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"relay-agent/internal/repository"
	"relay-agent/internal/util"
)

const (
	// SMTP response codes
	smtpReady     = 220
	smtpOK        = 250
	smtpStartData = 354
	smtpClosing   = 221
	smtpTempError = 451
	smtpPermError = 554

	// SMTP protocol limits
	maxLineLength  = 1000
	maxHeaderSize  = 64 * 1024        // 64KB for headers
	maxMessageSize = 50 * 1024 * 1024 // 50MB max message

	// Timeouts
	defaultTimeout = 5 * time.Minute
	dataTimeout    = 10 * time.Minute

	// Buffer sizes
	readBufferSize  = 4096
	writeBufferSize = 4096
)

var (
	// Error definitions
	ErrMessageTooLarge = errors.New("message too large")
	ErrInvalidCommand  = errors.New("invalid command")
	ErrNoRecipients    = errors.New("no recipients")
	ErrForwardFailed   = errors.New("forward failed")

	// Reusable byte slices
	crlf       = []byte("\r\n")
	colonSpace = []byte(": ")
	dotCrlf    = []byte(".\r\n")
)

// SMTPFilter implements a Postfix content_filter SMTP server.
// It receives emails from Postfix, logs them to MongoDB, and forwards them
// to the next hop for delivery.
type SMTPFilter struct {
	// Configuration
	listenAddr string
	nextHop    string
	hostname   string
	tlsConfig  *tls.Config

	// Communication channels
	emailChan chan *repository.Email

	// Logger
	logger zerolog.Logger

	// Server state
	listener net.Listener
	wg       sync.WaitGroup
	shutdown chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc

	// Connection pool for next hop
	connPool *SMTPConnectionPool

	// Metrics - atomic counters for lock-free updates
	received    atomic.Int64
	forwarded   atomic.Int64
	errors      atomic.Int64
	activeConns atomic.Int64
}

// EmailMessage represents a parsed email message in transit.
// Optimized for memory alignment and efficient parsing.
type EmailMessage struct {
	// 8-byte aligned fields
	ReceivedAt time.Time
	Size       int64

	// String fields
	From        string
	Subject     string
	MessageID   string
	QueueID     string // X-Mailgateway-Queue-ID
	Date        string
	ContentType string

	// Slices
	Recipients []string
	Data       []byte
	Headers    map[string]string

	// Original client info (from XFORWARD)
	ClientAddr string
	ClientName string
	ClientHelo string
}

// NewSMTPFilter creates a new SMTP content filter instance.
// The emailChan is used to send parsed emails to MongoDB batch processor.
func NewSMTPFilter(listenAddr, nextHop string, emailChan chan *repository.Email, logger zerolog.Logger) *SMTPFilter {
	ctx, cancel := context.WithCancel(context.Background())

	return &SMTPFilter{
		listenAddr: listenAddr,
		nextHop:    nextHop,
		hostname:   "localhost",
		emailChan:  emailChan,
		logger:     logger.With().Str("component", "smtp_filter").Logger(),
		shutdown:   make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		// Connection pool: 10 connections, 2 minute idle timeout
		connPool: NewSMTPConnectionPool(nextHop, 10, 2*time.Minute),
	}
}

// SetTLS configures TLS for the SMTP filter.
// This is optional and typically not needed for localhost content filters.
func (f *SMTPFilter) SetTLS(config *tls.Config) {
	f.tlsConfig = config
}

// SetHostname sets the hostname used in SMTP greetings.
func (f *SMTPFilter) SetHostname(hostname string) {
	if hostname != "" {
		f.hostname = hostname
	}
}

// Start begins listening for SMTP connections.
// It runs until the context is canceled or Stop is called.
func (f *SMTPFilter) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", f.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", f.listenAddr, err)
	}

	f.listener = listener
	f.logger.Info().
		Str("addr", f.listenAddr).
		Str("next_hop", f.nextHop).
		Msg("SMTP filter started")

	// Accept connections in a goroutine
	f.wg.Add(1)
	go f.acceptLoop(ctx)

	return nil
}

// Stop gracefully shuts down the SMTP filter.
// It waits for all active connections to complete.
func (f *SMTPFilter) Stop() error {
	f.logger.Info().Msg("stopping SMTP filter")

	// Signal shutdown
	close(f.shutdown)
	f.cancel()

	// Close listener to stop accepting new connections
	if f.listener != nil {
		f.listener.Close()
	}

	// Wait for all connections to finish (with timeout)
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		f.logger.Info().Msg("SMTP filter stopped gracefully")
	case <-time.After(30 * time.Second):
		f.logger.Warn().Msg("SMTP filter stop timeout, some connections may be terminated")
	}

	// Close connection pool
	if f.connPool != nil {
		f.connPool.Close()
		reused, newConns, errs, _ := f.connPool.Stats()
		f.logger.Info().
			Int64("reused", reused).
			Int64("new_connections", newConns).
			Int64("errors", errs).
			Msg("connection pool closed")
	}

	return nil
}

// acceptLoop accepts incoming connections until context is canceled.
func (f *SMTPFilter) acceptLoop(ctx context.Context) {
	defer f.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-f.shutdown:
			return
		default:
		}

		// Set accept deadline to allow periodic context checks
		if tcpListener, ok := f.listener.(*net.TCPListener); ok {
			tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := f.listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Accept timeout is expected, continue loop
				continue
			}

			select {
			case <-ctx.Done():
				return
			case <-f.shutdown:
				return
			default:
				f.logger.Error().Err(err).Msg("failed to accept connection")
				continue
			}
		}

		// Handle connection in goroutine
		f.activeConns.Add(1)
		f.wg.Add(1)
		go f.handleConnection(conn)
	}
}

// handleConnection processes a single SMTP session.
func (f *SMTPFilter) handleConnection(conn net.Conn) {
	defer f.wg.Done()
	defer f.activeConns.Add(-1)
	defer conn.Close()

	// Set initial deadline
	conn.SetDeadline(time.Now().Add(defaultTimeout))

	remoteAddr := conn.RemoteAddr().String()
	f.logger.Debug().Str("remote", remoteAddr).Msg("new connection")

	// Create session
	session := &smtpSession{
		filter: f,
		conn:   conn,
		reader: bufio.NewReaderSize(conn, readBufferSize),
		writer: bufio.NewWriterSize(conn, writeBufferSize),
		logger: f.logger.With().Str("remote", remoteAddr).Logger(),
		msg:    &EmailMessage{Headers: make(map[string]string)},
	}

	// Run SMTP protocol
	if err := session.run(); err != nil {
		f.errors.Add(1)
		session.logger.Error().Err(err).Msg("session error")
	}
}

// smtpSession represents a single SMTP session.
type smtpSession struct {
	filter *SMTPFilter
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	logger zerolog.Logger
	msg    *EmailMessage

	// Session state
	heloReceived bool
	mailFrom     string
	rcptTo       []string
}

// run executes the SMTP protocol for this session.
func (s *smtpSession) run() error {
	// Send greeting
	if err := s.writeLine(smtpReady, "%s ESMTP Relay Agent", s.filter.hostname); err != nil {
		return err
	}

	// Command loop
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		// Parse command
		cmd, arg := s.parseCommand(line)

		// Handle command
		switch cmd {
		case "HELO", "EHLO":
			if err := s.handleHelo(cmd, arg); err != nil {
				return err
			}
		case "MAIL":
			if err := s.handleMail(arg); err != nil {
				return err
			}
		case "RCPT":
			if err := s.handleRcpt(arg); err != nil {
				return err
			}
		case "DATA":
			if err := s.handleData(); err != nil {
				return err
			}
			// Reset for next message
			s.mailFrom = ""
			s.rcptTo = nil
			s.msg = &EmailMessage{Headers: make(map[string]string)}
		case "RSET":
			s.mailFrom = ""
			s.rcptTo = nil
			s.msg = &EmailMessage{Headers: make(map[string]string)}
			if err := s.writeLine(smtpOK, "OK"); err != nil {
				return err
			}
		case "NOOP":
			if err := s.writeLine(smtpOK, "OK"); err != nil {
				return err
			}
		case "QUIT":
			s.writeLine(smtpClosing, "Bye")
			return nil
		case "XFORWARD":
			if err := s.handleXForward(arg); err != nil {
				return err
			}
		default:
			if err := s.writeLine(500, "Command not recognized"); err != nil {
				return err
			}
		}
	}
}

// handleHelo handles HELO/EHLO commands.
func (s *smtpSession) handleHelo(cmd, arg string) error {
	if arg == "" {
		return s.writeLine(501, "Syntax error")
	}

	s.heloReceived = true

	if cmd == "EHLO" {
		// Send ESMTP extensions
		s.writer.WriteString(fmt.Sprintf("250-%s\r\n", s.filter.hostname))
		s.writer.WriteString("250-PIPELINING\r\n")
		s.writer.WriteString("250-SIZE 52428800\r\n")
		s.writer.WriteString("250-8BITMIME\r\n")
		s.writer.WriteString("250-XFORWARD NAME ADDR PROTO HELO\r\n")
		s.writer.WriteString("250 ENHANCEDSTATUSCODES\r\n")
		return s.writer.Flush()
	}

	return s.writeLine(smtpOK, "%s", s.filter.hostname)
}

// handleMail handles MAIL FROM command.
func (s *smtpSession) handleMail(arg string) error {
	if !s.heloReceived {
		return s.writeLine(503, "Send HELO/EHLO first")
	}

	// Parse MAIL FROM:<address>
	from := s.extractAddress(arg)
	if from == "" {
		return s.writeLine(501, "Syntax error in MAIL command")
	}

	s.mailFrom = from
	s.msg.From = from

	return s.writeLine(smtpOK, "OK")
}

// handleRcpt handles RCPT TO command.
func (s *smtpSession) handleRcpt(arg string) error {
	if s.mailFrom == "" {
		return s.writeLine(503, "Send MAIL first")
	}

	// Parse RCPT TO:<address>
	to := s.extractAddress(arg)
	if to == "" {
		return s.writeLine(501, "Syntax error in RCPT command")
	}

	s.rcptTo = append(s.rcptTo, to)
	s.msg.Recipients = s.rcptTo

	return s.writeLine(smtpOK, "OK")
}

// handleData handles DATA command and receives message.
func (s *smtpSession) handleData() error {
	if len(s.rcptTo) == 0 {
		return s.writeLine(503, "Send RCPT first")
	}

	// Send start data response
	if err := s.writeLine(smtpStartData, "Start mail input; end with <CRLF>.<CRLF>"); err != nil {
		return err
	}

	// Set data timeout
	s.conn.SetDeadline(time.Now().Add(dataTimeout))

	// Read message data
	data, err := s.readData()
	if err != nil {
		s.writeLine(smtpTempError, "Error reading message")
		return err
	}

	// Check size
	if int64(len(data)) > maxMessageSize {
		return s.writeLine(smtpPermError, "Message too large")
	}

	s.msg.Data = data
	s.msg.Size = int64(len(data))
	s.msg.ReceivedAt = util.NowTurkey()

	// Parse headers
	s.filter.parseHeaders(s.msg)

	// Process email (send to MongoDB)
	s.filter.processEmail(s.msg)

	// Forward to next hop
	if err := s.filter.forwardEmail(s.msg); err != nil {
		s.logger.Error().Err(err).Msg("failed to forward email")
		s.filter.errors.Add(1)
		return s.writeLine(smtpTempError, "Failed to forward message")
	}

	s.filter.received.Add(1)
	s.filter.forwarded.Add(1)

	return s.writeLine(smtpOK, "OK: queued")
}

// handleXForward handles XFORWARD command (Postfix extension).
// This preserves original client information through the filter.
func (s *smtpSession) handleXForward(arg string) error {
	// Parse XFORWARD attributes: NAME=value ADDR=value ...
	pairs := strings.Fields(arg)
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := parts[0]
		value := parts[1]

		// Unescape [UNAVAILABLE] and [TEMPUNAVAIL]
		if value == "[UNAVAILABLE]" || value == "[TEMPUNAVAIL]" {
			value = ""
		}

		switch key {
		case "NAME":
			s.msg.ClientName = value
		case "ADDR":
			s.msg.ClientAddr = value
		case "HELO":
			s.msg.ClientHelo = value
		}
	}

	return s.writeLine(smtpOK, "OK")
}

// readLine reads a line from the client.
func (s *smtpSession) readLine() (string, error) {
	line, err := s.reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	// Remove CRLF or LF
	line = strings.TrimRight(line, "\r\n")

	// Check length
	if len(line) > maxLineLength {
		s.writeLine(500, "Line too long")
		return "", ErrInvalidCommand
	}

	s.logger.Debug().Str("line", line).Msg("received")
	return line, nil
}

// dataBufferPool provides reusable buffers for reading email data.
// Reduces GC pressure by reusing large buffers across SMTP sessions.
var dataBufferPool = sync.Pool{
	New: func() interface{} {
		b := bytes.NewBuffer(make([]byte, 0, 32*1024)) // 32KB initial capacity
		return b
	},
}

// readData reads message data until "." line.
// Uses pooled buffer to reduce allocations.
func (s *smtpSession) readData() ([]byte, error) {
	buf := dataBufferPool.Get().(*bytes.Buffer)
	buf.Reset()

	tp := textproto.NewReader(s.reader)

	for {
		line, err := tp.ReadLine()
		if err != nil {
			dataBufferPool.Put(buf)
			return nil, err
		}

		// End of data
		if line == "." {
			break
		}

		// Un-escape leading dots (transparency)
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}

		// Append line with CRLF
		buf.WriteString(line)
		buf.Write(crlf)

		// Check size limit
		if buf.Len() > maxMessageSize {
			dataBufferPool.Put(buf)
			return nil, ErrMessageTooLarge
		}
	}

	// Copy data out so buffer can be returned to pool
	data := make([]byte, buf.Len())
	copy(data, buf.Bytes())
	dataBufferPool.Put(buf)
	return data, nil
}

// writeLine writes a formatted SMTP response.
// Optimized: uses strconv.AppendInt to avoid fmt.Sprintf allocation for code.
func (s *smtpSession) writeLine(code int, format string, args ...interface{}) error {
	// Build response with minimal allocations
	var codeBuf [3]byte
	codeBuf[0] = byte('0' + code/100)
	codeBuf[1] = byte('0' + (code/10)%10)
	codeBuf[2] = byte('0' + code%10)

	s.writer.Write(codeBuf[:])
	s.writer.WriteByte(' ')

	if len(args) == 0 {
		s.writer.WriteString(format)
	} else {
		msg := fmt.Sprintf(format, args...)
		s.writer.WriteString(msg)
	}
	s.writer.Write(crlf)

	return s.writer.Flush()
}

// parseCommand parses a SMTP command line.
// Zero-allocation: uses IndexByte and manual uppercase instead of SplitN+ToUpper.
func (s *smtpSession) parseCommand(line string) (cmd, arg string) {
	// Find first space
	spaceIdx := strings.IndexByte(line, ' ')
	if spaceIdx < 0 {
		cmd = toUpperASCII(line)
		return
	}

	cmd = toUpperASCII(line[:spaceIdx])
	arg = strings.TrimSpace(line[spaceIdx+1:])
	return
}

// toUpperASCII converts ASCII string to uppercase without allocation
// for short strings (SMTP commands are 4-8 chars).
func toUpperASCII(s string) string {
	// Fast path: check if already uppercase (common for SMTP)
	isUpper := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			isUpper = false
			break
		}
	}
	if isUpper {
		return s
	}

	// Need to convert - allocate once
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		b[i] = c
	}
	return string(b)
}

// extractAddress extracts email address from MAIL/RCPT parameter.
// Handles: FROM:<user@domain>, TO:<user@domain>, FROM:<>, etc.
// Zero-allocation: uses case-insensitive prefix check without strings.ToUpper.
func (s *smtpSession) extractAddress(param string) string {
	// Remove FROM: or TO: prefix (case-insensitive, no alloc)
	if len(param) >= 5 && (param[0] == 'F' || param[0] == 'f') {
		// Check "FROM:" case-insensitively
		if hasPrefixFold(param, "FROM:") {
			param = param[5:]
		}
	} else if len(param) >= 3 && (param[0] == 'T' || param[0] == 't') {
		if hasPrefixFold(param, "TO:") {
			param = param[3:]
		}
	}

	// Find < > brackets directly (skip TrimSpace to avoid alloc)
	start := strings.IndexByte(param, '<')
	if start == -1 {
		return ""
	}
	end := strings.IndexByte(param[start:], '>')
	if end <= 1 {
		return ""
	}

	return param[start+1 : start+end]
}

// hasPrefixFold checks if s starts with prefix (case-insensitive).
// Zero-allocation.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a, b := s[i], prefix[i]
		if a >= 'a' && a <= 'z' {
			a -= 32
		}
		if b >= 'a' && b <= 'z' {
			b -= 32
		}
		if a != b {
			return false
		}
	}
	return true
}

// parseHeaders parses email headers from message data.
// This is a zero-allocation optimized parser using byte operations.
func (f *SMTPFilter) parseHeaders(msg *EmailMessage) {
	data := msg.Data

	// Find end of headers (empty line)
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd == -1 {
		headerEnd = bytes.Index(data, []byte("\n\n"))
		if headerEnd == -1 {
			return
		}
	}

	headerSection := data[:headerEnd]

	// Parse headers line by line
	var currentKey string
	var currentValue bytes.Buffer

	lines := bytes.Split(headerSection, []byte("\n"))
	for _, line := range lines {
		// Remove CR if present
		line = bytes.TrimRight(line, "\r")

		// Check for continuation line (starts with space or tab)
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			// Continuation of previous header
			currentValue.WriteByte(' ')
			currentValue.Write(bytes.TrimSpace(line))
			continue
		}

		// Save previous header
		if currentKey != "" {
			value := currentValue.String()
			msg.Headers[currentKey] = value

			// Extract important headers
			switch currentKey {
			case "Subject":
				msg.Subject = value
			case "Message-ID", "Message-Id":
				msg.MessageID = value
			case "Date":
				msg.Date = value
			case "Content-Type":
				msg.ContentType = value
			case "X-Mailgateway-Queue-ID":
				msg.QueueID = value
			}
		}

		// Parse new header
		colonIdx := bytes.Index(line, colonSpace[:1])
		if colonIdx == -1 {
			continue
		}

		currentKey = string(line[:colonIdx])
		currentValue.Reset()
		currentValue.Write(bytes.TrimSpace(line[colonIdx+1:]))
	}

	// Save last header
	if currentKey != "" {
		value := currentValue.String()
		msg.Headers[currentKey] = value

		switch currentKey {
		case "Subject":
			msg.Subject = value
		case "Message-ID", "Message-Id":
			msg.MessageID = value
		case "Date":
			msg.Date = value
		case "Content-Type":
			msg.ContentType = value
		case "X-Mailgateway-Queue-ID":
			msg.QueueID = value
		}
	}
}

// processEmail sends email info to MongoDB via channel.
func (f *SMTPFilter) processEmail(msg *EmailMessage) {
	// Create Email records for each recipient
	for _, recipient := range msg.Recipients {
		email := repository.GetEmail()

		email.ID = primitive.NewObjectID()
		email.MailgatewayQueueID = msg.QueueID
		email.Sender = msg.From
		email.Recipient = recipient
		email.Size = msg.Size
		email.ReceivedAt = msg.ReceivedAt
		email.CreatedAt = util.NowTurkey()
		email.Status = "received"

		// Store original client info from XFORWARD (mailgateway source)
		email.ClientHost = msg.ClientName
		email.ClientIP = msg.ClientAddr

		// Extract domain from recipient
		if atIdx := strings.Index(recipient, "@"); atIdx != -1 {
			email.RecipientDomain = recipient[atIdx+1:]
		}

		// Send to channel (non-blocking to avoid stalling SMTP session)
		select {
		case f.emailChan <- email:
			f.logger.Debug().
				Str("queue_id", msg.QueueID).
				Str("from", msg.From).
				Str("to", recipient).
				Int64("size", msg.Size).
				Msg("email queued for MongoDB")
		default:
			// Channel full - drop record to prevent blocking the SMTP pipeline
			f.logger.Warn().
				Str("queue_id", msg.QueueID).
				Msg("channel full, dropping email record")
			repository.PutEmail(email)
		}
	}
}

// forwardEmail forwards the email to the next hop (Postfix on port 10026).
// Uses connection pool for improved performance.
func (f *SMTPFilter) forwardEmail(msg *EmailMessage) error {
	// Use connection pool to send email
	err := f.connPool.SendEmail(msg.From, msg.Recipients, msg.Data)
	if err != nil {
		return fmt.Errorf("failed to forward via pool: %w", err)
	}

	f.logger.Info().
		Str("queue_id", msg.QueueID).
		Str("from", msg.From).
		Int("recipients", len(msg.Recipients)).
		Int64("size", msg.Size).
		Msg("email forwarded")

	return nil
}

// Stats returns current filter statistics.
func (f *SMTPFilter) Stats() (received, forwarded, errors int64) {
	return f.received.Load(), f.forwarded.Load(), f.errors.Load()
}

// ActiveConnections returns the number of active SMTP connections.
func (f *SMTPFilter) ActiveConnections() int64 {
	return f.activeConns.Load()
}
