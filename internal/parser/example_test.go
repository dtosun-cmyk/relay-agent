package parser_test

import (
	"bufio"
	"fmt"
	"log"
	"strings"

	"relay-agent/internal/parser"
	"relay-agent/internal/repository"
)

// Example demonstrates how to use the Parser to process Postfix log lines.
func Example() {
	// Create output channel for completed emails
	outputChan := make(chan *repository.Email, 100)

	// Create parser
	p := parser.NewParser(outputChan)

	// Sample Postfix log lines (typical email flow)
	logLines := []string{
		"Dec 21 10:15:30 mail postfix/smtpd[12345]: ABC123: client=mail.example.com[192.0.2.1], sasl_username=sender@example.com",
		"Dec 21 10:15:30 mail postfix/cleanup[12346]: ABC123: message-id=<test@example.com>",
		"Dec 21 10:15:30 mail postfix/cleanup[12346]: ABC123: header X-Mailgateway-Queue-ID: MGW-20241221-101530-ABC123",
		"Dec 21 10:15:30 mail postfix/qmgr[12347]: ABC123: from=<sender@example.com>, size=1234, nrcpt=1",
		"Dec 21 10:15:32 mail postfix/smtp[12348]: ABC123: to=<user@gmail.com>, relay=gmail-smtp-in.l.google.com[142.250.153.27]:25, delay=2.1, dsn=2.0.0, status=sent (250 2.0.0 OK)",
	}

	// Start goroutine to process completed emails
	done := make(chan bool)
	go func() {
		for email := range outputChan {
			fmt.Printf("Email: %s -> %s [%s]\n", email.Sender, email.Recipient, email.Status)
			// In real application, would save to database here
			repository.PutEmail(email) // Return to pool
		}
		done <- true
	}()

	// Parse log lines
	for _, line := range logLines {
		if err := p.ParseLine(line); err != nil {
			log.Printf("Parse error: %v", err)
		}
	}

	// Close output channel and wait for processing
	close(outputChan)
	<-done

	// Output:
	// Email: sender@example.com -> user@gmail.com [sent]
}

// ExampleParser_streaming demonstrates streaming log processing.
func ExampleParser_streaming() {
	// Create buffered output channel
	outputChan := make(chan *repository.Email, 1000)
	p := parser.NewParser(outputChan)

	// Simulate reading from log file
	logContent := `Dec 21 10:15:30 mail postfix/smtp[1]: EMAIL1: to=<user1@gmail.com>, status=sent
Dec 21 10:15:31 mail postfix/smtp[2]: EMAIL2: to=<user2@outlook.com>, status=sent
Dec 21 10:15:32 mail postfix/smtp[3]: EMAIL3: to=<user3@yahoo.com>, status=bounced (User unknown)`

	reader := strings.NewReader(logContent)
	scanner := bufio.NewScanner(reader)

	// Process emails in background
	emailCount := 0
	done := make(chan bool)
	go func() {
		for email := range outputChan {
			emailCount++
			fmt.Printf("Provider: %s, Status: %s\n", email.Provider, email.Status)
			repository.PutEmail(email)
		}
		done <- true
	}()

	// Parse lines as they arrive
	for scanner.Scan() {
		line := scanner.Text()
		_ = p.ParseLine(line)
	}

	close(outputChan)
	<-done

	fmt.Printf("Processed %d emails\n", emailCount)

	// Output:
	// Provider: Gmail, Status: sent
	// Provider: Outlook, Status: sent
	// Provider: Yahoo, Status: bounced
	// Processed 3 emails
}

// ExampleParser_Flush demonstrates flushing pending emails on shutdown.
func ExampleParser_Flush() {
	outputChan := make(chan *repository.Email, 10)
	p := parser.NewParser(outputChan)

	// Parse incomplete email (only sender info, no delivery status yet)
	_ = p.ParseLine("Dec 21 10:15:30 mail postfix/qmgr[1]: ABC123: from=<sender@example.com>, size=5000")

	fmt.Printf("Pending emails: %d\n", p.PendingCount())

	// Flush pending emails (useful on shutdown)
	flushed := p.Flush()
	fmt.Printf("Flushed emails: %d\n", flushed)
	fmt.Printf("Pending after flush: %d\n", p.PendingCount())

	// Clean up
	close(outputChan)
	for email := range outputChan {
		repository.PutEmail(email)
	}

	// Output:
	// Pending emails: 1
	// Flushed emails: 1
	// Pending after flush: 0
}

// ExampleParser_multipleRecipients demonstrates handling multiple recipients.
func ExampleParser_multipleRecipients() {
	outputChan := make(chan *repository.Email, 10)
	p := parser.NewParser(outputChan)

	// One email sent to multiple recipients
	// Each recipient gets a separate log entry with the same queue ID
	logLines := []string{
		"Dec 21 10:15:30 mail postfix/qmgr[1]: ABC123: from=<sender@example.com>, size=1000, nrcpt=3",
		"Dec 21 10:15:31 mail postfix/smtp[2]: ABC123: to=<user1@gmail.com>, status=sent",
		"Dec 21 10:15:31 mail postfix/smtp[3]: ABC123: to=<user2@gmail.com>, status=sent",
		"Dec 21 10:15:31 mail postfix/smtp[4]: ABC123: to=<user3@gmail.com>, status=sent",
	}

	done := make(chan bool)
	emails := make([]*repository.Email, 0)
	go func() {
		for email := range outputChan {
			emails = append(emails, email)
		}
		done <- true
	}()

	for _, line := range logLines {
		_ = p.ParseLine(line)
	}

	close(outputChan)
	<-done

	fmt.Printf("Total emails: %d\n", len(emails))
	for _, email := range emails {
		fmt.Printf("Recipient: %s\n", email.Recipient)
		repository.PutEmail(email)
	}

	// Output:
	// Total emails: 3
	// Recipient: user1@gmail.com
	// Recipient: user2@gmail.com
	// Recipient: user3@gmail.com
}

// ExampleParser_deferredRetry demonstrates deferred email handling.
func ExampleParser_deferredRetry() {
	outputChan := make(chan *repository.Email, 10)
	p := parser.NewParser(outputChan)

	// Parse first two lines (setup and deferred)
	_ = p.ParseLine("Dec 21 10:15:30 mail postfix/qmgr[1]: ABC123: from=<sender@example.com>, size=1000")
	_ = p.ParseLine("Dec 21 10:15:31 mail postfix/smtp[2]: ABC123: to=<user@example.com>, status=deferred (Connection timeout)")

	// After deferred, email is still pending (not finalized)
	fmt.Printf("Pending after deferred: %d\n", p.PendingCount())

	// Now process final delivery
	emailsSent := 0
	done := make(chan bool)
	go func() {
		for email := range outputChan {
			fmt.Printf("Status: %s, Attempts: %d\n", email.Status, email.Attempts)
			emailsSent++
			repository.PutEmail(email)
		}
		done <- true
	}()

	// Email is retried and sent
	_ = p.ParseLine("Dec 21 10:20:31 mail postfix/smtp[3]: ABC123: to=<user@example.com>, status=sent (250 OK)")

	close(outputChan)
	<-done

	// Only one email finalized (when sent)
	fmt.Printf("Emails finalized: %d\n", emailsSent)

	// Output:
	// Pending after deferred: 1
	// Status: sent, Attempts: 2
	// Emails finalized: 1
}
