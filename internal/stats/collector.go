package stats

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"relay-agent/internal/repository"
)

// Repository interface for dependency injection
type Repository interface {
	SaveHourlyStats(ctx context.Context, stats *repository.HourlyStats) error
}

// StatsCollector provides lock-free statistics collection with periodic persistence
type StatsCollector struct {
	// Daily counters (reset at midnight)
	todaySent     atomic.Int64
	todayDeferred atomic.Int64
	todayBounced  atomic.Int64
	todayTotal    atomic.Int64

	// Delivery time tracking (for average calculation)
	totalDeliveryMs atomic.Int64
	deliveryCount   atomic.Int64

	// Provider-specific counters (thread-safe map)
	providerStats sync.Map // map[string]*ProviderCounter

	// Hourly stats for persistence
	currentHour atomic.Int32
	hourlyStats [24]*HourlyCounter // Pre-allocated array for 24 hours

	// Last processed date for daily reset detection
	lastDate atomic.Value // stores time.Time

	// Repository for persistence
	repo Repository

	// Background flush
	flushInterval time.Duration
	stopChan      chan struct{}
	wg            sync.WaitGroup

	logger zerolog.Logger
}

// ProviderCounter holds per-provider statistics with atomic counters
type ProviderCounter struct {
	Sent     atomic.Int64
	Deferred atomic.Int64
	Bounced  atomic.Int64
	Total    atomic.Int64
	TotalMs  atomic.Int64
	Count    atomic.Int64
}

// HourlyCounter holds per-hour statistics with atomic counters
type HourlyCounter struct {
	Sent     atomic.Int64
	Deferred atomic.Int64
	Bounced  atomic.Int64
	TotalMs  atomic.Int64
	Count    atomic.Int64
}

// TodayStats represents aggregated daily statistics
type TodayStats struct {
	Sent          int64   `json:"sent"`
	Deferred      int64   `json:"deferred"`
	Bounced       int64   `json:"bounced"`
	Total         int64   `json:"total"`
	AvgDeliveryMs float64 `json:"avg_delivery_ms"`
}

// ProviderStatsSnapshot represents per-provider statistics snapshot
type ProviderStatsSnapshot struct {
	Sent          int64   `json:"sent"`
	Deferred      int64   `json:"deferred"`
	Bounced       int64   `json:"bounced"`
	Total         int64   `json:"total"`
	AvgDeliveryMs float64 `json:"avg_delivery_ms"`
}

// HourlyStatsSnapshot represents per-hour statistics snapshot
type HourlyStatsSnapshot struct {
	Hour          int     `json:"hour"`
	Sent          int64   `json:"sent"`
	Deferred      int64   `json:"deferred"`
	Bounced       int64   `json:"bounced"`
	AvgDeliveryMs float64 `json:"avg_delivery_ms"`
}

// NewStatsCollector creates a new stats collector instance
func NewStatsCollector(repo Repository, flushInterval time.Duration, logger zerolog.Logger) *StatsCollector {
	now := time.Now()
	currentHour := now.Hour()

	collector := &StatsCollector{
		repo:          repo,
		flushInterval: flushInterval,
		stopChan:      make(chan struct{}),
		logger:        logger.With().Str("component", "stats_collector").Logger(),
	}

	// Pre-allocate hourly counters to avoid allocation during operation
	for i := 0; i < 24; i++ {
		collector.hourlyStats[i] = &HourlyCounter{}
	}

	// Initialize current hour
	collector.currentHour.Store(int32(currentHour))

	// Initialize last date
	collector.lastDate.Store(now)

	return collector
}

// Start begins the background flush goroutine
func (s *StatsCollector) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.flushLoop(ctx)
	s.logger.Info().
		Dur("flush_interval", s.flushInterval).
		Msg("Stats collector started")
}

// Stop gracefully stops the stats collector
func (s *StatsCollector) Stop() {
	close(s.stopChan)
	s.wg.Wait()
	s.logger.Info().Msg("Stats collector stopped")
}

// RecordDelivery records a delivery event (lock-free, zero allocation for existing providers).
// Optimized: single switch for all counter updates improves branch prediction
// and CPU cache locality by keeping related memory accesses together.
func (s *StatsCollector) RecordDelivery(provider, status string, deliveryTimeMs int64) {
	// Update daily counters (always)
	s.todayTotal.Add(1)
	s.totalDeliveryMs.Add(deliveryTimeMs)
	s.deliveryCount.Add(1)

	// Update hourly counters (always)
	hour := s.getCurrentHour()
	hourlyCounter := s.hourlyStats[hour]
	hourlyCounter.Count.Add(1)
	hourlyCounter.TotalMs.Add(deliveryTimeMs)

	// Get provider counter once (if needed)
	var providerCounter *ProviderCounter
	if provider != "" {
		providerCounter = s.getOrCreateProvider(provider)
		providerCounter.Total.Add(1)
		providerCounter.TotalMs.Add(deliveryTimeMs)
		providerCounter.Count.Add(1)
	}

	// Single switch for status-specific increments - better branch prediction
	// CPU can predict this single branch pattern much better than 3 separate switches
	switch status {
	case "sent", "delivered":
		s.todaySent.Add(1)
		hourlyCounter.Sent.Add(1)
		if providerCounter != nil {
			providerCounter.Sent.Add(1)
		}
	case "deferred":
		s.todayDeferred.Add(1)
		hourlyCounter.Deferred.Add(1)
		if providerCounter != nil {
			providerCounter.Deferred.Add(1)
		}
	case "bounced", "failed":
		s.todayBounced.Add(1)
		hourlyCounter.Bounced.Add(1)
		if providerCounter != nil {
			providerCounter.Bounced.Add(1)
		}
	}
}

// GetTodayStats returns a snapshot of today's statistics
func (s *StatsCollector) GetTodayStats() *TodayStats {
	sent := s.todaySent.Load()
	deferred := s.todayDeferred.Load()
	bounced := s.todayBounced.Load()
	total := s.todayTotal.Load()
	totalMs := s.totalDeliveryMs.Load()
	count := s.deliveryCount.Load()

	var avgMs float64
	if count > 0 {
		avgMs = float64(totalMs) / float64(count)
	}

	return &TodayStats{
		Sent:          sent,
		Deferred:      deferred,
		Bounced:       bounced,
		Total:         total,
		AvgDeliveryMs: avgMs,
	}
}

// GetProviderStats returns a snapshot of all provider statistics
func (s *StatsCollector) GetProviderStats() map[string]*ProviderStatsSnapshot {
	result := make(map[string]*ProviderStatsSnapshot)

	s.providerStats.Range(func(key, value interface{}) bool {
		provider := key.(string)
		counter := value.(*ProviderCounter)

		sent := counter.Sent.Load()
		deferred := counter.Deferred.Load()
		bounced := counter.Bounced.Load()
		total := counter.Total.Load()
		totalMs := counter.TotalMs.Load()
		count := counter.Count.Load()

		var avgMs float64
		if count > 0 {
			avgMs = float64(totalMs) / float64(count)
		}

		result[provider] = &ProviderStatsSnapshot{
			Sent:          sent,
			Deferred:      deferred,
			Bounced:       bounced,
			Total:         total,
			AvgDeliveryMs: avgMs,
		}

		return true
	})

	return result
}

// GetHourlyStats returns a snapshot of hourly statistics
func (s *StatsCollector) GetHourlyStats() []*HourlyStatsSnapshot {
	result := make([]*HourlyStatsSnapshot, 0, 24)

	for hour := 0; hour < 24; hour++ {
		counter := s.hourlyStats[hour]

		sent := counter.Sent.Load()
		deferred := counter.Deferred.Load()
		bounced := counter.Bounced.Load()
		totalMs := counter.TotalMs.Load()
		count := counter.Count.Load()

		// Skip hours with no data
		if count == 0 {
			continue
		}

		var avgMs float64
		if count > 0 {
			avgMs = float64(totalMs) / float64(count)
		}

		result = append(result, &HourlyStatsSnapshot{
			Hour:          hour,
			Sent:          sent,
			Deferred:      deferred,
			Bounced:       bounced,
			AvgDeliveryMs: avgMs,
		})
	}

	return result
}

// flushLoop periodically flushes stats and checks for daily reset
func (s *StatsCollector) flushLoop(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check for day change and reset if needed
			s.checkAndResetDaily()

			// Check for hour change
			s.checkAndUpdateHour()

			// Flush current stats to database
			if err := s.flush(ctx); err != nil {
				s.logger.Error().Err(err).Msg("Failed to flush stats")
			}

		case <-s.stopChan:
			// Final flush before shutdown
			if err := s.flush(ctx); err != nil {
				s.logger.Error().Err(err).Msg("Failed to flush stats on shutdown")
			}
			return

		case <-ctx.Done():
			s.logger.Info().Msg("Context cancelled, stopping stats collector")
			return
		}
	}
}

// flush persists current hourly stats to the database
func (s *StatsCollector) flush(ctx context.Context) error {
	hour := s.getCurrentHour()
	counter := s.hourlyStats[hour]

	sent := counter.Sent.Load()
	deferred := counter.Deferred.Load()
	bounced := counter.Bounced.Load()
	totalMs := counter.TotalMs.Load()
	count := counter.Count.Load()

	// Skip flush if no data
	if count == 0 {
		s.logger.Debug().Int("hour", hour).Msg("No data to flush")
		return nil
	}

	var avgMs float64
	if count > 0 {
		avgMs = float64(totalMs) / float64(count)
	}

	now := time.Now()
	stats := &repository.HourlyStats{
		Date:          time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location()),
		Hour:          hour,
		Sent:          sent,
		Deferred:      deferred,
		Bounced:       bounced,
		Total:         sent + deferred + bounced,
		AvgDeliveryMs: avgMs,
		UpdatedAt:     now,
	}

	if err := s.repo.SaveHourlyStats(ctx, stats); err != nil {
		return fmt.Errorf("failed to save hourly stats: %w", err)
	}

	s.logger.Debug().
		Int("hour", hour).
		Int64("sent", sent).
		Int64("deferred", deferred).
		Int64("bounced", bounced).
		Float64("avg_ms", avgMs).
		Msg("Stats flushed successfully")

	return nil
}

// checkAndResetDaily checks if the day has changed and resets daily counters
func (s *StatsCollector) checkAndResetDaily() {
	now := time.Now()
	lastDateVal := s.lastDate.Load()
	if lastDateVal == nil {
		return
	}

	lastDate := lastDateVal.(time.Time)
	lastDay := lastDate.Day()
	currentDay := now.Day()

	// Check if day has changed
	if lastDay != currentDay {
		s.resetDaily()
		s.lastDate.Store(now)
		s.logger.Info().
			Time("last_date", lastDate).
			Time("current_date", now).
			Msg("Daily stats reset")
	}
}

// resetDaily resets all daily counters to zero
func (s *StatsCollector) resetDaily() {
	s.todaySent.Store(0)
	s.todayDeferred.Store(0)
	s.todayBounced.Store(0)
	s.todayTotal.Store(0)
	s.totalDeliveryMs.Store(0)
	s.deliveryCount.Store(0)

	// Reset provider stats
	s.providerStats.Range(func(key, value interface{}) bool {
		counter := value.(*ProviderCounter)
		counter.Sent.Store(0)
		counter.Deferred.Store(0)
		counter.Bounced.Store(0)
		counter.Total.Store(0)
		counter.TotalMs.Store(0)
		counter.Count.Store(0)
		return true
	})

	// Reset all hourly counters
	for i := 0; i < 24; i++ {
		counter := s.hourlyStats[i]
		counter.Sent.Store(0)
		counter.Deferred.Store(0)
		counter.Bounced.Store(0)
		counter.TotalMs.Store(0)
		counter.Count.Store(0)
	}
}

// checkAndUpdateHour checks if the hour has changed and updates the current hour
func (s *StatsCollector) checkAndUpdateHour() {
	currentHour := time.Now().Hour()
	oldHour := s.currentHour.Load()

	if int(oldHour) != currentHour {
		s.currentHour.Store(int32(currentHour))
		s.logger.Debug().
			Int("old_hour", int(oldHour)).
			Int("new_hour", currentHour).
			Msg("Hour changed")
	}
}

// getOrCreateProvider retrieves or creates a provider counter (thread-safe)
func (s *StatsCollector) getOrCreateProvider(provider string) *ProviderCounter {
	// Fast path: provider already exists
	if val, ok := s.providerStats.Load(provider); ok {
		return val.(*ProviderCounter)
	}

	// Slow path: create new provider counter
	counter := &ProviderCounter{}
	actual, loaded := s.providerStats.LoadOrStore(provider, counter)

	if !loaded {
		s.logger.Debug().Str("provider", provider).Msg("New provider registered")
	}

	return actual.(*ProviderCounter)
}

// getCurrentHour returns the current hour from the atomic value
func (s *StatsCollector) getCurrentHour() int {
	return int(s.currentHour.Load())
}
