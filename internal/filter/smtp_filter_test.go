package filter

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"relay-agent/internal/repository"
)

// TestSMTPFilterBasic tests basic SMTP filter operations.
func TestSMTPFilterBasic(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)

	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		t.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	// Verify filter is running
	if filter.listener == nil {
		t.Fatal("Listener not initialized")
	}

	// Check initial stats
	received, forwarded, errors := filter.Stats()
	if received != 0 || forwarded != 0 || errors != 0 {
		t.Errorf("Initial stats incorrect: received=%d, forwarded=%d, errors=%d",
			received, forwarded, errors)
	}
}

// TestSMTPSession tests a complete SMTP session.
func TestSMTPSession(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)

	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		t.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	// Connect to filter
	addr := filter.listener.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Helper functions
	readResponse := func() (int, string, error) {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimSpace(line)

		var code int
		var msg string
		fmt.Sscanf(line, "%d %s", &code, &msg)
		return code, line, nil
	}

	writeCommand := func(cmd string) error {
		writer.WriteString(cmd + "\r\n")
		return writer.Flush()
	}

	// Read greeting
	code, line, err := readResponse()
	if err != nil {
		t.Fatalf("Failed to read greeting: %v", err)
	}
	if code != 220 {
		t.Errorf("Expected 220, got %d: %s", code, line)
	}

	// Send EHLO
	if err := writeCommand("EHLO test.local"); err != nil {
		t.Fatalf("Failed to send EHLO: %v", err)
	}

	// Read EHLO responses (multiple lines)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("Failed to read EHLO response: %v", err)
		}
		line = strings.TrimSpace(line)

		// Last line starts with "250 " (space, not dash)
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}

	// Send MAIL FROM
	if err := writeCommand("MAIL FROM:<sender@example.com>"); err != nil {
		t.Fatalf("Failed to send MAIL FROM: %v", err)
	}
	code, line, err = readResponse()
	if err != nil {
		t.Fatalf("Failed to read MAIL response: %v", err)
	}
	if code != 250 {
		t.Errorf("Expected 250 for MAIL, got %d: %s", code, line)
	}

	// Send RCPT TO
	if err := writeCommand("RCPT TO:<recipient@example.com>"); err != nil {
		t.Fatalf("Failed to send RCPT TO: %v", err)
	}
	code, line, err = readResponse()
	if err != nil {
		t.Fatalf("Failed to read RCPT response: %v", err)
	}
	if code != 250 {
		t.Errorf("Expected 250 for RCPT, got %d: %s", code, line)
	}

	// Send QUIT
	if err := writeCommand("QUIT"); err != nil {
		t.Fatalf("Failed to send QUIT: %v", err)
	}
	code, line, err = readResponse()
	if err != nil {
		t.Fatalf("Failed to read QUIT response: %v", err)
	}
	if code != 221 {
		t.Errorf("Expected 221 for QUIT, got %d: %s", code, line)
	}
}

// TestParseHeaders tests header parsing.
func TestParseHeaders(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	tests := []struct {
		name     string
		data     string
		expected map[string]string
		subject  string
		queueID  string
	}{
		{
			name: "Basic headers",
			data: "From: sender@example.com\r\n" +
				"To: recipient@example.com\r\n" +
				"Subject: Test Email\r\n" +
				"Message-ID: <123@example.com>\r\n" +
				"X-Mailgateway-Queue-ID: ABC123\r\n" +
				"\r\n" +
				"Body text",
			expected: map[string]string{
				"From":                   "sender@example.com",
				"To":                     "recipient@example.com",
				"Subject":                "Test Email",
				"Message-ID":             "<123@example.com>",
				"X-Mailgateway-Queue-ID": "ABC123",
			},
			subject: "Test Email",
			queueID: "ABC123",
		},
		{
			name: "Folded headers",
			data: "Subject: This is a very long subject line\r\n" +
				" that continues on the next line\r\n" +
				" and another line\r\n" +
				"From: sender@example.com\r\n" +
				"\r\n" +
				"Body",
			expected: map[string]string{
				"Subject": "This is a very long subject line that continues on the next line and another line",
				"From":    "sender@example.com",
			},
			subject: "This is a very long subject line that continues on the next line and another line",
		},
		{
			name: "Multiple header types",
			data: "Date: Mon, 22 Dec 2025 10:00:00 +0000\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"Message-Id: <msg-456@test.com>\r\n" +
				"\r\n" +
				"Body",
			expected: map[string]string{
				"Date":         "Mon, 22 Dec 2025 10:00:00 +0000",
				"Content-Type": "text/plain; charset=utf-8",
				"Message-Id":   "<msg-456@test.com>",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &EmailMessage{
				Data:    []byte(tt.data),
				Headers: make(map[string]string),
			}

			filter.parseHeaders(msg)

			// Check expected headers
			for key, expectedVal := range tt.expected {
				if gotVal, ok := msg.Headers[key]; !ok {
					t.Errorf("Missing header %s", key)
				} else if gotVal != expectedVal {
					t.Errorf("Header %s: expected %q, got %q", key, expectedVal, gotVal)
				}
			}

			// Check subject
			if tt.subject != "" && msg.Subject != tt.subject {
				t.Errorf("Subject: expected %q, got %q", tt.subject, msg.Subject)
			}

			// Check queue ID
			if tt.queueID != "" && msg.QueueID != tt.queueID {
				t.Errorf("QueueID: expected %q, got %q", tt.queueID, msg.QueueID)
			}
		})
	}
}

// TestExtractAddress tests email address extraction.
func TestExtractAddress(t *testing.T) {
	session := &smtpSession{}

	tests := []struct {
		param    string
		expected string
	}{
		{"FROM:<user@example.com>", "user@example.com"},
		{"TO:<admin@test.com>", "admin@test.com"},
		{"FROM:<>", ""},
		{"<user@domain.com>", "user@domain.com"},
		{"FROM:<user+tag@example.com>", "user+tag@example.com"},
		{"<user@domain.com> SIZE=1234", "user@domain.com"},
		{"invalid", ""},
		{"<missing-bracket", ""},
	}

	for _, tt := range tests {
		t.Run(tt.param, func(t *testing.T) {
			result := session.extractAddress(tt.param)
			if result != tt.expected {
				t.Errorf("extractAddress(%q) = %q, want %q", tt.param, result, tt.expected)
			}
		})
	}
}

// TestProcessEmail tests email processing and channel sending.
func TestProcessEmail(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)

	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	msg := &EmailMessage{
		From:       "sender@example.com",
		Recipients: []string{"user1@example.com", "user2@test.org"},
		QueueID:    "QUEUE123",
		Size:       1024,
		ReceivedAt: time.Now(),
	}

	filter.processEmail(msg)

	// Should receive 2 emails (one per recipient)
	timeout := time.After(2 * time.Second)
	emails := make([]*repository.Email, 0, 2)

	for i := 0; i < 2; i++ {
		select {
		case email := <-emailChan:
			emails = append(emails, email)
		case <-timeout:
			t.Fatalf("Timeout waiting for email %d", i+1)
		}
	}

	// Verify emails
	if len(emails) != 2 {
		t.Fatalf("Expected 2 emails, got %d", len(emails))
	}

	for i, email := range emails {
		if email.Sender != msg.From {
			t.Errorf("Email %d: wrong sender: %s", i, email.Sender)
		}
		if email.MailgatewayQueueID != msg.QueueID {
			t.Errorf("Email %d: wrong queue ID: %s", i, email.MailgatewayQueueID)
		}
		if email.Size != msg.Size {
			t.Errorf("Email %d: wrong size: %d", i, email.Size)
		}
		if email.Status != "received" {
			t.Errorf("Email %d: wrong status: %s", i, email.Status)
		}
	}

	// Verify recipient domains
	expectedDomains := map[string]bool{"example.com": false, "test.org": false}
	for _, email := range emails {
		if _, ok := expectedDomains[email.RecipientDomain]; ok {
			expectedDomains[email.RecipientDomain] = true
		}
	}

	for domain, found := range expectedDomains {
		if !found {
			t.Errorf("Domain %s not found in emails", domain)
		}
	}
}

// TestSMTPFilterStats tests statistics tracking.
func TestSMTPFilterStats(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)

	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	// Increment counters
	filter.received.Add(10)
	filter.forwarded.Add(9)
	filter.errors.Add(1)

	received, forwarded, errors := filter.Stats()

	if received != 10 {
		t.Errorf("Expected 10 received, got %d", received)
	}
	if forwarded != 9 {
		t.Errorf("Expected 9 forwarded, got %d", forwarded)
	}
	if errors != 1 {
		t.Errorf("Expected 1 error, got %d", errors)
	}
}

// TestHeaderParsing tests various header formats.
func TestHeaderParsing(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	// Test with LF only (Unix style)
	msg := &EmailMessage{
		Data:    []byte("Subject: Test\nFrom: sender@example.com\n\nBody"),
		Headers: make(map[string]string),
	}

	filter.parseHeaders(msg)

	if msg.Subject != "Test" {
		t.Errorf("Subject parsing failed with LF: %q", msg.Subject)
	}

	// Test empty headers
	msg2 := &EmailMessage{
		Data:    []byte("\r\n\r\nBody only"),
		Headers: make(map[string]string),
	}

	filter.parseHeaders(msg2)

	if len(msg2.Headers) != 0 {
		t.Errorf("Expected empty headers, got %d", len(msg2.Headers))
	}
}

// BenchmarkParseHeaders benchmarks header parsing performance.
func BenchmarkParseHeaders(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	data := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Test Email with a reasonably long subject line\r\n" +
		"Message-ID: <1234567890.123456.123456789@example.com>\r\n" +
		"X-Mailgateway-Queue-ID: ABC123DEF456\r\n" +
		"Date: Mon, 22 Dec 2025 10:00:00 +0000\r\n" +
		"Content-Type: multipart/alternative; boundary=\"----=_Part_123_456.789\"\r\n" +
		"MIME-Version: 1.0\r\n" +
		"\r\n" +
		"Body content here")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		msg := &EmailMessage{
			Data:    data,
			Headers: make(map[string]string),
		}
		filter.parseHeaders(msg)
	}
}

// BenchmarkExtractAddress benchmarks address extraction.
func BenchmarkExtractAddress(b *testing.B) {
	session := &smtpSession{}
	param := "FROM:<user+tag@example.com> SIZE=1024"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = session.extractAddress(param)
	}
}

// TestXForward tests XFORWARD command handling.
func TestXForward(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)
	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		t.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	// Connect
	addr := filter.listener.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	// Read greeting
	reader.ReadString('\n')

	// Send EHLO
	writer.WriteString("EHLO test.local\r\n")
	writer.Flush()

	// Read EHLO responses
	for {
		line, _ := reader.ReadString('\n')
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}

	// Send XFORWARD
	writer.WriteString("XFORWARD NAME=client.example.com ADDR=192.168.1.100 HELO=client.local\r\n")
	writer.Flush()

	line, _ := reader.ReadString('\n')
	if !strings.HasPrefix(line, "250") {
		t.Errorf("XFORWARD failed: %s", line)
	}

	// Send QUIT
	writer.WriteString("QUIT\r\n")
	writer.Flush()
}

// TestConcurrentConnections tests multiple simultaneous connections.
func TestConcurrentConnections(t *testing.T) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 100)

	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		t.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	addr := filter.listener.Addr().String()

	// Create multiple concurrent connections
	numConns := 10
	errChan := make(chan error, numConns)

	for i := 0; i < numConns; i++ {
		go func(id int) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				errChan <- fmt.Errorf("conn %d: dial failed: %w", id, err)
				return
			}
			defer conn.Close()

			reader := bufio.NewReader(conn)
			writer := bufio.NewWriter(conn)

			// Read greeting
			if _, err := reader.ReadString('\n'); err != nil {
				errChan <- fmt.Errorf("conn %d: read greeting failed: %w", id, err)
				return
			}

			// Send QUIT
			writer.WriteString("QUIT\r\n")
			writer.Flush()

			errChan <- nil
		}(i)
	}

	// Wait for all connections
	for i := 0; i < numConns; i++ {
		if err := <-errChan; err != nil {
			t.Error(err)
		}
	}

	// Verify active connections go back to 0
	time.Sleep(100 * time.Millisecond)
	active := filter.ActiveConnections()
	if active != 0 {
		t.Errorf("Expected 0 active connections, got %d", active)
	}
}

// Example_smtpFilter demonstrates basic SMTP filter usage.
func Example_smtpFilter() {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 100)

	// Create filter
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)
	filter.SetHostname("mail.example.com")

	// Start filter
	ctx := context.Background()
	if err := filter.Start(ctx); err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		return
	}
	defer filter.Stop()

	// Process emails from channel
	go func() {
		for email := range emailChan {
			fmt.Printf("Received email: %s -> %s\n", email.Sender, email.Recipient)
			repository.PutEmail(email)
		}
	}()

	// Run for some time...
	// In production, this would run indefinitely

	// Get stats
	received, forwarded, errors := filter.Stats()
	fmt.Printf("Stats: received=%d, forwarded=%d, errors=%d\n", received, forwarded, errors)
}
