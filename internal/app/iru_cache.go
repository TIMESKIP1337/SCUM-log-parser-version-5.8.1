package app

import (
	"container/list"
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// LRUCache implements a thread-safe LRU cache with persistence
type LRUCache struct {
	capacity  int
	items     map[string]*list.Element
	order     *list.List
	mu        sync.RWMutex
	hits      int64
	misses    int64
	evictions int64
}

// cacheItem represents an item in the cache
type cacheItem struct {
	key       string
	timestamp time.Time
}

// CacheStats for monitoring
type CacheStats struct {
	Size      int     `json:"size"`
	Capacity  int     `json:"capacity"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	Evictions int64   `json:"evictions"`
	HitRate   float64 `json:"hit_rate"`
}

// NewLRUCache creates a new LRU cache with the specified capacity
func NewLRUCache(capacity int) *LRUCache {
	if capacity <= 0 {
		capacity = 50000 // Default 50k items
	}

	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Add adds a key to the cache
func (c *LRUCache) Add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If exists, move to front
	if elem, exists := c.items[key]; exists {
		c.order.MoveToFront(elem)
		elem.Value.(*cacheItem).timestamp = time.Now()
		return
	}

	// If at capacity, remove oldest
	if c.order.Len() >= c.capacity {
		c.evictOldest()
	}

	// Add new item
	item := &cacheItem{
		key:       key,
		timestamp: time.Now(),
	}
	elem := c.order.PushFront(item)
	c.items[key] = elem
}

// Contains checks if a key exists in the cache
func (c *LRUCache) Contains(key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if _, exists := c.items[key]; exists {
		c.hits++
		return true
	}

	c.misses++
	return false
}

// evictOldest removes the least recently used item
func (c *LRUCache) evictOldest() {
	oldest := c.order.Back()
	if oldest != nil {
		c.order.Remove(oldest)
		item := oldest.Value.(*cacheItem)
		delete(c.items, item.key)
		c.evictions++
	}
}

// Size returns the current size of the cache
func (c *LRUCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order.Len()
}

// Clear removes all items from the cache
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.order = list.New()
	c.hits = 0
	c.misses = 0
	c.evictions = 0
}

// Stats returns cache statistics
func (c *LRUCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.hits + c.misses
	hitRate := float64(0)
	if total > 0 {
		hitRate = float64(c.hits) / float64(total) * 100
	}

	return CacheStats{
		Size:      c.order.Len(),
		Capacity:  c.capacity,
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		HitRate:   hitRate,
	}
}

// SaveToFile saves the cache to a file with limited entries
func (c *LRUCache) SaveToFile(filename string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Prepare data for saving (only save most recent entries)
	maxSave := 10000 // Save only 10k most recent entries
	if maxSave > c.order.Len() {
		maxSave = c.order.Len()
	}

	type savedItem struct {
		Key       string    `json:"key"`
		Timestamp time.Time `json:"timestamp"`
	}

	savedItems := make([]savedItem, 0, maxSave)
	count := 0

	for elem := c.order.Front(); elem != nil && count < maxSave; elem = elem.Next() {
		item := elem.Value.(*cacheItem)
		savedItems = append(savedItems, savedItem{
			Key:       item.key,
			Timestamp: item.timestamp,
		})
		count++
	}

	// Create temp file first
	tempFile := filename + ".tmp"
	file, err := os.Create(tempFile)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(savedItems); err != nil {
		file.Close()
		os.Remove(tempFile)
		return err
	}

	file.Close()

	// Atomic rename
	if err := os.Rename(tempFile, filename); err != nil {
		return err
	}

	log.Printf("✅ Saved %d cache entries to %s", len(savedItems), filename)
	return nil
}

// LoadFromFile loads cache entries from a file
func (c *LRUCache) LoadFromFile(filename string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist, not an error
		}
		return err
	}
	defer file.Close()

	type savedItem struct {
		Key       string    `json:"key"`
		Timestamp time.Time `json:"timestamp"`
	}

	var savedItems []savedItem
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&savedItems); err != nil {
		return err
	}

	// Clear existing cache
	c.items = make(map[string]*list.Element)
	c.order = list.New()

	// Load items (they're already in order)
	loaded := 0
	for _, saved := range savedItems {
		if loaded >= c.capacity {
			break
		}

		item := &cacheItem{
			key:       saved.Key,
			timestamp: saved.Timestamp,
		}
		elem := c.order.PushBack(item) // Push to back since they're in order
		c.items[item.key] = elem
		loaded++
	}

	log.Printf("✅ Loaded %d cache entries from %s", loaded, filename)
	return nil
}

// CleanOldEntries removes entries older than the specified duration
func (c *LRUCache) CleanOldEntries(maxAge time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	// Start from the back (oldest entries)
	for elem := c.order.Back(); elem != nil; {
		item := elem.Value.(*cacheItem)
		if item.timestamp.After(cutoff) {
			break // All remaining items are newer
		}

		prev := elem.Prev()
		c.order.Remove(elem)
		delete(c.items, item.key)
		removed++
		elem = prev
	}

	if removed > 0 {
		log.Printf("🧹 Cleaned %d old cache entries", removed)
	}

	return removed
}

// Global cache instance to replace SharedSentLogs
var SentLogsCache *LRUCache

// Initialize the cache
func InitializeSentLogsCache() {
	capacity := getEnvInt("CACHE_CAPACITY", 50000)
	SentLogsCache = NewLRUCache(capacity)

	// Load from file if exists
	if err := SentLogsCache.LoadFromFile("sent_logs_cache.json"); err != nil {
		log.Printf("⚠️ Warning: Could not load cache from file: %v", err)
		log.Printf("🆕 Starting with empty cache - all logs will be processed as NEW")
	} else {
		stats := SentLogsCache.Stats()
		log.Printf("✅ Loaded cache with %d entries", stats.Size)
	}

	// Start periodic cleanup
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			// Clean entries older than 7 days
			SentLogsCache.CleanOldEntries(7 * 24 * time.Hour)

			// Log stats
			stats := SentLogsCache.Stats()
			log.Printf("📊 Cache stats: Size=%d/%d, Hits=%d, Misses=%d, HitRate=%.2f%%",
				stats.Size, stats.Capacity, stats.Hits, stats.Misses, stats.HitRate)
		}
	}()
}

// Replacement functions for the old SharedSentLogs system
func IsLogSentCached(logKey string) bool {
	return SentLogsCache.Contains(logKey)
}

// Counter for batch saving (save every N entries instead of every single one)
var (
	cacheWriteCounter int
	cacheWriteMux     sync.Mutex
)

func MarkLogAsSentCached(logKey string) {
	SentLogsCache.Add(logKey)

	// Realtime save: save to disk every 5 new entries for near-realtime
	cacheWriteMux.Lock()
	cacheWriteCounter++
	shouldSave := cacheWriteCounter >= 5
	if shouldSave {
		cacheWriteCounter = 0
	}
	cacheWriteMux.Unlock()

	if shouldSave {
		go func() {
			if err := SaveSentLogsCache(); err != nil {
				log.Printf("⚠️ Error saving cache: %v", err)
			}
		}()
	}
}

func SaveSentLogsCache() error {
	return SentLogsCache.SaveToFile("sent_logs_cache.json")
}

// Periodic save function
func StartCacheAutoSave(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final save on shutdown
			SaveSentLogsCache()
			return
		case <-ticker.C:
			if err := SaveSentLogsCache(); err != nil {
				log.Printf("Error saving cache: %v", err)
			}
		}
	}
}
