package parser

import (
	"testing"
	"time"

	"relay-agent/internal/repository"
)

// Sample Postfix log lines for testing
const (
	// Complete email flow
	logSmtpd       = "Dec 21 10:15:30 mail postfix/smtpd[12345]: ABC123: client=mail.example.com[192.0.2.1], sasl_username=sender@example.com, sasl_method=PLAIN"
	logCleanup     = "Dec 21 10:15:30 mail postfix/cleanup[12346]: ABC123: message-id=<test@example.com>"
	logHeader      = "Dec 21 10:15:30 mail postfix/cleanup[12346]: ABC123: header X-Mailgateway-Queue-ID: MGW-20241221-101530-ABC123"
	logQmgrFrom    = "Dec 21 10:15:30 mail postfix/qmgr[12347]: ABC123: from=<sender@example.com>, size=1234, nrcpt=1"
	logSmtpSent    = "Dec 21 10:15:32 mail postfix/smtp[12348]: ABC123: to=<user@gmail.com>, relay=gmail-smtp-in.l.google.com[142.250.153.27]:25, delay=2.1, delays=0.01/0.01/1.5/0.58, dsn=2.0.0, status=sent (250 2.0.0 OK 1234567890)"
	logQmgrRemoved = "Dec 21 10:15:32 mail postfix/qmgr[12347]: ABC123: removed"

	// Deferred email
	logDeferred = "Dec 21 10:15:32 mail postfix/smtp[12348]: DEF456: to=<user@example.com>, relay=mail.example.com[192.0.2.2]:25, delay=5.2, dsn=4.4.1, status=deferred (Connection timed out)"

	// Bounced email
	logBounced = "Dec 21 10:15:33 mail postfix/smtp[12349]: GHI789: to=<baduser@example.com>, relay=mail.example.com[192.0.2.3]:25, delay=1.5, dsn=5.1.1, status=bounced (User unknown)"

	// Known providers
	logGmail   = "Dec 21 10:15:34 mail postfix/smtp[12350]: JKL012: to=<test@gmail.com>, relay=gmail-smtp-in.l.google.com[1.2.3.4]:25, status=sent"
	logOutlook = "Dec 21 10:15:35 mail postfix/smtp[12351]: MNO345: to=<test@outlook.com>, relay=outlook-smtp-in.l.microsoft.com[5.6.7.8]:25, status=sent"
	logYahoo   = "Dec 21 10:15:36 mail postfix/smtp[12352]: PQR678: to=<test@yahoo.com>, relay=mta5.am0.yahoodns.net[9.10.11.12]:25, status=sent"
	logApple   = "Dec 21 10:15:37 mail postfix/smtp[12353]: STU901: to=<test@icloud.com>, relay=mx01.mail.icloud.com[13.14.15.16]:25, status=sent"
)

func TestNewParser(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	if parser == nil {
		t.Fatal("NewParser returned nil")
	}

	if parser.outputChan == nil {
		t.Error("outputChan not set")
	}

	if parser.pending == nil {
		t.Error("pending map not initialized")
	}

	if parser.providerMap == nil {
		t.Error("providerMap not initialized")
	}

	// Verify provider map has expected entries
	expectedProviders := []string{"gmail.com", "outlook.com", "yahoo.com", "icloud.com"}
	for _, domain := range expectedProviders {
		if _, ok := parser.providerMap[domain]; !ok {
			t.Errorf("providerMap missing domain: %s", domain)
		}
	}
}

func TestParseCompleteEmail(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	// Parse complete email flow
	lines := []string{
		logSmtpd,
		logCleanup,
		logHeader,
		logQmgrFrom,
		logSmtpSent,
	}

	for _, line := range lines {
		if err := parser.ParseLine(line); err != nil {
			t.Fatalf("ParseLine failed: %v", err)
		}
	}

	// Should have one email in output channel
	select {
	case email := <-outputChan:
		// Verify email fields
		if email.QueueID != "ABC123" {
			t.Errorf("Expected QueueID ABC123, got %s", email.QueueID)
		}
		if email.MailgatewayQueueID != "MGW-20241221-101530-ABC123" {
			t.Errorf("Expected MailgatewayQueueID MGW-20241221-101530-ABC123, got %s", email.MailgatewayQueueID)
		}
		if email.Sender != "sender@example.com" {
			t.Errorf("Expected Sender sender@example.com, got %s", email.Sender)
		}
		if email.Recipient != "user@gmail.com" {
			t.Errorf("Expected Recipient user@gmail.com, got %s", email.Recipient)
		}
		if email.RecipientDomain != "gmail.com" {
			t.Errorf("Expected RecipientDomain gmail.com, got %s", email.RecipientDomain)
		}
		if email.Provider != "Gmail" {
			t.Errorf("Expected Provider Gmail, got %s", email.Provider)
		}
		if email.Size != 1234 {
			t.Errorf("Expected Size 1234, got %d", email.Size)
		}
		if email.Status != "sent" {
			t.Errorf("Expected Status sent, got %s", email.Status)
		}
		if email.DSN != "2.0.0" {
			t.Errorf("Expected DSN 2.0.0, got %s", email.DSN)
		}
		if email.StatusMessage != "250 2.0.0 OK 1234567890" {
			t.Errorf("Expected StatusMessage '250 2.0.0 OK 1234567890', got '%s'", email.StatusMessage)
		}
		if email.RelayHost != "gmail-smtp-in.l.google.com" {
			t.Errorf("Expected RelayHost gmail-smtp-in.l.google.com, got %s", email.RelayHost)
		}
		if email.RelayIP != "142.250.153.27" {
			t.Errorf("Expected RelayIP 142.250.153.27, got %s", email.RelayIP)
		}
		if email.DeliveryTimeMs != 2100 {
			t.Errorf("Expected DeliveryTimeMs 2100, got %d", email.DeliveryTimeMs)
		}
		if email.Attempts != 1 {
			t.Errorf("Expected Attempts 1, got %d", email.Attempts)
		}

		// Return to pool
		repository.PutEmail(email)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Expected email in output channel")
	}
}

func TestParseDeferredEmail(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	// Deferred email should NOT be finalized (still in pending)
	if err := parser.ParseLine(logDeferred); err != nil {
		t.Fatalf("ParseLine failed: %v", err)
	}

	// Should have 1 pending email
	if count := parser.PendingCount(); count != 1 {
		t.Errorf("Expected 1 pending email, got %d", count)
	}

	// Should NOT have email in output channel
	select {
	case <-outputChan:
		t.Fatal("Deferred email should not be finalized")
	case <-time.After(10 * time.Millisecond):
		// Expected - no output
	}
}

func TestParseBouncedEmail(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	// Bounced email should be finalized immediately
	if err := parser.ParseLine(logBounced); err != nil {
		t.Fatalf("ParseLine failed: %v", err)
	}

	// Should have email in output channel
	select {
	case email := <-outputChan:
		if email.Status != "bounced" {
			t.Errorf("Expected Status bounced, got %s", email.Status)
		}
		if email.DSN != "5.1.1" {
			t.Errorf("Expected DSN 5.1.1, got %s", email.DSN)
		}
		if email.StatusMessage != "User unknown" {
			t.Errorf("Expected StatusMessage 'User unknown', got '%s'", email.StatusMessage)
		}
		repository.PutEmail(email)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Expected bounced email in output channel")
	}
}

func TestProviderDetection(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	tests := []struct {
		domain   string
		expected string
	}{
		{"gmail.com", "Gmail"},
		{"googlemail.com", "Gmail"},
		{"outlook.com", "Outlook"},
		{"hotmail.com", "Outlook"},
		{"live.com", "Outlook"},
		{"yahoo.com", "Yahoo"},
		{"ymail.com", "Yahoo"},
		{"icloud.com", "Apple"},
		{"me.com", "Apple"},
		{"mac.com", "Apple"},
		{"aol.com", "AOL"},
		{"company.com", "Company"},
		{"example.org", "Example"},
		{"test.net", "Test"},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			result := parser.detectProvider(tt.domain)
			if result != tt.expected {
				t.Errorf("detectProvider(%s) = %s, want %s", tt.domain, result, tt.expected)
			}
		})
	}
}

func TestExtractDomain(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	tests := []struct {
		email    string
		expected string
	}{
		{"user@gmail.com", "gmail.com"},
		{"test@example.org", "example.org"},
		{"admin@mail.company.com", "mail.company.com"},
		{"noemail", ""},
		{"@nodomain", ""},
		{"no@", ""},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			result := parser.extractDomain(tt.email)
			if result != tt.expected {
				t.Errorf("extractDomain(%s) = %s, want %s", tt.email, result, tt.expected)
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	tests := []struct {
		line   string
		month  time.Month
		day    int
		hour   int
		minute int
		second int
	}{
		{"Dec 21 10:15:30 rest of line", time.December, 21, 10, 15, 30},
		{"Jan  1 09:05:01 rest of line", time.January, 1, 9, 5, 1},
		{"Jul 15 23:59:59 rest of line", time.July, 15, 23, 59, 59},
		{"Feb  3 00:00:00 rest of line", time.February, 3, 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.line[:15], func(t *testing.T) {
			ts := parser.parseTimestamp(tt.line)
			if ts.IsZero() {
				t.Fatal("parseTimestamp returned zero time")
			}
			if ts.Month() != tt.month {
				t.Errorf("Expected month %v, got %v", tt.month, ts.Month())
			}
			if ts.Day() != tt.day {
				t.Errorf("Expected day %d, got %d", tt.day, ts.Day())
			}
			if ts.Hour() != tt.hour {
				t.Errorf("Expected hour %d, got %d", tt.hour, ts.Hour())
			}
			if ts.Minute() != tt.minute {
				t.Errorf("Expected minute %d, got %d", tt.minute, ts.Minute())
			}
			if ts.Second() != tt.second {
				t.Errorf("Expected second %d, got %d", tt.second, ts.Second())
			}
		})
	}
}

func TestParseDelay(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	tests := []struct {
		delay    string
		expected int64
	}{
		{"2.1", 2100},
		{"0.5", 500},
		{"10.25", 10250},
		{"1", 1000},
		{"0.001", 1}, // Rounds to 1ms
		{"5.999", 5999},
		{"0.12", 120},
	}

	for _, tt := range tests {
		t.Run(tt.delay, func(t *testing.T) {
			result := parser.parseDelay(tt.delay)
			if result != tt.expected {
				t.Errorf("parseDelay(%s) = %d, want %d", tt.delay, result, tt.expected)
			}
		})
	}
}

func TestFlush(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	// Parse partial email (no final status)
	lines := []string{
		logSmtpd,
		logQmgrFrom,
	}

	for _, line := range lines {
		if err := parser.ParseLine(line); err != nil {
			t.Fatalf("ParseLine failed: %v", err)
		}
	}

	// Should have 1 pending email
	if count := parser.PendingCount(); count != 1 {
		t.Errorf("Expected 1 pending email, got %d", count)
	}

	// Flush pending emails
	flushed := parser.Flush()
	if flushed != 1 {
		t.Errorf("Expected to flush 1 email, got %d", flushed)
	}

	// Should have no pending emails after flush
	if count := parser.PendingCount(); count != 0 {
		t.Errorf("Expected 0 pending emails after flush, got %d", count)
	}
}

func TestMultipleEmails(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	// Parse multiple emails interleaved
	lines := []string{
		"Dec 21 10:15:30 mail postfix/smtp[1]: ABC123: to=<user1@gmail.com>, status=sent",
		"Dec 21 10:15:31 mail postfix/smtp[2]: DEF456: to=<user2@outlook.com>, status=sent",
		"Dec 21 10:15:32 mail postfix/smtp[3]: GHI789: to=<user3@yahoo.com>, status=sent",
	}

	for _, line := range lines {
		if err := parser.ParseLine(line); err != nil {
			t.Fatalf("ParseLine failed: %v", err)
		}
	}

	// Should have 3 emails in output channel
	receivedCount := 0
	timeout := time.After(100 * time.Millisecond)
	for receivedCount < 3 {
		select {
		case email := <-outputChan:
			receivedCount++
			repository.PutEmail(email)
		case <-timeout:
			t.Fatalf("Expected 3 emails, received %d", receivedCount)
		}
	}
}

func TestEmptyLineHandling(t *testing.T) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	// Parse empty line (should not error)
	if err := parser.ParseLine(""); err != nil {
		t.Errorf("ParseLine with empty string should not error: %v", err)
	}

	// Parse non-postfix line (should not error)
	if err := parser.ParseLine("Some random log line"); err != nil {
		t.Errorf("ParseLine with non-postfix line should not error: %v", err)
	}
}

// Benchmark tests for zero-allocation verification

func BenchmarkParseLineSmtp(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	// Drain output channel in background
	go func() {
		for email := range outputChan {
			repository.PutEmail(email)
		}
	}()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.ParseLine(logSmtpSent)
	}
}

func BenchmarkParseLineQmgr(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.ParseLine(logQmgrFrom)
	}
}

func BenchmarkParseCompleteEmail(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	// Drain output channel in background
	go func() {
		for email := range outputChan {
			repository.PutEmail(email)
		}
	}()

	lines := []string{
		logSmtpd,
		logCleanup,
		logHeader,
		logQmgrFrom,
		logSmtpSent,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for _, line := range lines {
			_ = parser.ParseLine(line)
		}
	}
}

func BenchmarkProviderDetection(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	domains := []string{
		"gmail.com",
		"outlook.com",
		"yahoo.com",
		"company.com",
		"example.org",
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		domain := domains[i%len(domains)]
		_ = parser.detectProvider(domain)
	}
}

func BenchmarkExtractDomain(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	email := "user@example.com"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.extractDomain(email)
	}
}

func BenchmarkParseTimestamp(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	line := "Dec 21 10:15:30 mail postfix/smtp[1]: test"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseTimestamp(line)
	}
}

func BenchmarkParseDelay(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	delay := "2.125"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseDelay(delay)
	}
}
