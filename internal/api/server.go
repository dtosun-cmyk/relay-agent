package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"relay-agent/internal/postfix"
	"relay-agent/internal/repository"
	"relay-agent/internal/smtp"
	"relay-agent/internal/stats"
)

// ServerConfig holds the HTTP server configuration.
type ServerConfig struct {
	Host          string
	Port          int
	Version       string
	SMTPDomain    string
	SMTPAPISecret string
}

// Server represents the HTTP API server.
type Server struct {
	httpServer    *http.Server
	router        *http.ServeMux
	repo          *repository.MongoRepository
	stats         *stats.StatsCollector
	smtpManager   *smtp.UserManager
	queueManager  *postfix.QueueManager
	smtpAPISecret string
	logger        zerolog.Logger
	startTime     time.Time
	version       string
}

// NewServer creates a new API server instance.
func NewServer(cfg ServerConfig, repo *repository.MongoRepository, stats *stats.StatsCollector, logger zerolog.Logger) *Server {
	s := &Server{
		router:        http.NewServeMux(),
		repo:          repo,
		stats:         stats,
		smtpManager:   smtp.NewUserManager(cfg.SMTPDomain, logger),
		queueManager:  postfix.NewQueueManager(logger),
		smtpAPISecret: cfg.SMTPAPISecret,
		logger:        logger,
		startTime:     time.Now(),
		version:       cfg.Version,
	}

	s.setupRoutes()

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      s.applyMiddleware(s.router),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	s.logger.Info().
		Str("addr", s.httpServer.Addr).
		Str("version", s.version).
		Msg("Starting API server")

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info().Msg("Shutting down API server")

	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	s.logger.Info().Msg("API server stopped")
	return nil
}

// setupRoutes configures the HTTP routes.
func (s *Server) setupRoutes() {
	// Health check endpoints (no auth required)
	s.router.HandleFunc("/health", s.handleHealth)
	s.router.HandleFunc("/api/health", s.handleHealth)

	// Protected API endpoints (require X-API-Secret header)
	s.router.HandleFunc("/api/stats", s.handleStats)
	s.router.HandleFunc("/api/logs", s.handleLogs)

	// Queue endpoints (Postfix queue management)
	s.router.HandleFunc("/api/queue", s.handleQueueStats)           // GET - queue stats
	s.router.HandleFunc("/api/queue/messages", s.handleQueueMessages) // GET - list, DELETE - delete all
	s.router.HandleFunc("/api/queue/messages/", s.handleQueueMessage) // DELETE/{id}, POST/{id}/requeue|hold|release
	s.router.HandleFunc("/api/queue/flush", s.handleQueueFlush)       // POST - flush queue

	// SMTP user management
	s.router.HandleFunc("/api/smtp-users", s.handleSMTPUsers)
	s.router.HandleFunc("/api/smtp-users/", s.handleSMTPUsers)
}

// applyMiddleware wraps the handler with all middleware in order.
func (s *Server) applyMiddleware(handler http.Handler) http.Handler {
	// Middleware is applied in reverse order (last applied = first executed)
	// Order: CORS -> Auth -> Recovery -> Logging -> Handler
	handler = s.loggingMiddleware(handler)
	handler = s.recoveryMiddleware(handler)
	handler = s.authMiddleware(handler)
	handler = s.corsMiddleware(handler)
	return handler
}
