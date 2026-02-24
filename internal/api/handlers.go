package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"relay-agent/internal/postfix"
	"relay-agent/internal/repository"
	"relay-agent/internal/smtp"
)

// handleHealth returns the health status of the service.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Check MongoDB connection
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	mongoStatus := "connected"
	if err := s.repo.Ping(ctx); err != nil {
		mongoStatus = "disconnected"
		s.logger.Error().Err(err).Msg("MongoDB health check failed")
	}

	response := map[string]interface{}{
		"status":         "healthy",
		"uptime_seconds": int(time.Since(s.startTime).Seconds()),
		"version":        s.version,
		"mongodb":        mongoStatus,
	}

	writeJSON(w, http.StatusOK, response)
}

// handleStats returns delivery statistics.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Get today's stats from stats collector (in-memory)
	todayStats := s.stats.GetTodayStats()
	providerStats := s.stats.GetProviderStats()

	response := map[string]interface{}{
		"today": map[string]interface{}{
			"total":           todayStats.Total,
			"sent":            todayStats.Sent,
			"deferred":        todayStats.Deferred,
			"bounced":         todayStats.Bounced,
			"avg_delivery_ms": todayStats.AvgDeliveryMs,
		},
		"by_provider": providerStats,
	}

	writeJSON(w, http.StatusOK, response)
}

// handleLogs returns delivery logs with filtering and pagination.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// Parse query parameters
	query := r.URL.Query()

	// Parse pagination parameters
	limit := 50
	if limitStr := query.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 1000 {
			limit = l
		}
	}

	offset := 0
	if offsetStr := query.Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	// Parse filter parameters
	filter := repository.LogFilter{
		Status:    query.Get("status"),
		Recipient: query.Get("recipient"),
		Sender:    query.Get("sender"),
		Provider:  query.Get("provider"),
	}

	// Parse date range
	if startDateStr := query.Get("start_date"); startDateStr != "" {
		if t, err := time.Parse("2006-01-02", startDateStr); err == nil {
			filter.StartDate = t
		}
	}

	if endDateStr := query.Get("end_date"); endDateStr != "" {
		if t, err := time.Parse("2006-01-02", endDateStr); err == nil {
			// Set to end of day
			filter.EndDate = t.Add(24*time.Hour - time.Second)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Fetch logs
	logs, total, err := s.repo.GetLogs(ctx, filter, limit, offset)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to fetch logs")
		writeError(w, http.StatusInternalServerError, "Failed to fetch logs")
		return
	}

	response := map[string]interface{}{
		"logs":   logs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	}

	writeJSON(w, http.StatusOK, response)
}


// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		// If encoding fails, we can't write another response
		// Just log the error
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// writeError writes a JSON error response with the given status code.
func writeError(w http.ResponseWriter, status int, message string) {
	response := map[string]interface{}{
		"error":  message,
		"status": status,
	}
	writeJSON(w, status, response)
}

// handleSMTPUsers handles SMTP user management endpoints
// GET    /api/smtp-users - List all users
// POST   /api/smtp-users - Create a new user
// DELETE /api/smtp-users/{username} - Delete a user
// Note: Authentication is handled by authMiddleware
func (s *Server) handleSMTPUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListSMTPUsers(w, r)
	case http.MethodPost:
		s.handleCreateSMTPUser(w, r)
	case http.MethodDelete:
		s.handleDeleteSMTPUser(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleListSMTPUsers lists all SMTP users
func (s *Server) handleListSMTPUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.smtpManager.ListUsers()
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to list SMTP users")
		writeError(w, http.StatusInternalServerError, "Failed to list users")
		return
	}

	response := map[string]interface{}{
		"users": users,
		"count": len(users),
	}

	writeJSON(w, http.StatusOK, response)
}

// handleCreateSMTPUser creates a new SMTP user
func (s *Server) handleCreateSMTPUser(w http.ResponseWriter, r *http.Request) {
	var req smtp.CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Validate request
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "Username is required")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "Password is required")
		return
	}

	// Create user
	if err := s.smtpManager.CreateUser(req.Username, req.Password); err != nil {
		if err == smtp.ErrInvalidUsername {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err == smtp.ErrInvalidPassword {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err == smtp.ErrUserExists {
			writeError(w, http.StatusConflict, "User already exists")
			return
		}

		s.logger.Error().
			Err(err).
			Str("username", req.Username).
			Msg("Failed to create SMTP user")
		writeError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}

	s.logger.Info().
		Str("username", req.Username).
		Str("remote_addr", r.RemoteAddr).
		Msg("SMTP user created via API")

	response := map[string]interface{}{
		"message":  "User created successfully",
		"username": req.Username,
	}

	writeJSON(w, http.StatusCreated, response)
}

// handleDeleteSMTPUser deletes an SMTP user
func (s *Server) handleDeleteSMTPUser(w http.ResponseWriter, r *http.Request) {
	// Extract username from path: /api/smtp-users/{username}
	path := strings.TrimPrefix(r.URL.Path, "/api/smtp-users/")
	username := strings.TrimSpace(path)

	if username == "" {
		writeError(w, http.StatusBadRequest, "Username is required")
		return
	}

	// Delete user
	if err := s.smtpManager.DeleteUser(username); err != nil {
		if err == smtp.ErrInvalidUsername {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err == smtp.ErrUserNotFound {
			writeError(w, http.StatusNotFound, "User not found")
			return
		}

		s.logger.Error().
			Err(err).
			Str("username", username).
			Msg("Failed to delete SMTP user")
		writeError(w, http.StatusInternalServerError, "Failed to delete user")
		return
	}

	s.logger.Info().
		Str("username", username).
		Str("remote_addr", r.RemoteAddr).
		Msg("SMTP user deleted via API")

	response := map[string]interface{}{
		"message":  "User deleted successfully",
		"username": username,
	}

	writeJSON(w, http.StatusOK, response)
}

// ============================================
// Queue Management Handlers
// ============================================

// handleQueueMessages handles queue message operations
// GET    /api/queue/messages - List messages with pagination
// DELETE /api/queue/messages - Delete all messages
func (s *Server) handleQueueMessages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListQueueMessages(w, r)
	case http.MethodDelete:
		s.handleDeleteAllQueueMessages(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleQueueMessage handles single message operations
// DELETE /api/queue/messages/{queue_id} - Delete a message
// POST   /api/queue/messages/{queue_id}/requeue - Requeue a message
// POST   /api/queue/messages/{queue_id}/hold - Put message on hold
// POST   /api/queue/messages/{queue_id}/release - Release message from hold
func (s *Server) handleQueueMessage(w http.ResponseWriter, r *http.Request) {
	// Extract queue_id and action from path
	// Path: /api/queue/messages/{queue_id} or /api/queue/messages/{queue_id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/queue/messages/")
	parts := strings.Split(path, "/")

	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "Queue ID is required")
		return
	}

	queueID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodDelete:
		// DELETE /api/queue/messages/{queue_id}
		if err := s.queueManager.DeleteMessage(ctx, queueID); err != nil {
			s.logger.Error().Err(err).Str("queue_id", queueID).Msg("Failed to delete queue message")
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"message":  "Message deleted successfully",
			"queue_id": queueID,
		})

	case http.MethodPost:
		// POST /api/queue/messages/{queue_id}/{action}
		switch action {
		case "requeue":
			if err := s.queueManager.RequeueMessage(ctx, queueID); err != nil {
				s.logger.Error().Err(err).Str("queue_id", queueID).Msg("Failed to requeue message")
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"message":  "Message requeued successfully",
				"queue_id": queueID,
			})

		case "hold":
			if err := s.queueManager.HoldMessage(ctx, queueID); err != nil {
				s.logger.Error().Err(err).Str("queue_id", queueID).Msg("Failed to hold message")
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"message":  "Message put on hold",
				"queue_id": queueID,
			})

		case "release":
			if err := s.queueManager.ReleaseMessage(ctx, queueID); err != nil {
				s.logger.Error().Err(err).Str("queue_id", queueID).Msg("Failed to release message")
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"message":  "Message released from hold",
				"queue_id": queueID,
			})

		default:
			writeError(w, http.StatusBadRequest, "Invalid action. Use: requeue, hold, or release")
		}

	default:
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// handleListQueueMessages lists all messages in the queue with pagination
func (s *Server) handleListQueueMessages(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// Parse pagination
	limit := 50
	if limitStr := query.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 500 {
			limit = l
		}
	}

	offset := 0
	if offsetStr := query.Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	messages, total, err := s.queueManager.ListMessages(ctx, limit, offset)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to list queue messages")
		writeError(w, http.StatusInternalServerError, "Failed to list queue messages")
		return
	}

	response := map[string]interface{}{
		"messages": messages,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	}

	writeJSON(w, http.StatusOK, response)
}

// handleDeleteAllQueueMessages deletes all messages from the queue
func (s *Server) handleDeleteAllQueueMessages(w http.ResponseWriter, r *http.Request) {
	// Require confirmation parameter for safety
	if r.URL.Query().Get("confirm") != "yes" {
		writeError(w, http.StatusBadRequest, "Add ?confirm=yes to confirm deletion of all messages")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	count, err := s.queueManager.DeleteAll(ctx)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to delete all queue messages")
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Warn().
		Int("count", count).
		Str("remote_addr", r.RemoteAddr).
		Msg("All queue messages deleted via API")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "All messages deleted",
		"count":   count,
	})
}

// handleQueueFlush triggers immediate delivery of all queued messages
func (s *Server) handleQueueFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.queueManager.FlushQueue(ctx); err != nil {
		s.logger.Error().Err(err).Msg("Failed to flush queue")
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info().
		Str("remote_addr", r.RemoteAddr).
		Msg("Queue flushed via API")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Queue flush initiated",
	})
}

// handleQueueStats returns queue statistics (updated version using postfix)
func (s *Server) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	stats, err := s.queueManager.GetStats(ctx)
	if err != nil {
		s.logger.Error().Err(err).Msg("Failed to get queue stats")
		writeError(w, http.StatusInternalServerError, "Failed to get queue statistics")
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

// Ensure postfix.QueueManager is used (compile check)
var _ = (*postfix.QueueManager)(nil)
