package repository

import (
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Email represents the main model for MongoDB storage.
// Fields are ordered by size (largest to smallest) for optimal memory alignment
// and CPU cache efficiency.
type Email struct {
	// 24 bytes - ObjectID (12 bytes actual, but aligned to pointer size)
	ID primitive.ObjectID `bson:"_id,omitempty"`

	// 8-byte aligned fields (time.Time, int64, pointers)
	ReceivedAt     time.Time `bson:"received_at"`
	DeliveredAt    time.Time `bson:"delivered_at"`
	WebhookSentAt  time.Time `bson:"webhook_sent_at,omitempty"`
	CreatedAt      time.Time `bson:"created_at"`
	Size           int64     `bson:"size"`
	DeliveryTimeMs int64     `bson:"delivery_time_ms"`

	// String fields (16 bytes each on 64-bit systems - pointer + length)
	QueueID            string `bson:"queue_id"`
	MailgatewayQueueID string `bson:"mailgateway_queue_id"`
	Sender             string `bson:"sender"`
	Recipient          string `bson:"recipient"`
	RecipientDomain    string `bson:"recipient_domain"`
	Provider           string `bson:"provider"`
	Status             string `bson:"status"`
	DSN                string `bson:"dsn"`
	StatusMessage      string `bson:"status_message"`
	ClientHost         string `bson:"client_host"`  // Mailgateway hostname (source)
	ClientIP           string `bson:"client_ip"`    // Mailgateway IP (source)
	RelayHost          string `bson:"relay_host"`   // Destination relay hostname
	RelayIP            string `bson:"relay_ip"`     // Destination relay IP

	// 4-byte aligned fields
	Attempts int `bson:"attempts"`

	// 1-byte aligned fields (booleans)
	WebhookSent bool `bson:"webhook_sent"`
}

// Reset clears the Email struct for reuse, minimizing GC pressure.
// This method is called when returning objects to the pool.
func (e *Email) Reset() {
	e.ID = primitive.NilObjectID
	e.QueueID = ""
	e.MailgatewayQueueID = ""
	e.Sender = ""
	e.Recipient = ""
	e.RecipientDomain = ""
	e.Provider = ""
	e.Size = 0
	e.Status = ""
	e.DSN = ""
	e.StatusMessage = ""
	e.ClientHost = ""
	e.ClientIP = ""
	e.RelayHost = ""
	e.RelayIP = ""
	e.ReceivedAt = time.Time{}
	e.DeliveredAt = time.Time{}
	e.DeliveryTimeMs = 0
	e.Attempts = 0
	e.WebhookSent = false
	e.WebhookSentAt = time.Time{}
	e.CreatedAt = time.Time{}
}

// LogEntry represents an intermediate parsing state from log files.
// Optimized for zero-allocation parsing with field ordering for cache efficiency.
type LogEntry struct {
	// 8-byte aligned fields
	Timestamp      time.Time `json:"timestamp"`
	ReceivedAt     time.Time `json:"received_at,omitempty"`
	DeliveredAt    time.Time `json:"delivered_at,omitempty"`
	Size           int64     `json:"size"`
	DeliveryTimeMs int64     `json:"delivery_time_ms,omitempty"`

	// String fields
	QueueID            string `json:"queue_id"`
	MailgatewayQueueID string `json:"mailgateway_queue_id,omitempty"`
	Sender             string `json:"sender"`
	Recipient          string `json:"recipient"`
	RecipientDomain    string `json:"recipient_domain"`
	Provider           string `json:"provider,omitempty"`
	Status             string `json:"status"`
	DSN                string `json:"dsn,omitempty"`
	StatusMessage      string `json:"status_message,omitempty"`
	ClientHost         string `json:"client_host,omitempty"`
	ClientIP           string `json:"client_ip,omitempty"`
	RelayHost          string `json:"relay_host,omitempty"`
	RelayIP            string `json:"relay_ip,omitempty"`
	Action             string `json:"action"`
	LogType            string `json:"log_type"`

	// 4-byte aligned fields
	Attempts int `json:"attempts,omitempty"`
}

// Reset clears the LogEntry struct for reuse.
func (l *LogEntry) Reset() {
	l.Timestamp = time.Time{}
	l.ReceivedAt = time.Time{}
	l.DeliveredAt = time.Time{}
	l.Size = 0
	l.DeliveryTimeMs = 0
	l.QueueID = ""
	l.MailgatewayQueueID = ""
	l.Sender = ""
	l.Recipient = ""
	l.RecipientDomain = ""
	l.Provider = ""
	l.Status = ""
	l.DSN = ""
	l.StatusMessage = ""
	l.ClientHost = ""
	l.ClientIP = ""
	l.RelayHost = ""
	l.RelayIP = ""
	l.Action = ""
	l.LogType = ""
	l.Attempts = 0
}

// DayStats represents daily email delivery statistics.
type DayStats struct {
	// String fields
	Date string `bson:"_id" json:"date"`

	// 8-byte aligned fields
	TotalSize int64 `bson:"total_size" json:"total_size"`

	// 4-byte aligned fields
	Total    int `bson:"total" json:"total"`
	Sent     int `bson:"sent" json:"sent"`
	Deferred int `bson:"deferred" json:"deferred"`
	Bounced  int `bson:"bounced" json:"bounced"`
	Rejected int `bson:"rejected" json:"rejected"`
}

// HourlyStats represents hourly email delivery statistics.
type HourlyStats struct {
	// 8-byte aligned fields
	Date          time.Time `bson:"date" json:"date"`
	UpdatedAt     time.Time `bson:"updated_at" json:"updated_at"`
	AvgDeliveryMs float64   `bson:"avg_delivery_ms" json:"avg_delivery_ms"`

	// 4-byte aligned fields
	Hour     int   `bson:"hour" json:"hour"`
	Total    int64 `bson:"total" json:"total"`
	Sent     int64 `bson:"sent" json:"sent"`
	Deferred int64 `bson:"deferred" json:"deferred"`
	Bounced  int64 `bson:"bounced" json:"bounced"`
}

// ProviderStats represents statistics per email provider.
type ProviderStats struct {
	// String fields
	Provider string `bson:"_id" json:"provider"`

	// 8-byte aligned fields
	TotalSize       int64   `bson:"total_size" json:"total_size"`
	AvgDeliveryTime float64 `bson:"avg_delivery_time" json:"avg_delivery_time"`

	// 4-byte aligned fields
	Total     int `bson:"total" json:"total"`
	Delivered int `bson:"delivered" json:"delivered"`
	Deferred  int `bson:"deferred" json:"deferred"`
	Bounced   int `bson:"bounced" json:"bounced"`
	Rejected  int `bson:"rejected" json:"rejected"`
}

// LogFilter represents query parameters for filtering log entries.
// Used for API queries and search operations.
type LogFilter struct {
	// 8-byte aligned fields
	StartDate time.Time `json:"start_date,omitempty"`
	EndDate   time.Time `json:"end_date,omitempty"`

	// String fields
	QueueID            string `json:"queue_id,omitempty"`
	MailgatewayQueueID string `json:"mailgateway_queue_id,omitempty"`
	Sender             string `json:"sender,omitempty"`
	Recipient          string `json:"recipient,omitempty"`
	Domain             string `json:"domain,omitempty"` // Simplified from RecipientDomain
	Provider           string `json:"provider,omitempty"`
	Status             string `json:"status,omitempty"`
	RelayHost          string `json:"relay_host,omitempty"`

	// 4-byte aligned fields
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// EmailPool provides zero-allocation Email object pooling.
// Use GetEmail() to acquire and PutEmail() to release.
var EmailPool = sync.Pool{
	New: func() interface{} {
		return &Email{}
	},
}

// LogEntryPool provides zero-allocation LogEntry object pooling.
// Use GetLogEntry() to acquire and PutLogEntry() to release.
var LogEntryPool = sync.Pool{
	New: func() interface{} {
		return &LogEntry{}
	},
}

// GetEmail acquires an Email from the pool.
// The returned Email is in a reset state.
func GetEmail() *Email {
	e := EmailPool.Get().(*Email)
	e.Reset()
	return e
}

// PutEmail returns an Email to the pool.
// The Email should not be used after calling this function.
// Note: Reset is deferred to GetEmail to avoid double-reset overhead.
func PutEmail(e *Email) {
	if e != nil {
		EmailPool.Put(e)
	}
}

// GetLogEntry acquires a LogEntry from the pool.
// The returned LogEntry is in a reset state.
func GetLogEntry() *LogEntry {
	l := LogEntryPool.Get().(*LogEntry)
	l.Reset()
	return l
}

// PutLogEntry returns a LogEntry to the pool.
// The LogEntry should not be used after calling this function.
// Note: Reset is deferred to GetLogEntry to avoid double-reset overhead.
func PutLogEntry(l *LogEntry) {
	if l != nil {
		LogEntryPool.Put(l)
	}
}
