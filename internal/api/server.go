package api

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
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
	TLSCert       string // both set = HTTPS only
	TLSKey        string
	AllowedIPs    []string // source IPs/CIDRs; loopback always allowed; empty = any
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
	tlsCert       string
	tlsKey        string
	allowed       []*net.IPNet
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
		tlsCert:       cfg.TLSCert,
		tlsKey:        cfg.TLSKey,
		allowed:       parseAllowedIPs(cfg.AllowedIPs),
	}

	s.setupRoutes()

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      s.applyMiddleware(s.router),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	if s.tlsCert != "" {
		s.httpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return s
}

// parseAllowedIPs turns "1.2.3.4" / "1.2.3.0/24" entries into networks (invalid ones are
// rejected earlier by config validation and skipped here).
func parseAllowedIPs(list []string) []*net.IPNet {
	var out []*net.IPNet
	for _, a := range list {
		if _, n, err := net.ParseCIDR(a); err == nil {
			out = append(out, n)
			continue
		}
		if ip := net.ParseIP(a); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	s.logger.Info().
		Str("addr", s.httpServer.Addr).
		Str("version", s.version).
		Bool("tls", s.tlsCert != "").
		Int("allowed_networks", len(s.allowed)).
		Msg("Starting API server")

	var err error
	if s.tlsCert != "" {
		err = s.httpServer.ListenAndServeTLS(s.tlsCert, s.tlsKey)
	} else {
		err = s.httpServer.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
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
	s.router.HandleFunc("/api/queue", s.handleQueueStats)             // GET - queue stats
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
	// Order: source IP allow-list -> Auth -> Recovery -> Logging -> Handler
	// (no CORS: the API is called server-to-server by Mail Gateway, never from a browser)
	handler = s.loggingMiddleware(handler)
	handler = s.recoveryMiddleware(handler)
	handler = s.authMiddleware(handler)
	handler = s.allowListMiddleware(handler)
	return handler
}
