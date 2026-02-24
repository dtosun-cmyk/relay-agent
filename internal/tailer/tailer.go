package tailer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

const (
	defaultBufferSize       = 64 * 1024 // 64KB
	defaultPositionSaveFreq = 1000      // Save position every N lines
	positionSaveInterval    = 5 * time.Second
)

// Tailer efficiently tails a file using inotify and handles log rotation.
type Tailer struct {
	filePath     string
	lineChan     chan string
	positionFile string

	file     *os.File
	watcher  *fsnotify.Watcher
	position int64

	bufSize int

	// Reusable scanner buffer - avoids 64KB allocation per inotify event
	scanBuf []byte

	// Metrics
	linesRead atomic.Int64
	bytesRead atomic.Int64

	// Position tracking for periodic saves
	lastSavedPosition  int64
	linesSinceLastSave int64
	lastSaveTime       time.Time

	// Synchronization
	mu     sync.Mutex
	stopCh chan struct{}
	wg     sync.WaitGroup

	logger zerolog.Logger
}

// TailerOption configures the Tailer.
type TailerOption func(*Tailer)

// WithPositionFile sets the file path for persisting read position.
func WithPositionFile(path string) TailerOption {
	return func(t *Tailer) {
		t.positionFile = path
	}
}

// WithBufferSize sets the buffer size for reading.
func WithBufferSize(size int) TailerOption {
	return func(t *Tailer) {
		t.bufSize = size
	}
}

// WithLogger sets the logger.
func WithLogger(logger zerolog.Logger) TailerOption {
	return func(t *Tailer) {
		t.logger = logger
	}
}

// NewTailer creates a new file tailer.
func NewTailer(filePath string, lineChan chan string, opts ...TailerOption) *Tailer {
	t := &Tailer{
		filePath:     filePath,
		lineChan:     lineChan,
		positionFile: filepath.Join(os.TempDir(), fmt.Sprintf(".tailer-%s.pos", filepath.Base(filePath))),
		bufSize:      defaultBufferSize,
		scanBuf:      make([]byte, 0, defaultBufferSize), // Pre-allocate once, reuse forever
		stopCh:       make(chan struct{}),
		logger:       zerolog.Nop(),
	}

	for _, opt := range opts {
		opt(t)
	}

	t.logger = t.logger.With().Str("component", "tailer").Str("file", filePath).Logger()

	return t
}

// Start begins tailing the file with inotify-based watching.
func (t *Tailer) Start(ctx context.Context) error {
	// Load saved position
	position, err := t.loadPosition()
	if err != nil {
		t.logger.Warn().Err(err).Msg("failed to load position, starting from beginning")
		position = 0
	} else {
		t.logger.Info().Int64("position", position).Msg("loaded saved position")
	}

	// Open file
	if err := t.openFile(position); err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	// Create fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.file.Close()
		return fmt.Errorf("failed to create watcher: %w", err)
	}
	t.watcher = watcher

	// Watch the file
	if err := t.watcher.Add(t.filePath); err != nil {
		t.file.Close()
		t.watcher.Close()
		return fmt.Errorf("failed to watch file: %w", err)
	}

	// Also watch the directory for rotation detection
	dir := filepath.Dir(t.filePath)
	if err := t.watcher.Add(dir); err != nil {
		t.logger.Warn().Err(err).Msg("failed to watch directory, rotation detection may be limited")
	}

	// Read initial content if file has data beyond our position
	if err := t.readLines(); err != nil {
		t.logger.Warn().Err(err).Msg("failed to read initial lines")
	}

	t.lastSaveTime = time.Now()

	// Start main loop
	t.wg.Add(1)
	go t.run(ctx)

	t.logger.Info().Msg("tailer started")
	return nil
}

// Stop gracefully shuts down the tailer.
func (t *Tailer) Stop() {
	t.logger.Info().Msg("stopping tailer")
	close(t.stopCh)
	t.wg.Wait()

	// Save final position
	if err := t.savePosition(); err != nil {
		t.logger.Error().Err(err).Msg("failed to save final position")
	}

	if t.watcher != nil {
		t.watcher.Close()
	}

	t.mu.Lock()
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
	t.mu.Unlock()

	t.logger.Info().
		Int64("lines_read", t.linesRead.Load()).
		Int64("bytes_read", t.bytesRead.Load()).
		Msg("tailer stopped")
}

// Stats returns read metrics.
func (t *Tailer) Stats() (lines int64, bytes int64) {
	return t.linesRead.Load(), t.bytesRead.Load()
}

// run is the main event loop.
func (t *Tailer) run(ctx context.Context) {
	defer t.wg.Done()

	ticker := time.NewTicker(positionSaveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-t.stopCh:
			return

		case <-ticker.C:
			// Periodic position save
			if t.shouldSavePosition() {
				if err := t.savePosition(); err != nil {
					t.logger.Error().Err(err).Msg("failed to save position")
				}
			}

		case event, ok := <-t.watcher.Events:
			if !ok {
				return
			}

			if err := t.handleEvent(event); err != nil {
				t.logger.Error().Err(err).Str("event", event.String()).Msg("failed to handle event")
			}

		case err, ok := <-t.watcher.Errors:
			if !ok {
				return
			}
			t.logger.Error().Err(err).Msg("watcher error")
		}
	}
}

// handleEvent processes fsnotify events.
func (t *Tailer) handleEvent(event fsnotify.Event) error {
	// Only process events for our target file
	if event.Name != t.filePath {
		// Check if it's a create event in our directory (rotation scenario)
		if event.Has(fsnotify.Create) && filepath.Base(event.Name) == filepath.Base(t.filePath) {
			t.logger.Info().Msg("file recreated, handling rotation")
			return t.handleRotation()
		}
		return nil
	}

	switch {
	case event.Has(fsnotify.Write):
		// New data written to file
		return t.readLines()

	case event.Has(fsnotify.Remove), event.Has(fsnotify.Rename):
		// File removed or renamed (log rotation)
		t.logger.Info().Str("op", event.Op.String()).Msg("file rotation detected")
		return t.handleRotation()

	case event.Has(fsnotify.Chmod):
		// Ignore chmod events
		return nil
	}

	return nil
}

// openFile opens the file and seeks to the given position.
func (t *Tailer) openFile(position int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	file, err := os.Open(t.filePath)
	if err != nil {
		return err
	}

	// Get file size
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}

	// If position is beyond file size, file was truncated/rotated
	if position > stat.Size() {
		t.logger.Info().
			Int64("saved_position", position).
			Int64("file_size", stat.Size()).
			Msg("file truncated, starting from beginning")
		position = 0
	}

	// Seek to position
	if _, err := file.Seek(position, io.SeekStart); err != nil {
		file.Close()
		return err
	}

	t.file = file
	t.position = position
	t.lastSavedPosition = position

	return nil
}

// readLines reads available lines from the file.
func (t *Tailer) readLines() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return fmt.Errorf("file not open")
	}

	scanner := bufio.NewScanner(t.file)
	scanner.Buffer(t.scanBuf, t.bufSize) // Reuse pre-allocated buffer

	linesRead := 0
	for scanner.Scan() {
		line := scanner.Text()
		lineBytes := int64(len(line) + 1) // +1 for newline

		// Send line to channel (non-blocking)
		select {
		case t.lineChan <- line:
			t.position += lineBytes
			t.bytesRead.Add(lineBytes)
			t.linesRead.Add(1)
			linesRead++
			t.linesSinceLastSave++

		case <-t.stopCh:
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		// Check if error is due to file being truncated
		if t.checkTruncation() {
			return nil
		}
		return fmt.Errorf("scanner error: %w", err)
	}

	if linesRead > 0 {
		t.logger.Debug().Int("lines", linesRead).Int64("position", t.position).Msg("read lines")
	}

	return nil
}

// checkTruncation checks if the file was truncated and resets position if needed.
func (t *Tailer) checkTruncation() bool {
	stat, err := t.file.Stat()
	if err != nil {
		return false
	}

	if stat.Size() < t.position {
		t.logger.Info().
			Int64("current_position", t.position).
			Int64("file_size", stat.Size()).
			Msg("file truncated, resetting position")

		// Seek to beginning
		if _, err := t.file.Seek(0, io.SeekStart); err != nil {
			t.logger.Error().Err(err).Msg("failed to seek after truncation")
			return false
		}

		t.position = 0
		return true
	}

	return false
}

// handleRotation handles log rotation by reopening the file.
func (t *Tailer) handleRotation() error {
	t.logger.Info().Msg("handling log rotation")

	// Save current position before closing
	if err := t.savePosition(); err != nil {
		t.logger.Error().Err(err).Msg("failed to save position before rotation")
	}

	t.mu.Lock()
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
	t.mu.Unlock()

	// Wait a bit for the new file to be created
	time.Sleep(100 * time.Millisecond)

	// Try to open the new file
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		// Check if file exists
		if _, err := os.Stat(t.filePath); os.IsNotExist(err) {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Open from beginning (new file after rotation)
		if err := t.openFile(0); err != nil {
			t.logger.Error().Err(err).Int("attempt", i+1).Msg("failed to reopen file")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Re-add watch (may have been removed)
		if err := t.watcher.Add(t.filePath); err != nil {
			t.logger.Warn().Err(err).Msg("failed to re-add watch after rotation")
		}

		t.logger.Info().Msg("successfully reopened file after rotation")
		return nil
	}

	return fmt.Errorf("failed to reopen file after %d attempts", maxRetries)
}

// shouldSavePosition determines if position should be saved.
func (t *Tailer) shouldSavePosition() bool {
	if t.linesSinceLastSave >= defaultPositionSaveFreq {
		return true
	}

	if time.Since(t.lastSaveTime) >= positionSaveInterval && t.linesSinceLastSave > 0 {
		return true
	}

	return false
}

// savePosition persists the current read position to disk.
func (t *Tailer) savePosition() error {
	t.mu.Lock()
	position := t.position
	t.mu.Unlock()

	// Don't save if position hasn't changed
	if position == t.lastSavedPosition {
		return nil
	}

	// Create temporary file
	tmpFile := t.positionFile + ".tmp"
	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to create position file: %w", err)
	}

	if _, err := fmt.Fprintf(f, "%d\n", position); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("failed to write position: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("failed to sync position file: %w", err)
	}

	f.Close()

	// Atomic rename
	if err := os.Rename(tmpFile, t.positionFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to rename position file: %w", err)
	}

	t.lastSavedPosition = position
	t.linesSinceLastSave = 0
	t.lastSaveTime = time.Now()

	t.logger.Debug().Int64("position", position).Msg("saved position")
	return nil
}

// loadPosition restores the read position from disk.
func (t *Tailer) loadPosition() (int64, error) {
	data, err := os.ReadFile(t.positionFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // No saved position, start from beginning
		}
		return 0, fmt.Errorf("failed to read position file: %w", err)
	}

	posStr := strings.TrimSpace(string(data))
	position, err := strconv.ParseInt(posStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse position: %w", err)
	}

	if position < 0 {
		return 0, nil
	}

	return position, nil
}
