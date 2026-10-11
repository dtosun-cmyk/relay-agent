package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// loggingMiddleware logs HTTP requests with method, path, status, and duration.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a custom response writer to capture status code
		lrw := &loggingResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		// Call the next handler
		next.ServeHTTP(lrw, r)

		// Log the request
		duration := time.Since(start)
		s.logger.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Str("remote_addr", r.RemoteAddr).
			Int("status", lrw.statusCode).
			Dur("duration_ms", duration).
			Msg("HTTP request")
	})
}

// loggingResponseWriter wraps http.ResponseWriter to capture the status code.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code and calls the underlying WriteHeader.
func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// recoveryMiddleware recovers from panics and returns a 500 error.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Log the panic with stack trace
				s.logger.Error().
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Str("remote_addr", r.RemoteAddr).
					Interface("panic", err).
					Bytes("stack", debug.Stack()).
					Msg("Panic recovered")

				// Return 500 Internal Server Error
				writeError(w, http.StatusInternalServerError, "Internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// allowListMiddleware rejects requests whose source address is not loopback and not in
// server.allowed_ips (BUG-098 / v1.0.5). The TCP peer address is used, never forwarded headers.
func (s *Server) allowListMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.allowed) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			next.ServeHTTP(w, r)
			return
		}
		if ip != nil {
			for _, n := range s.allowed {
				if n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		s.logger.Warn().Str("remote_addr", r.RemoteAddr).Str("path", r.URL.Path).Msg("Source address not allowed")
		writeError(w, http.StatusForbidden, "Source address not allowed")
	})
}

// authMiddleware checks for valid API secret key on protected endpoints.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health endpoint
		if r.URL.Path == "/health" || r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Check X-API-Secret header
		apiSecret := r.Header.Get("X-API-Secret")
		if apiSecret == "" {
			s.logger.Warn().
				Str("path", r.URL.Path).
				Str("remote_addr", r.RemoteAddr).
				Msg("Missing API secret")
			writeError(w, http.StatusUnauthorized, "Missing X-API-Secret header")
			return
		}

		if subtle.ConstantTimeCompare([]byte(apiSecret), []byte(s.smtpAPISecret)) != 1 {
			s.logger.Warn().
				Str("path", r.URL.Path).
				Str("remote_addr", r.RemoteAddr).
				Msg("Invalid API secret")
			writeError(w, http.StatusUnauthorized, "Invalid API secret")
			return
		}

		next.ServeHTTP(w, r)
	})
}
