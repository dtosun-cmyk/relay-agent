package postfix

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// QueueMessage represents a message in the Postfix queue
type QueueMessage struct {
	QueueID      string    `json:"queue_id"`
	QueueName    string    `json:"queue_name"`    // active, deferred, hold, incoming
	Sender       string    `json:"sender"`
	Recipients   []string  `json:"recipients"`
	ArrivalTime  time.Time `json:"arrival_time"`
	MessageSize  int64     `json:"message_size"`
	Reason       string    `json:"reason,omitempty"` // For deferred messages
}

// QueueStats represents queue statistics
type QueueStats struct {
	Active   int `json:"active"`
	Deferred int `json:"deferred"`
	Hold     int `json:"hold"`
	Incoming int `json:"incoming"`
	Total    int `json:"total"`
}

// QueueManager handles Postfix queue operations
type QueueManager struct {
	logger zerolog.Logger
	mu     sync.Mutex // Protects concurrent queue operations
}

// NewQueueManager creates a new queue manager
func NewQueueManager(logger zerolog.Logger) *QueueManager {
	return &QueueManager{
		logger: logger.With().Str("component", "queue_manager").Logger(),
	}
}

// postqueueJSON represents the JSON output from postqueue -j
type postqueueJSON struct {
	QueueID          string   `json:"queue_id"`
	QueueName        string   `json:"queue_name"`
	ArrivalTime      int64    `json:"arrival_time"`
	MessageSize      int64    `json:"message_size"`
	ForcedExpire     bool     `json:"forced_expire"`
	Sender           string   `json:"sender"`
	Recipients       []recipient `json:"recipients"`
}

type recipient struct {
	Address      string `json:"address"`
	DelayReason  string `json:"delay_reason,omitempty"`
}

// ListMessages returns messages from the queue with pagination
// This uses postqueue -j for efficient JSON parsing
func (qm *QueueManager) ListMessages(ctx context.Context, limit, offset int) ([]QueueMessage, int, error) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	// Run postqueue -j to get JSON output
	cmd := exec.CommandContext(ctx, "postqueue", "-j")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("failed to start postqueue: %w", err)
	}

	var messages []QueueMessage
	total := 0
	scanner := bufio.NewScanner(stdout)

	// Increase buffer size for large messages
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var pq postqueueJSON
		if err := json.Unmarshal([]byte(line), &pq); err != nil {
			qm.logger.Warn().Err(err).Str("line", line[:min(100, len(line))]).Msg("Failed to parse queue entry")
			continue
		}

		total++

		// Apply pagination
		if total <= offset {
			continue
		}
		if limit > 0 && len(messages) >= limit {
			continue // Keep counting total but don't add more
		}

		// Convert to QueueMessage
		recipients := make([]string, len(pq.Recipients))
		var reason string
		for i, r := range pq.Recipients {
			recipients[i] = r.Address
			if r.DelayReason != "" && reason == "" {
				reason = r.DelayReason
			}
		}

		msg := QueueMessage{
			QueueID:      pq.QueueID,
			QueueName:    pq.QueueName,
			Sender:       pq.Sender,
			Recipients:   recipients,
			ArrivalTime:  time.Unix(pq.ArrivalTime, 0),
			MessageSize:  pq.MessageSize,
			Reason:       reason,
		}
		messages = append(messages, msg)
	}

	if err := cmd.Wait(); err != nil {
		// postqueue returns exit code 0 even when queue is empty
		// Only log as warning
		qm.logger.Debug().Err(err).Msg("postqueue command finished")
	}

	return messages, total, nil
}

// GetStats returns queue statistics
func (qm *QueueManager) GetStats(ctx context.Context) (*QueueStats, error) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	cmd := exec.CommandContext(ctx, "postqueue", "-j")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start postqueue: %w", err)
	}

	stats := &QueueStats{}
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var pq postqueueJSON
		if err := json.Unmarshal([]byte(line), &pq); err != nil {
			continue
		}

		switch pq.QueueName {
		case "active":
			stats.Active++
		case "deferred":
			stats.Deferred++
		case "hold":
			stats.Hold++
		case "incoming":
			stats.Incoming++
		}
		stats.Total++
	}

	cmd.Wait()
	return stats, nil
}

// DeleteMessage removes a specific message from the queue
func (qm *QueueManager) DeleteMessage(ctx context.Context, queueID string) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	// Validate queue ID format (alphanumeric, typically hex)
	if !isValidQueueID(queueID) {
		return fmt.Errorf("invalid queue ID format")
	}

	cmd := exec.CommandContext(ctx, "postsuper", "-d", queueID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete message: %s", string(output))
	}

	qm.logger.Info().Str("queue_id", queueID).Msg("Deleted message from queue")
	return nil
}

// DeleteAll removes all messages from the queue
func (qm *QueueManager) DeleteAll(ctx context.Context) (int, error) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	cmd := exec.CommandContext(ctx, "postsuper", "-d", "ALL")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to delete all messages: %s", string(output))
	}

	// Parse output to get count
	// postsuper output: "postsuper: Deleted: 123 messages"
	count := parseDeleteCount(string(output))

	qm.logger.Warn().Int("count", count).Msg("Deleted all messages from queue")
	return count, nil
}

// RequeueMessage requeues a specific message for immediate delivery
func (qm *QueueManager) RequeueMessage(ctx context.Context, queueID string) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if !isValidQueueID(queueID) {
		return fmt.Errorf("invalid queue ID format")
	}

	cmd := exec.CommandContext(ctx, "postsuper", "-r", queueID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to requeue message: %s", string(output))
	}

	qm.logger.Info().Str("queue_id", queueID).Msg("Requeued message")
	return nil
}

// FlushQueue triggers immediate delivery of all queued messages
func (qm *QueueManager) FlushQueue(ctx context.Context) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	cmd := exec.CommandContext(ctx, "postqueue", "-f")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to flush queue: %s", string(output))
	}

	qm.logger.Info().Msg("Flushed mail queue")
	return nil
}

// HoldMessage puts a message on hold
func (qm *QueueManager) HoldMessage(ctx context.Context, queueID string) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if !isValidQueueID(queueID) {
		return fmt.Errorf("invalid queue ID format")
	}

	cmd := exec.CommandContext(ctx, "postsuper", "-h", queueID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to hold message: %s", string(output))
	}

	qm.logger.Info().Str("queue_id", queueID).Msg("Put message on hold")
	return nil
}

// ReleaseMessage releases a message from hold
func (qm *QueueManager) ReleaseMessage(ctx context.Context, queueID string) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if !isValidQueueID(queueID) {
		return fmt.Errorf("invalid queue ID format")
	}

	cmd := exec.CommandContext(ctx, "postsuper", "-H", queueID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to release message: %s", string(output))
	}

	qm.logger.Info().Str("queue_id", queueID).Msg("Released message from hold")
	return nil
}

// Helper functions

func isValidQueueID(id string) bool {
	if len(id) == 0 || len(id) > 20 {
		return false
	}
	for _, c := range id {
		if !((c >= 'A' && c <= 'F') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func parseDeleteCount(output string) int {
	// Parse "postsuper: Deleted: 123 messages"
	if idx := strings.Index(output, "Deleted:"); idx >= 0 {
		var count int
		fmt.Sscanf(output[idx:], "Deleted: %d", &count)
		return count
	}
	return 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
