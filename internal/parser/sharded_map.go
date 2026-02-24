package parser

import (
	"sync"

	"relay-agent/internal/repository"
)

// ShardedPendingMap is a thread-safe map that uses multiple shards
// to reduce lock contention when multiple goroutines access it concurrently.
type ShardedPendingMap struct {
	shards    []*mapShard
	shardMask uint32
}

// mapShard represents a single shard of the map.
type mapShard struct {
	mu    sync.RWMutex
	items map[string]*repository.LogEntry
}

// defaultShardCount is the number of shards (must be power of 2).
const defaultShardCount = 32

// NewShardedPendingMap creates a new sharded map with the default number of shards.
func NewShardedPendingMap() *ShardedPendingMap {
	return NewShardedPendingMapWithSize(defaultShardCount)
}

// NewShardedPendingMapWithSize creates a new sharded map with the specified number of shards.
// shardCount must be a power of 2.
func NewShardedPendingMapWithSize(shardCount int) *ShardedPendingMap {
	// Ensure shardCount is power of 2
	if shardCount&(shardCount-1) != 0 {
		shardCount = defaultShardCount
	}

	m := &ShardedPendingMap{
		shards:    make([]*mapShard, shardCount),
		shardMask: uint32(shardCount - 1),
	}

	for i := 0; i < shardCount; i++ {
		m.shards[i] = &mapShard{
			items: make(map[string]*repository.LogEntry, 64), // Pre-allocate reasonable size per shard
		}
	}

	return m
}

// getShard returns the shard for the given key using FNV-1a hash.
func (m *ShardedPendingMap) getShard(key string) *mapShard {
	hash := fnv1a32(key)
	return m.shards[hash&m.shardMask]
}

// fnv1a32 calculates the FNV-1a hash of a string.
// This is a fast, well-distributed hash function.
func fnv1a32(s string) uint32 {
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)

	hash := offset32
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime32
	}
	return hash
}

// Get retrieves a value from the map.
func (m *ShardedPendingMap) Get(key string) (*repository.LogEntry, bool) {
	shard := m.getShard(key)
	shard.mu.RLock()
	entry, ok := shard.items[key]
	shard.mu.RUnlock()
	return entry, ok
}

// GetOrCreate retrieves an existing entry or creates a new one if it doesn't exist.
// Returns the entry and a boolean indicating if it was newly created.
func (m *ShardedPendingMap) GetOrCreate(key string, createFn func() *repository.LogEntry) (*repository.LogEntry, bool) {
	shard := m.getShard(key)

	// First try read lock (fast path)
	shard.mu.RLock()
	entry, ok := shard.items[key]
	shard.mu.RUnlock()

	if ok {
		return entry, false
	}

	// Slow path: need to create
	shard.mu.Lock()
	// Double-check after acquiring write lock
	entry, ok = shard.items[key]
	if ok {
		shard.mu.Unlock()
		return entry, false
	}

	// Create new entry
	entry = createFn()
	shard.items[key] = entry
	shard.mu.Unlock()

	return entry, true
}

// Set stores a value in the map.
func (m *ShardedPendingMap) Set(key string, entry *repository.LogEntry) {
	shard := m.getShard(key)
	shard.mu.Lock()
	shard.items[key] = entry
	shard.mu.Unlock()
}

// Delete removes a key from the map.
func (m *ShardedPendingMap) Delete(key string) {
	shard := m.getShard(key)
	shard.mu.Lock()
	delete(shard.items, key)
	shard.mu.Unlock()
}

// Count returns the total number of entries across all shards.
func (m *ShardedPendingMap) Count() int {
	count := 0
	for _, shard := range m.shards {
		shard.mu.RLock()
		count += len(shard.items)
		shard.mu.RUnlock()
	}
	return count
}

// GetAllAndClear returns all entries and clears the map.
// This is used for flushing pending entries.
// Single-pass implementation: avoids double locking and reduces cache misses.
func (m *ShardedPendingMap) GetAllAndClear() []*repository.LogEntry {
	// Single pass: collect and clear atomically per shard
	// Estimate capacity from shard count (avoid second pass for counting)
	entries := make([]*repository.LogEntry, 0, len(m.shards)*8)

	for _, shard := range m.shards {
		shard.mu.Lock()
		if len(shard.items) > 0 {
			for _, entry := range shard.items {
				entries = append(entries, entry)
			}
			// Clear by replacing with fresh map (faster than delete loop for large maps)
			shard.items = make(map[string]*repository.LogEntry, 64)
		}
		shard.mu.Unlock()
	}

	if len(entries) == 0 {
		return nil
	}
	return entries
}

// CollectStale collects and removes entries that match the predicate.
// Used for cleanup of stale entries.
// Optimized: deletes in-place during range iteration (safe in Go) to avoid
// intermediate slice allocation for stale keys.
func (m *ShardedPendingMap) CollectStale(predicate func(*repository.LogEntry) bool) []*repository.LogEntry {
	var staleEntries []*repository.LogEntry

	for _, shard := range m.shards {
		shard.mu.Lock()
		for key, entry := range shard.items {
			if predicate(entry) {
				staleEntries = append(staleEntries, entry)
				delete(shard.items, key) // Safe: Go allows delete during range
			}
		}
		shard.mu.Unlock()
	}

	return staleEntries
}
