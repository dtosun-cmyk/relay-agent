package filter

import (
	"errors"
	"net"
	"net/smtp"
	"regexp"
	"sync"
	"time"
)

var (
	ErrPoolClosed    = errors.New("connection pool is closed")
	ErrNoConnections = errors.New("no available connections")
	ErrConnFailed    = errors.New("connection failed")
)

// SMTPConnectionPool manages a pool of SMTP connections to the next hop.
// It provides connection reuse for improved performance under high load.
type SMTPConnectionPool struct {
	mu sync.Mutex

	// Configuration
	addr    string
	maxSize int
	maxIdle time.Duration

	// Connection pool
	pool    chan *pooledConn
	created int
	closed  bool

	// Stats
	reused   int64
	newConns int64
	errors   int64
}

// pooledConn wraps an smtp.Client with metadata for pool management.
type pooledConn struct {
	client   *smtp.Client
	created  time.Time
	lastUsed time.Time
}

// NewSMTPConnectionPool creates a new connection pool for the given address.
func NewSMTPConnectionPool(addr string, maxSize int, maxIdle time.Duration) *SMTPConnectionPool {
	if maxSize <= 0 {
		maxSize = 5
	}
	if maxIdle <= 0 {
		maxIdle = 2 * time.Minute
	}

	return &SMTPConnectionPool{
		addr:    addr,
		maxSize: maxSize,
		maxIdle: maxIdle,
		pool:    make(chan *pooledConn, maxSize),
	}
}

// Get retrieves a connection from the pool or creates a new one.
func (p *SMTPConnectionPool) Get() (*smtp.Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	p.mu.Unlock()

	// Try to get a pooled connection
	for {
		select {
		case conn := <-p.pool:
			// Check if connection is still valid
			if conn.client != nil && time.Since(conn.lastUsed) < p.maxIdle {
				// Test connection health with NOOP
				if err := conn.client.Noop(); err == nil {
					p.mu.Lock()
					p.reused++
					p.mu.Unlock()
					return conn.client, nil
				}
				// Connection is dead, close it
				conn.client.Close()
			}
			p.mu.Lock()
			p.created--
			p.mu.Unlock()
			continue
		default:
			// No pooled connections available, create new one
			return p.createConn()
		}
	}
}

// Put returns a connection to the pool for reuse.
func (p *SMTPConnectionPool) Put(client *smtp.Client) {
	if client == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		client.Close()
		return
	}
	p.mu.Unlock()

	conn := &pooledConn{
		client:   client,
		lastUsed: time.Now(),
	}

	select {
	case p.pool <- conn:
		// Connection returned to pool
	default:
		// Pool is full, close the connection
		client.Close()
		p.mu.Lock()
		p.created--
		p.mu.Unlock()
	}
}

// Close closes all pooled connections and marks the pool as closed.
func (p *SMTPConnectionPool) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	// Close all pooled connections
	close(p.pool)
	for conn := range p.pool {
		if conn.client != nil {
			conn.client.Close()
		}
	}
}

// createConn creates a new SMTP connection.
func (p *SMTPConnectionPool) createConn() (*smtp.Client, error) {
	p.mu.Lock()
	if p.created >= p.maxSize {
		p.mu.Unlock()
		// Pool is at max capacity, wait for a connection
		select {
		case conn := <-p.pool:
			if conn.client != nil && time.Since(conn.lastUsed) < p.maxIdle {
				if err := conn.client.Noop(); err == nil {
					p.mu.Lock()
					p.reused++
					p.mu.Unlock()
					return conn.client, nil
				}
				conn.client.Close()
			}
			p.mu.Lock()
			p.created--
			p.mu.Unlock()
			return p.createConn()
		case <-time.After(5 * time.Second):
			return nil, ErrNoConnections
		}
	}
	p.created++
	p.newConns++
	p.mu.Unlock()

	// Set dial timeout
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
	}

	// Connect to the next hop
	conn, err := dialer.Dial("tcp", p.addr)
	if err != nil {
		p.mu.Lock()
		p.created--
		p.errors++
		p.mu.Unlock()
		return nil, err
	}

	// Create SMTP client
	client, err := smtp.NewClient(conn, p.addr)
	if err != nil {
		conn.Close()
		p.mu.Lock()
		p.created--
		p.errors++
		p.mu.Unlock()
		return nil, err
	}

	return client, nil
}

// Stats returns pool statistics.
func (p *SMTPConnectionPool) Stats() (reused, newConns, errors int64, current int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reused, p.newConns, p.errors, len(p.pool)
}

// queuedAsRe reads Postfix's queue ID from the end-of-data reply ("250 2.0.0 Ok: queued as 9521F3A17CC").
var queuedAsRe = regexp.MustCompile(`queued as ([0-9A-Za-z]+)`)

// QueueIDFromReply returns the queue ID of a Postfix end-of-data reply ("" when the reply has none).
func QueueIDFromReply(msg string) string {
	if m := queuedAsRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}

// SendEmail sends an email using a pooled connection and returns the next hop's end-of-data reply text.
// The reply carries the queue ID Postfix gave the re-injected message — the ID its delivery log line uses,
// so the delivery result is attached to the record exactly (v1.0.6; net/smtp's Data().Close() drops the text).
func (p *SMTPConnectionPool) SendEmail(from string, recipients []string, data []byte) (string, error) {
	client, err := p.Get()
	if err != nil {
		return "", err
	}

	// Always try to return connection to pool
	var sendErr error
	defer func() {
		if sendErr != nil {
			// Connection might be broken, close it
			client.Close()
			p.mu.Lock()
			p.created--
			p.mu.Unlock()
		} else {
			// Reset session for reuse
			if resetErr := client.Reset(); resetErr != nil {
				client.Close()
				p.mu.Lock()
				p.created--
				p.mu.Unlock()
			} else {
				p.Put(client)
			}
		}
	}()

	// Send MAIL FROM
	if err := client.Mail(from); err != nil {
		sendErr = err
		return "", err
	}

	// Send RCPT TO for each recipient
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			sendErr = err
			return "", err
		}
	}

	// DATA with the reply text kept (same steps as net/smtp Client.Data + dataCloser.Close)
	id, err := client.Text.Cmd("DATA")
	if err != nil {
		sendErr = err
		return "", err
	}
	client.Text.StartResponse(id)
	_, _, err = client.Text.ReadResponse(354)
	client.Text.EndResponse(id)
	if err != nil {
		sendErr = err
		return "", err
	}
	wc := client.Text.DotWriter()
	if _, err := wc.Write(data); err != nil {
		wc.Close()
		sendErr = err
		return "", err
	}
	if err := wc.Close(); err != nil {
		sendErr = err
		return "", err
	}
	_, reply, err := client.Text.ReadResponse(250)
	if err != nil {
		sendErr = err
		return "", err
	}
	return reply, nil
}
