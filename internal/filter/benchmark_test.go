package filter

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"relay-agent/internal/repository"
)

// BenchmarkHeaderParsing benchmarks different header parsing strategies.
func BenchmarkHeaderParsing(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	testData := []byte(
		"Return-Path: <sender@example.com>\r\n" +
			"Received: from mail.example.com ([192.168.1.100])\r\n" +
			"\tby localhost with ESMTP id ABC123\r\n" +
			"\tfor <recipient@test.com>; Mon, 22 Dec 2025 10:00:00 +0000\r\n" +
			"From: sender@example.com\r\n" +
			"To: recipient@test.com\r\n" +
			"Subject: This is a test email with a moderately long subject line\r\n" +
			"\tthat continues on multiple lines for testing purposes\r\n" +
			"Message-ID: <20251222100000.ABC123@example.com>\r\n" +
			"X-Mailgateway-Queue-ID: GATEWAY-ABC123-DEF456\r\n" +
			"Date: Mon, 22 Dec 2025 10:00:00 +0000\r\n" +
			"Content-Type: multipart/alternative;\r\n" +
			"\tboundary=\"----=_Part_123456_789012.1234567890123\"\r\n" +
			"MIME-Version: 1.0\r\n" +
			"X-Mailer: Custom Mailer 1.0\r\n" +
			"X-Custom-Header: Custom Value Here\r\n" +
			"\r\n" +
			"Body content starts here\r\n")

	b.Run("ParseHeaders", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			msg := &EmailMessage{
				Data:    testData,
				Headers: make(map[string]string, 20),
			}
			filter.parseHeaders(msg)
		}
	})

	b.Run("ParseHeaders_NoAlloc", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		// Reuse map
		headers := make(map[string]string, 20)
		for i := 0; i < b.N; i++ {
			// Clear map
			for k := range headers {
				delete(headers, k)
			}

			msg := &EmailMessage{
				Data:    testData,
				Headers: headers,
			}
			filter.parseHeaders(msg)
		}
	})
}

// BenchmarkAddressExtraction benchmarks email address extraction.
func BenchmarkAddressExtraction(b *testing.B) {
	session := &smtpSession{}

	testCases := []struct {
		name  string
		param string
	}{
		{"Simple", "FROM:<user@example.com>"},
		{"WithSize", "FROM:<user@example.com> SIZE=12345"},
		{"WithTag", "FROM:<user+tag@example.com>"},
		{"Complex", "TO:<user+tag@subdomain.example.com> SIZE=999999"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_ = session.extractAddress(tc.param)
			}
		})
	}
}

// BenchmarkSMTPSession benchmarks a complete SMTP session.
func BenchmarkSMTPSession(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 1000)

	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		b.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	// Drain email channel
	go func() {
		for email := range emailChan {
			repository.PutEmail(email)
		}
	}()

	addr := filter.listener.Addr().String()

	b.Run("Connect_EHLO_QUIT", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatal(err)
			}

			reader := bufio.NewReader(conn)
			writer := bufio.NewWriter(conn)

			// Read greeting
			reader.ReadString('\n')

			// EHLO
			writer.WriteString("EHLO test.local\r\n")
			writer.Flush()

			// Read EHLO responses
			for {
				line, _ := reader.ReadString('\n')
				if len(line) > 4 && line[3] == ' ' {
					break
				}
			}

			// QUIT
			writer.WriteString("QUIT\r\n")
			writer.Flush()
			reader.ReadString('\n')

			conn.Close()
		}
	})

	b.Run("FullTransaction_NoData", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatal(err)
			}

			reader := bufio.NewReader(conn)
			writer := bufio.NewWriter(conn)

			// Greeting
			reader.ReadString('\n')

			// EHLO
			writer.WriteString("EHLO test.local\r\n")
			writer.Flush()
			for {
				line, _ := reader.ReadString('\n')
				if len(line) > 4 && line[3] == ' ' {
					break
				}
			}

			// MAIL FROM
			writer.WriteString("MAIL FROM:<sender@example.com>\r\n")
			writer.Flush()
			reader.ReadString('\n')

			// RCPT TO
			writer.WriteString("RCPT TO:<recipient@example.com>\r\n")
			writer.Flush()
			reader.ReadString('\n')

			// RSET (instead of DATA)
			writer.WriteString("RSET\r\n")
			writer.Flush()
			reader.ReadString('\n')

			// QUIT
			writer.WriteString("QUIT\r\n")
			writer.Flush()
			reader.ReadString('\n')

			conn.Close()
		}
	})
}

// BenchmarkConcurrentSessions benchmarks multiple concurrent SMTP sessions.
func BenchmarkConcurrentSessions(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10000)

	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		b.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	// Drain channel
	go func() {
		for email := range emailChan {
			repository.PutEmail(email)
		}
	}()

	addr := filter.listener.Addr().String()

	concurrencies := []int{1, 10, 50, 100}

	for _, concurrency := range concurrencies {
		b.Run(fmt.Sprintf("Concurrency_%d", concurrency), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					conn, err := net.Dial("tcp", addr)
					if err != nil {
						b.Error(err)
						continue
					}

					reader := bufio.NewReader(conn)
					writer := bufio.NewWriter(conn)

					// Read greeting
					reader.ReadString('\n')

					// QUIT immediately
					writer.WriteString("QUIT\r\n")
					writer.Flush()
					reader.ReadString('\n')

					conn.Close()
				}
			})
		})
	}
}

// BenchmarkEmailProcessing benchmarks email data processing.
func BenchmarkEmailProcessing(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10000)
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	// Drain channel
	go func() {
		for email := range emailChan {
			repository.PutEmail(email)
		}
	}()

	msg := &EmailMessage{
		From:       "sender@example.com",
		Recipients: []string{"user1@example.com", "user2@test.org", "user3@company.com"},
		QueueID:    "QUEUE123",
		Size:       12345,
		ReceivedAt: time.Now(),
	}

	b.Run("ProcessEmail_3Recipients", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			filter.processEmail(msg)
		}
	})

	b.Run("ProcessEmail_10Recipients", func(b *testing.B) {
		msg := &EmailMessage{
			From: "sender@example.com",
			Recipients: []string{
				"user1@example.com", "user2@test.org", "user3@company.com",
				"user4@example.com", "user5@test.org", "user6@company.com",
				"user7@example.com", "user8@test.org", "user9@company.com",
				"user10@example.com",
			},
			QueueID:    "QUEUE123",
			Size:       12345,
			ReceivedAt: time.Now(),
		}

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			filter.processEmail(msg)
		}
	})
}

// BenchmarkByteOperations benchmarks byte slice operations.
func BenchmarkByteOperations(b *testing.B) {
	data := []byte("From: sender@example.com\r\nTo: recipient@example.com\r\n\r\nBody")

	b.Run("BytesIndex", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			_ = bytes.Index(data, []byte("\r\n\r\n"))
		}
	})

	b.Run("BytesSplit", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			_ = bytes.Split(data, []byte("\r\n"))
		}
	})
}

// BenchmarkMemoryAllocation benchmarks memory allocation patterns.
func BenchmarkMemoryAllocation(b *testing.B) {
	b.Run("NewEmail", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			email := &repository.Email{}
			email.Sender = "sender@example.com"
			email.Recipient = "recipient@example.com"
			email.Size = 1024
			_ = email
		}
	})

	b.Run("GetEmail_Pool", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			email := repository.GetEmail()
			email.Sender = "sender@example.com"
			email.Recipient = "recipient@example.com"
			email.Size = 1024
			repository.PutEmail(email)
		}
	})

	b.Run("MapAllocation_Small", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			m := make(map[string]string)
			m["From"] = "sender@example.com"
			m["To"] = "recipient@example.com"
			m["Subject"] = "Test"
			_ = m
		}
	})

	b.Run("MapAllocation_Sized", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			m := make(map[string]string, 10)
			m["From"] = "sender@example.com"
			m["To"] = "recipient@example.com"
			m["Subject"] = "Test"
			_ = m
		}
	})
}

// BenchmarkThroughput measures overall throughput.
func BenchmarkThroughput(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 100000)

	filter := NewSMTPFilter("127.0.0.1:0", "127.0.0.1:10026", emailChan, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := filter.Start(ctx); err != nil {
		b.Fatalf("Failed to start filter: %v", err)
	}
	defer filter.Stop()

	// Fast drain
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for email := range emailChan {
			repository.PutEmail(email)
		}
	}()

	addr := filter.listener.Addr().String()

	b.Run("Throughput_1000Connections", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		start := time.Now()

		var connWg sync.WaitGroup
		for i := 0; i < 1000; i++ {
			connWg.Add(1)
			go func() {
				defer connWg.Done()

				conn, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				defer conn.Close()

				reader := bufio.NewReader(conn)
				writer := bufio.NewWriter(conn)

				reader.ReadString('\n')

				writer.WriteString("EHLO test.local\r\n")
				writer.Flush()
				for {
					line, _ := reader.ReadString('\n')
					if len(line) > 4 && line[3] == ' ' {
						break
					}
				}

				writer.WriteString("QUIT\r\n")
				writer.Flush()
				reader.ReadString('\n')
			}()
		}

		connWg.Wait()

		elapsed := time.Since(start)
		b.ReportMetric(float64(1000)/elapsed.Seconds(), "conn/sec")
	})

	close(emailChan)
	wg.Wait()
}

// BenchmarkStats benchmarks atomic statistics operations.
func BenchmarkStats(b *testing.B) {
	logger := zerolog.Nop()
	emailChan := make(chan *repository.Email, 10)
	filter := NewSMTPFilter("127.0.0.1:10025", "127.0.0.1:10026", emailChan, logger)

	b.Run("AtomicIncrement", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				filter.received.Add(1)
			}
		})
	})

	b.Run("AtomicLoad", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = filter.received.Load()
			}
		})
	})

	b.Run("Stats_Combined", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			_, _, _ = filter.Stats()
		}
	})
}
