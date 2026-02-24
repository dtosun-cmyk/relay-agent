package parser

import (
	"bytes"
	"regexp"
	"sync"
	"testing"

	"relay-agent/internal/repository"
)

// Test data - realistic Postfix log lines
var testLines = struct {
	smtp    string
	qmgr    string
	cleanup string
	smtpd   string
}{
	smtp:    `Dec 21 10:15:32 relay1 postfix/smtp[1236]: ABC123DEF: to=<user@gmail.com>, relay=gmail-smtp-in.l.google.com[142.250.1.27]:25, delay=2.1, delays=0.1/0.01/1/1, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
	qmgr:    `Dec 21 10:15:30 relay1 postfix/qmgr[1234]: ABC123DEF: from=<sender@example.com>, size=1234, nrcpt=1 (queue active)`,
	cleanup: `Dec 21 10:15:30 relay1 postfix/cleanup[1235]: ABC123DEF: info: header X-Mailgateway-Queue-ID: MGW-20241221-101532-ABC123`,
	smtpd:   `Dec 21 10:15:30 relay1 postfix/smtpd[1234]: ABC123DEF: client=mailgateway[192.0.2.1], sasl_method=PLAIN, sasl_username=mailgateway`,
}

// Complete email flow for full lifecycle testing
var completeEmailFlow = []string{
	`Dec 21 10:15:30 relay1 postfix/smtpd[1234]: ABC123XYZ: client=mail.example.com[192.0.2.1], sasl_username=sender@example.com, sasl_method=PLAIN`,
	`Dec 21 10:15:30 relay1 postfix/cleanup[1235]: ABC123XYZ: message-id=<test@example.com>`,
	`Dec 21 10:15:30 relay1 postfix/cleanup[1235]: ABC123XYZ: header X-Mailgateway-Queue-ID: MGW-20241221-101530-ABC123`,
	`Dec 21 10:15:30 relay1 postfix/qmgr[1236]: ABC123XYZ: from=<sender@example.com>, size=5678, nrcpt=1`,
	`Dec 21 10:15:32 relay1 postfix/smtp[1237]: ABC123XYZ: to=<user@gmail.com>, relay=gmail-smtp-in.l.google.com[142.250.1.27]:25, delay=2.1, delays=0.1/0.01/1/1, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
}

// ============================================================================
// 1. Parser Line Benchmarks
// ============================================================================

// BenchmarkParseLine_Smtp benchmarks SMTP delivery status line parsing.
// This is the most common and performance-critical line type.
func BenchmarkParseLine_Smtp(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	// Drain output channel in background
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.ParseLine(testLines.smtp)
	}
}

// BenchmarkParseLine_Qmgr benchmarks queue manager line parsing.
// Tests sender extraction and size parsing.
func BenchmarkParseLine_Qmgr(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.ParseLine(testLines.qmgr)
	}
}

// BenchmarkParseLine_Cleanup benchmarks cleanup/header line parsing.
// Tests header extraction including X-Mailgateway-Queue-ID.
func BenchmarkParseLine_Cleanup(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.ParseLine(testLines.cleanup)
	}
}

// BenchmarkParseLine_Smtpd benchmarks client connection line parsing.
// Tests client IP and SASL username extraction.
func BenchmarkParseLine_Smtpd(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.ParseLine(testLines.smtpd)
	}
}

// BenchmarkParseCompleteEmail_Lifecycle benchmarks full email lifecycle parsing.
// This measures end-to-end performance from connection to delivery.
func BenchmarkParseCompleteEmail_Lifecycle(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	// Drain output channel in background
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for _, line := range completeEmailFlow {
			_ = parser.ParseLine(line)
		}
	}
}

// BenchmarkParseCompleteEmail_Parallel tests concurrent parsing performance.
// Note: Each goroutine gets its own parser instance to avoid contention.
func BenchmarkParseCompleteEmail_Parallel(b *testing.B) {
	outputChan := make(chan *repository.Email, 10000)

	// Drain output channel in background
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		// Each goroutine gets its own parser to avoid lock contention
		parser := NewParser(outputChan)
		for pb.Next() {
			for _, line := range completeEmailFlow {
				_ = parser.ParseLine(line)
			}
		}
	})
}

// ============================================================================
// 2. Zero-Allocation Helper Benchmarks
// ============================================================================

// BenchmarkExtractDomain_Basic benchmarks domain extraction from email addresses.
// Target: 0 allocs/op
func BenchmarkExtractDomain_Basic(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	email := "user@example.com"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.extractDomain(email)
	}
}

// BenchmarkExtractDomain_LongEmail tests extraction with longer email addresses.
func BenchmarkExtractDomain_LongEmail(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	email := "very.long.email.address.with.many.dots@subdomain.example.com"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.extractDomain(email)
	}
}

// BenchmarkParseTimestamp_Standard benchmarks custom timestamp parsing.
// Target: 0 allocs/op (vs time.Parse which allocates)
func BenchmarkParseTimestamp_Standard(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	line := "Dec 21 10:15:30 relay1 postfix/smtp[1236]: test"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseTimestamp(line)
	}
}

// BenchmarkParseTimestamp_SingleDigitDay tests single-digit day parsing.
func BenchmarkParseTimestamp_SingleDigitDay(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	line := "Jan  1 09:05:01 relay1 postfix/smtp[1236]: test"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseTimestamp(line)
	}
}

// BenchmarkParseDelay_Decimal benchmarks delay string to milliseconds conversion.
// Target: 0 allocs/op
func BenchmarkParseDelay_Decimal(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	delay := "2.125"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseDelay(delay)
	}
}

// BenchmarkParseDelay_Integer tests integer delay parsing.
func BenchmarkParseDelay_Integer(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	delay := "5"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseDelay(delay)
	}
}

// BenchmarkParseDelay_LongDecimal tests delay with many decimal places.
func BenchmarkParseDelay_LongDecimal(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	delay := "123.456789"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parser.parseDelay(delay)
	}
}

// BenchmarkDetectProvider benchmarks provider detection with map lookup.
// Target: 0 allocs/op for known providers
func BenchmarkDetectProvider(b *testing.B) {
	outputChan := make(chan *repository.Email, 10)
	parser := NewParser(outputChan)

	b.Run("KnownProvider_Gmail", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = parser.detectProvider("gmail.com")
		}
	})

	b.Run("KnownProvider_Outlook", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = parser.detectProvider("outlook.com")
		}
	})

	b.Run("KnownProvider_Yahoo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = parser.detectProvider("yahoo.com")
		}
	})

	b.Run("UnknownProvider", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = parser.detectProvider("company.com")
		}
	})
}

// ============================================================================
// 3. Regex Pattern Benchmarks
// ============================================================================

// BenchmarkRegex_QueueID benchmarks queue ID extraction performance.
func BenchmarkRegex_QueueID(b *testing.B) {
	line := "Dec 21 10:15:30 relay1 postfix/smtp[1236]: ABC123DEF: test message"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = queueIDRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_Recipient benchmarks recipient email extraction.
func BenchmarkRegex_Recipient(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = recipientRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_Status benchmarks status extraction (sent/deferred/bounced).
func BenchmarkRegex_Status(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = statusRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_Relay benchmarks relay host and IP extraction.
func BenchmarkRegex_Relay(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = relayRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_DSN benchmarks DSN code extraction.
func BenchmarkRegex_DSN(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = dsnRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_Delay benchmarks delay value extraction.
func BenchmarkRegex_Delay(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = delayRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_From benchmarks sender address extraction.
func BenchmarkRegex_From(b *testing.B) {
	line := testLines.qmgr

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = fromRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_Process benchmarks process type extraction.
func BenchmarkRegex_Process(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = processRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_Client benchmarks client hostname and IP extraction.
func BenchmarkRegex_Client(b *testing.B) {
	line := testLines.smtpd

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = clientRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_SASL benchmarks SASL username extraction.
func BenchmarkRegex_SASL(b *testing.B) {
	line := testLines.smtpd

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = saslRegex.FindStringSubmatch(line)
	}
}

// BenchmarkRegex_AllPatterns benchmarks all regex patterns on a line.
// This simulates worst-case where all patterns are tried.
func BenchmarkRegex_AllPatterns(b *testing.B) {
	line := testLines.smtp

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = processRegex.FindStringSubmatch(line)
		_ = queueIDRegex.FindStringSubmatch(line)
		_ = recipientRegex.FindStringSubmatch(line)
		_ = relayRegex.FindStringSubmatch(line)
		_ = delayRegex.FindStringSubmatch(line)
		_ = dsnRegex.FindStringSubmatch(line)
		_ = statusRegex.FindStringSubmatch(line)
		_ = statusMessageRegex.FindStringSubmatch(line)
	}
}

// ============================================================================
// 4. Object Pool Benchmarks
// ============================================================================

// BenchmarkLogEntryPool benchmarks LogEntry pool performance.
// Target: 0 allocs/op after pool warmup
func BenchmarkLogEntryPool(b *testing.B) {
	// Warmup pool
	entries := make([]*repository.LogEntry, 100)
	for i := range entries {
		entries[i] = repository.GetLogEntry()
	}
	for _, e := range entries {
		repository.PutLogEntry(e)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		entry := repository.GetLogEntry()
		repository.PutLogEntry(entry)
	}
}

// BenchmarkEmailPool benchmarks Email pool performance.
// Target: 0 allocs/op after pool warmup
func BenchmarkEmailPool(b *testing.B) {
	// Warmup pool
	emails := make([]*repository.Email, 100)
	for i := range emails {
		emails[i] = repository.GetEmail()
	}
	for _, e := range emails {
		repository.PutEmail(e)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		email := repository.GetEmail()
		repository.PutEmail(email)
	}
}

// BenchmarkBufferPool benchmarks the internal buffer pool.
func BenchmarkBufferPool(b *testing.B) {
	pool := &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}

	// Warmup pool
	buffers := make([]*bytes.Buffer, 100)
	for i := range buffers {
		buffers[i] = pool.Get().(*bytes.Buffer)
	}
	for _, buf := range buffers {
		buf.Reset()
		pool.Put(buf)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := pool.Get().(*bytes.Buffer)
		buf.WriteString("test")
		buf.Reset()
		pool.Put(buf)
	}
}

// BenchmarkLogEntryPool_Concurrent tests pool under concurrent load.
func BenchmarkLogEntryPool_Concurrent(b *testing.B) {
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			entry := repository.GetLogEntry()
			entry.QueueID = "TEST123"
			entry.Sender = "test@example.com"
			repository.PutLogEntry(entry)
		}
	})
}

// BenchmarkEmailPool_Concurrent tests Email pool under concurrent load.
func BenchmarkEmailPool_Concurrent(b *testing.B) {
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			email := repository.GetEmail()
			email.QueueID = "TEST123"
			email.Sender = "test@example.com"
			repository.PutEmail(email)
		}
	})
}

// ============================================================================
// 5. Throughput Benchmarks
// ============================================================================

// BenchmarkParserThroughput measures lines per second processing rate.
func BenchmarkParserThroughput(b *testing.B) {
	outputChan := make(chan *repository.Email, 10000)
	parser := NewParser(outputChan)

	// Drain output channel in background
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	// Mix of different line types
	lines := []string{
		testLines.smtpd,
		testLines.cleanup,
		testLines.qmgr,
		testLines.smtp,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		line := lines[i%len(lines)]
		_ = parser.ParseLine(line)
	}

	// Report throughput
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "lines/sec")
}

// BenchmarkParserThroughput_1000Lines measures throughput for 1000-line batches.
func BenchmarkParserThroughput_1000Lines(b *testing.B) {
	outputChan := make(chan *repository.Email, 10000)
	parser := NewParser(outputChan)

	// Drain output channel in background
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	// Generate 1000 lines
	lines := make([]string, 1000)
	for i := range lines {
		lines[i] = completeEmailFlow[i%len(completeEmailFlow)]
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		for _, line := range lines {
			_ = parser.ParseLine(line)
		}
	}

	// Report throughput and bytes processed
	totalLines := b.N * len(lines)
	b.ReportMetric(float64(totalLines)/b.Elapsed().Seconds(), "lines/sec")
}

// BenchmarkParserMemory measures memory usage per 1000 lines.
func BenchmarkParserMemory(b *testing.B) {
	b.Run("Memory_1000Lines", func(b *testing.B) {
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			outputChan := make(chan *repository.Email, 10000)
			parser := NewParser(outputChan)

			// Drain output channel in background
			done := make(chan struct{})
			go func() {
				for {
					select {
					case email := <-outputChan:
						repository.PutEmail(email)
					case <-done:
						return
					}
				}
			}()

			// Parse 1000 lines
			for j := 0; j < 1000; j++ {
				line := completeEmailFlow[j%len(completeEmailFlow)]
				_ = parser.ParseLine(line)
			}

			close(done)
		}
	})
}

// ============================================================================
// 6. Comparison Benchmarks (Optimization Impact)
// ============================================================================

// BenchmarkWithPool_vs_WithoutPool compares pool vs direct allocation.
func BenchmarkWithPool_vs_WithoutPool(b *testing.B) {
	b.Run("WithPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			entry := repository.GetLogEntry()
			entry.QueueID = "TEST123"
			entry.Sender = "test@example.com"
			entry.Recipient = "user@gmail.com"
			repository.PutLogEntry(entry)
		}
	})

	b.Run("WithoutPool", func(b *testing.B) {
		var entry *repository.LogEntry
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			entry = &repository.LogEntry{}
			entry.QueueID = "TEST123"
			entry.Sender = "test@example.com"
			entry.Recipient = "user@gmail.com"
			// No pool return - gets garbage collected
		}
		// Prevent compiler from optimizing away
		if entry != nil && entry.QueueID == "" {
			b.Fatal("unexpected")
		}
	})
}

// BenchmarkPrecompiled_vs_Runtime compares precompiled vs runtime regex.
func BenchmarkPrecompiled_vs_Runtime(b *testing.B) {
	line := testLines.smtp

	b.Run("Precompiled", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = recipientRegex.FindStringSubmatch(line)
		}
	})

	b.Run("RuntimeCompiled", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			re := regexp.MustCompile(`to=<([^>]+)>`)
			_ = re.FindStringSubmatch(line)
		}
	})
}

// BenchmarkStringBuilding_Buffer_vs_Concat compares buffer pool vs string concat.
func BenchmarkStringBuilding_Buffer_vs_Concat(b *testing.B) {
	pool := &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}

	parts := []string{"part1", "part2", "part3", "part4", "part5"}

	b.Run("WithBufferPool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := pool.Get().(*bytes.Buffer)
			buf.Reset()
			for _, part := range parts {
				buf.WriteString(part)
			}
			_ = buf.String()
			pool.Put(buf)
		}
	})

	b.Run("WithConcat", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			result := ""
			for _, part := range parts {
				result += part
			}
			_ = result
		}
	})
}

// BenchmarkMapLookup_vs_Switch compares map lookup vs switch statement.
func BenchmarkMapLookup_vs_Switch(b *testing.B) {
	providerMap := map[string]string{
		"gmail.com":   "Gmail",
		"outlook.com": "Outlook",
		"yahoo.com":   "Yahoo",
		"icloud.com":  "Apple",
	}

	domains := []string{"gmail.com", "outlook.com", "yahoo.com", "icloud.com", "unknown.com"}

	b.Run("MapLookup", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			domain := domains[i%len(domains)]
			_, _ = providerMap[domain]
		}
	})

	b.Run("SwitchStatement", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			domain := domains[i%len(domains)]
			var provider string
			switch domain {
			case "gmail.com":
				provider = "Gmail"
			case "outlook.com":
				provider = "Outlook"
			case "yahoo.com":
				provider = "Yahoo"
			case "icloud.com":
				provider = "Apple"
			default:
				provider = ""
			}
			_ = provider
		}
	})
}

// ============================================================================
// Additional Performance Tests
// ============================================================================

// BenchmarkParseLineTypes benchmarks each process type separately.
func BenchmarkParseLineTypes(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	// Drain output channel
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	testCases := []struct {
		name string
		line string
	}{
		{"SMTPD", testLines.smtpd},
		{"Cleanup", testLines.cleanup},
		{"QMGR", testLines.qmgr},
		{"SMTP", testLines.smtp},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = parser.ParseLine(tc.line)
			}
		})
	}
}

// BenchmarkPendingMapOperations benchmarks pending map operations.
func BenchmarkPendingMapOperations(b *testing.B) {
	outputChan := make(chan *repository.Email, 1000)
	parser := NewParser(outputChan)

	b.Run("MapInsert", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			queueID := "QUEUE" + string(rune('A'+i%26))
			_ = parser.ParseLine("Dec 21 10:15:30 relay1 postfix/qmgr[1234]: " + queueID + ": from=<test@example.com>, size=1234")
		}
	})

	b.Run("MapLookup", func(b *testing.B) {
		// Pre-populate map
		for i := 0; i < 100; i++ {
			queueID := "QUEUE" + string(rune('A'+i%26))
			_ = parser.ParseLine("Dec 21 10:15:30 relay1 postfix/qmgr[1234]: " + queueID + ": from=<test@example.com>, size=1234")
		}

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			queueID := "QUEUE" + string(rune('A'+i%26))
			_ = parser.ParseLine("Dec 21 10:15:30 relay1 postfix/smtp[1236]: " + queueID + ": to=<user@gmail.com>, status=sent")
		}
	})
}

// BenchmarkFlush benchmarks the flush operation.
func BenchmarkFlush(b *testing.B) {
	outputChan := make(chan *repository.Email, 10000)

	// Drain output channel
	done := make(chan struct{})
	go func() {
		for {
			select {
			case email := <-outputChan:
				repository.PutEmail(email)
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		parser := NewParser(outputChan)

		// Add 100 pending entries
		for j := 0; j < 100; j++ {
			_ = parser.ParseLine("Dec 21 10:15:30 relay1 postfix/qmgr[1234]: QUEUE" + string(rune('A'+j%26)) + ": from=<test@example.com>, size=1234")
		}

		// Flush all pending
		_ = parser.Flush()
	}
}
