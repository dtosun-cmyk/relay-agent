package filter

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"relay-agent/internal/repository"
)

// fakeNextHop answers like Postfix on the re-injection port; the end-of-data reply carries the queue ID.
func fakeNextHop(t *testing.T, endOfData string) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				w := bufio.NewWriter(c)
				r := bufio.NewReader(c)
				say := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
				say("220 relay.test ESMTP Postfix")
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					if inData {
						if line == "." {
							inData = false
							say(endOfData)
						}
						continue
					}
					got <- line
					switch cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0]); cmd {
					case "EHLO", "HELO":
						say("250 relay.test")
					case "MAIL", "RCPT", "RSET", "NOOP":
						say("250 2.0.0 Ok")
					case "DATA":
						inData = true
						say("354 End data with <CR><LF>.<CR><LF>")
					case "QUIT":
						say("221 2.0.0 Bye")
						return
					default:
						say("502 5.5.2 Error")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), got
}

func TestQueueIDFromReply(t *testing.T) {
	for in, want := range map[string]string{
		"2.0.0 Ok: queued as 9521F3A17CC":    "9521F3A17CC",
		"2.0.0 Ok: queued as 4cXyz1Ab2Lz9kq": "4cXyz1Ab2Lz9kq", // long queue IDs
		"Ok":                                 "",
		"2.0.0 Ok: queued as ":               "",
	} {
		if got := QueueIDFromReply(in); got != want {
			t.Errorf("QueueIDFromReply(%q) = %q, want %q", in, got, want)
		}
	}
}

// The pool returns the end-of-data reply, the filter attaches its queue ID to every recipient record,
// and the pooled connection is reused for the next message.
func TestForwardKeepsRelayQueueID(t *testing.T) {
	addr, cmds := fakeNextHop(t, "250 2.0.0 Ok: queued as 9521F3A17CC")
	emailChan := make(chan *repository.Email, 10)
	f := NewSMTPFilter("127.0.0.1:0", addr, emailChan, zerolog.Nop())
	msg := &EmailMessage{From: "sender@example.com", Recipients: []string{"a@icloud.com", "b@icloud.com"},
		QueueID: "MGW-20261016-174632-578366", Data: []byte("Subject: t\r\n\r\nbody\r\n"), ReceivedAt: time.Now()}
	qid, err := f.forwardEmail(msg)
	if err != nil || qid != "9521F3A17CC" {
		t.Fatalf("forwardEmail = %q, %v", qid, err)
	}
	f.processEmail(msg, qid)
	for i := 0; i < 2; i++ {
		select {
		case e := <-emailChan:
			if e.QueueID != "9521F3A17CC" || e.MailgatewayQueueID != msg.QueueID || e.Status != "received" {
				t.Errorf("record %d: queue %q mgw %q status %q", i, e.QueueID, e.MailgatewayQueueID, e.Status)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("record missing")
		}
	}
	if qid2, err := f.forwardEmail(msg); err != nil || qid2 != "9521F3A17CC" {
		t.Fatalf("second message on the pooled connection: %q, %v", qid2, err)
	}
	seen := []string{}
	for len(cmds) > 0 {
		seen = append(seen, <-cmds)
	}
	joined := strings.Join(seen, "|")
	if strings.Count(joined, "DATA") != 2 || strings.Count(joined, "RSET") < 1 {
		t.Fatalf("unexpected SMTP dialogue: %s", joined)
	}
}

// A next hop that rejects the data: no queue ID, an error, the connection is dropped from the pool.
func TestForwardRejectedData(t *testing.T) {
	addr, _ := fakeNextHop(t, "451 4.3.0 Error: queue file write error")
	f := NewSMTPFilter("127.0.0.1:0", addr, make(chan *repository.Email, 1), zerolog.Nop())
	qid, err := f.forwardEmail(&EmailMessage{From: "s@x.example", Recipients: []string{"r@y.example"}, Data: []byte("x\r\n")})
	if err == nil || qid != "" {
		t.Fatalf("want an error without queue ID, got %q, %v", qid, err)
	}
}
