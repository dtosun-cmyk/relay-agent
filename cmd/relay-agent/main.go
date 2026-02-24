package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"relay-agent/internal/api"
	"relay-agent/internal/config"
	"relay-agent/internal/filter"
	"relay-agent/internal/parser"
	"relay-agent/internal/repository"
	"relay-agent/internal/stats"
	"relay-agent/internal/tailer"
)

var (
	Version   = "dev"
	BuildTime = ""
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", "/opt/relay-agent/config/config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Setup logger
	logger := setupLogger(cfg)
	logger.Info().
		Str("version", Version).
		Str("build_time", BuildTime).
		Str("config", *configPath).
		Msg("Starting relay-agent")

	// Create root context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to MongoDB
	logger.Info().Str("uri", cfg.MongoDB.URI).Str("database", cfg.MongoDB.Database).Msg("Connecting to MongoDB")
	repo, err := repository.NewMongoRepository(cfg.MongoDB.URI, cfg.MongoDB.Database, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to connect to MongoDB")
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := repo.Close(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("Failed to close MongoDB connection")
		}
	}()

	// Ensure MongoDB indexes
	logger.Info().Msg("Ensuring MongoDB indexes")
	indexCtx, indexCancel := context.WithTimeout(ctx, 30*time.Second)
	defer indexCancel()
	if err := repo.EnsureIndexes(indexCtx); err != nil {
		logger.Fatal().Err(err).Msg("Failed to ensure MongoDB indexes")
	}

	// Create buffered channels
	lineChan := make(chan string, cfg.Processing.ChannelBuffer)
	emailChan := make(chan *repository.Email, cfg.Processing.ChannelBuffer)

	logger.Info().
		Int("line_buffer", cfg.Processing.ChannelBuffer).
		Int("email_buffer", cfg.Processing.ChannelBuffer).
		Msg("Created processing channels")

	// Start SMTP filter if enabled
	var smtpFilter *filter.SMTPFilter
	if cfg.Filter.Enabled {
		smtpFilter = filter.NewSMTPFilter(
			cfg.Filter.ListenAddr,
			cfg.Filter.NextHop,
			emailChan,
			logger,
		)

		go func() {
			if err := smtpFilter.Start(ctx); err != nil {
				logger.Error().Err(err).Msg("SMTP filter error")
			}
		}()

		logger.Info().
			Str("listen", cfg.Filter.ListenAddr).
			Str("next_hop", cfg.Filter.NextHop).
			Msg("SMTP filter started")
	}

	// Create components
	logger.Info().Msg("Creating components")

	// Parser
	emailParser := parser.NewParser(emailChan)

	// Start parser cleanup goroutine (cleans stale entries every 5 minutes)
	emailParser.StartCleanup(5 * time.Minute)
	defer emailParser.StopCleanup()

	// Tailer
	positionFile := filepath.Join(os.TempDir(), "relay-agent-tailer.pos")
	logTailer := tailer.NewTailer(
		cfg.Postfix.LogFile,
		lineChan,
		tailer.WithPositionFile(positionFile),
		tailer.WithLogger(logger),
	)

	// Stats Collector
	statsCollector := stats.NewStatsCollector(
		repo,
		time.Duration(cfg.Processing.FlushInterval)*time.Second,
		logger,
	)

	// API Server
	apiServer := api.NewServer(
		api.ServerConfig{
			Host:          cfg.Server.Host,
			Port:          cfg.Server.Port,
			Version:       Version,
			SMTPDomain:    cfg.SMTP.Domain,
			SMTPAPISecret: cfg.SMTP.APISecret,
		},
		repo,
		statsCollector,
		logger,
	)

	// Start components
	logger.Info().Msg("Starting components")

	// Start stats collector
	statsCollector.Start(ctx)

	// Start API server in goroutine
	go func() {
		if err := apiServer.Start(); err != nil {
			logger.Error().Err(err).Msg("API server error")
		}
	}()

	// Main processing loop
	batchProcessor := &BatchProcessor{
		repo:           repo,
		statsCollector: statsCollector,
		emailChan:      emailChan,
		batchSize:      cfg.Processing.BatchSize,
		flushInterval:  time.Duration(cfg.Processing.FlushInterval) * time.Second,
		logger:         logger,
	}

	go batchProcessor.Start(ctx)

	// Start log line processing BEFORE tailer (tailer blocks if channel is full)
	go processLogLines(ctx, lineChan, emailParser, logger)

	// Start tailer (must be after processLogLines to avoid blocking)
	if err := logTailer.Start(ctx); err != nil {
		logger.Fatal().Err(err).Msg("Failed to start tailer")
	}

	logger.Info().Msg("All components started successfully")

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		logger.Info().Str("signal", sig.String()).Msg("Received shutdown signal")
	case <-ctx.Done():
		logger.Info().Msg("Context cancelled")
	}

	// Graceful shutdown
	logger.Info().Msg("Starting graceful shutdown")
	shutdown(ctx, smtpFilter, logTailer, emailParser, statsCollector, apiServer, logger)
	logger.Info().Msg("Relay agent stopped")
}

// setupLogger creates and configures the zerolog logger
func setupLogger(cfg *config.Config) zerolog.Logger {
	// Parse log level
	level := zerolog.InfoLevel
	switch cfg.Logging.Level {
	case "debug":
		level = zerolog.DebugLevel
	case "info":
		level = zerolog.InfoLevel
	case "warn", "warning":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	case "fatal":
		level = zerolog.FatalLevel
	case "panic":
		level = zerolog.PanicLevel
	}

	zerolog.SetGlobalLevel(level)

	// Create log file directory if it doesn't exist
	logDir := filepath.Dir(cfg.Logging.File)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create log directory: %v\n", err)
		os.Exit(1)
	}

	// Open log file
	logFile, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
		os.Exit(1)
	}

	// Configure multi-writer (file + console)
	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}

	multiWriter := zerolog.MultiLevelWriter(consoleWriter, logFile)

	logger := zerolog.New(multiWriter).
		With().
		Timestamp().
		Str("service", "relay-agent").
		Str("version", Version).
		Logger()

	return logger
}

// processLogLines reads log lines from channel and passes them to parser
func processLogLines(ctx context.Context, lineChan <-chan string, p *parser.Parser, logger zerolog.Logger) {
	logger.Info().Msg("Starting log line processor")
	lineCount := 0
	errorCount := 0

	for {
		select {
		case <-ctx.Done():
			logger.Info().
				Int("lines_processed", lineCount).
				Int("errors", errorCount).
				Msg("Log line processor stopped")
			return

		case line, ok := <-lineChan:
			if !ok {
				logger.Info().
					Int("lines_processed", lineCount).
					Int("errors", errorCount).
					Msg("Log line channel closed")
				return
			}

			if err := p.ParseLine(line); err != nil {
				errorCount++
				logger.Debug().Err(err).Str("line", line).Msg("Failed to parse line")
			}
			lineCount++

			// Log progress periodically
			if lineCount%10000 == 0 {
				logger.Debug().
					Int("lines_processed", lineCount).
					Int("pending_emails", p.PendingCount()).
					Msg("Processing progress")
			}
		}
	}
}

// BatchProcessor handles batching emails for MongoDB insertion
type BatchProcessor struct {
	repo           *repository.MongoRepository
	statsCollector *stats.StatsCollector
	emailChan      <-chan *repository.Email
	batchSize      int
	flushInterval  time.Duration
	logger         zerolog.Logger
}

// Start begins the batch processing loop
func (bp *BatchProcessor) Start(ctx context.Context) {
	bp.logger.Info().
		Int("batch_size", bp.batchSize).
		Dur("flush_interval", bp.flushInterval).
		Msg("Starting batch processor")

	batch := make([]*repository.Email, 0, bp.batchSize)
	ticker := time.NewTicker(bp.flushInterval)
	defer ticker.Stop()

	processedCount := 0

	flush := func() {
		if len(batch) == 0 {
			return
		}

		insertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		// Separate emails by whether they have MailgatewayQueueID
		bulkEmails := make([]*repository.Email, 0, len(batch))
		matchEmails := make([]*repository.Email, 0)

		for _, email := range batch {
			if email.MailgatewayQueueID != "" {
				bulkEmails = append(bulkEmails, email)
			} else {
				matchEmails = append(matchEmails, email)
			}
		}

		upsertSuccess := 0
		matchSuccess := 0
		skipped := 0

		// Bulk upsert emails with MailgatewayQueueID
		if len(bulkEmails) > 0 {
			if err := bp.repo.BulkUpsertByMailgatewayQueueID(insertCtx, bulkEmails); err != nil {
				bp.logger.Error().
					Err(err).
					Int("count", len(bulkEmails)).
					Msg("Failed to bulk upsert emails")
			} else {
				upsertSuccess = len(bulkEmails)
			}
		}

		// Process emails without MailgatewayQueueID individually (need matching)
		for _, email := range matchEmails {
			// No MailgatewayQueueID - try to match with existing filter record
			// This handles reinjected emails from content_filter
			mailgatewayQueueID, err := bp.repo.UpdateByRecipientMatch(insertCtx, email)
			if err != nil {
				bp.logger.Error().
					Err(err).
					Str("recipient", email.Recipient).
					Msg("Failed to match email by recipient")
			} else if mailgatewayQueueID != "" {
				matchSuccess++
				// Set the mailgateway_queue_id for tracking
				email.MailgatewayQueueID = mailgatewayQueueID
			} else {
				// No match found - skip (don't create orphan records)
				skipped++
				bp.logger.Debug().
					Str("recipient", email.Recipient).
					Str("queue_id", email.QueueID).
					Msg("No matching filter record found, skipping")
			}
		}

		if upsertSuccess > 0 || matchSuccess > 0 {
			bp.logger.Debug().
				Int("bulk_upsert", upsertSuccess).
				Int("matched", matchSuccess).
				Int("skipped", skipped).
				Msg("Batch processing completed")
		}

		// Update stats for each email
		for _, email := range batch {
			// Update statistics
			bp.statsCollector.RecordDelivery(email.Provider, email.Status, email.DeliveryTimeMs)

			// Return email to pool
			repository.PutEmail(email)
		}

		processedCount += len(batch)
		batch = batch[:0]

		// Log progress
		if processedCount%1000 == 0 {
			bp.logger.Info().Int("processed", processedCount).Msg("Processing progress")
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			bp.logger.Info().Int("total_processed", processedCount).Msg("Batch processor stopped")
			return

		case email, ok := <-bp.emailChan:
			if !ok {
				flush()
				bp.logger.Info().Int("total_processed", processedCount).Msg("Email channel closed")
				return
			}

			batch = append(batch, email)

			// Flush if batch is full
			if len(batch) >= bp.batchSize {
				flush()
				ticker.Reset(bp.flushInterval)
			}

		case <-ticker.C:
			flush()
		}
	}
}

// shutdown performs graceful shutdown of all components
func shutdown(
	ctx context.Context,
	smtpFilter *filter.SMTPFilter,
	logTailer *tailer.Tailer,
	emailParser *parser.Parser,
	statsCollector *stats.StatsCollector,
	apiServer *api.Server,
	logger zerolog.Logger,
) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop SMTP filter if running
	if smtpFilter != nil {
		logger.Info().Msg("Stopping SMTP filter")
		if err := smtpFilter.Stop(); err != nil {
			logger.Error().Err(err).Msg("Failed to stop SMTP filter")
		}
		received, forwarded, errors := smtpFilter.Stats()
		logger.Info().
			Int64("received", received).
			Int64("forwarded", forwarded).
			Int64("errors", errors).
			Msg("SMTP filter stopped")
	}

	// Stop tailer (no more log lines)
	logger.Info().Msg("Stopping tailer")
	logTailer.Stop()

	// Flush parser (process pending emails)
	logger.Info().Msg("Flushing parser")
	flushedCount := emailParser.Flush()
	logger.Info().Int("flushed", flushedCount).Msg("Parser flushed")

	// Wait a bit for batch processor to finish
	logger.Info().Msg("Waiting for batch processor to drain")
	time.Sleep(2 * time.Second)

	// Stop stats collector (flush stats)
	logger.Info().Msg("Stopping stats collector")
	statsCollector.Stop()

	// Shutdown API server
	logger.Info().Msg("Shutting down API server")
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		logger.Error().Err(err).Msg("Failed to shutdown API server")
	}

	logger.Info().Msg("Graceful shutdown complete")
}
