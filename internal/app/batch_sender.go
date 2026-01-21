package app

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// MessageBatch represents a batch of messages with capacity limits
type MessageBatch struct {
	ChannelID   string
	Messages    []string
	mu          sync.Mutex
	maxCapacity int // จำกัดขนาดสูงสุด
}

// BatchSender handles batched message sending with memory management
type BatchSender struct {
	session       *discordgo.Session
	batches       map[string]*MessageBatch
	batchMu       sync.RWMutex
	flushInterval time.Duration
	maxBatchSize  int
	messageDelay  time.Duration

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Memory management
	maxTotalMessages int // จำกัดจำนวนข้อความทั้งหมดที่เก็บไว้
	currentMessages  int
	messagesMu       sync.Mutex
}

// NewBatchSender creates a new batch sender with memory limits
func NewBatchSender(session *discordgo.Session) *BatchSender {
	ctx, cancel := context.WithCancel(context.Background())

	bs := &BatchSender{
		session:          session,
		batches:          make(map[string]*MessageBatch),
		flushInterval:    time.Duration(getEnvInt("BATCH_FLUSH_INTERVAL", 2)) * time.Second,
		maxBatchSize:     getEnvInt("MAX_BATCH_SIZE", 10),
		messageDelay:     time.Duration(getEnvInt("MESSAGE_DELAY_MS", 50)) * time.Millisecond,
		maxTotalMessages: getEnvInt("MAX_TOTAL_MESSAGES", 1000), // จำกัด 1000 ข้อความทั้งหมด
		ctx:              ctx,
		cancel:           cancel,
	}

	// เริ่ม auto-flush goroutine
	bs.wg.Add(1)
	go bs.autoFlush()

	log.Printf("✅ Batch Sender initialized (flush: %v, max batch: %d, delay: %v, max total: %d)",
		bs.flushInterval, bs.maxBatchSize, bs.messageDelay, bs.maxTotalMessages)

	return bs
}

// AddMessage เพิ่มข้อความเข้า batch พร้อมตรวจสอบ memory
func (bs *BatchSender) AddMessage(channelID, message string) {
	// ตรวจสอบว่าเกิน limit หรือไม่
	bs.messagesMu.Lock()
	if bs.currentMessages >= bs.maxTotalMessages {
		// ถ้าเต็มแล้ว flush ทันที
		bs.messagesMu.Unlock()
		bs.FlushAll()
		bs.messagesMu.Lock()
	}
	bs.currentMessages++
	bs.messagesMu.Unlock()

	bs.batchMu.Lock()
	batch, exists := bs.batches[channelID]
	if !exists {
		batch = &MessageBatch{
			ChannelID:   channelID,
			Messages:    make([]string, 0, bs.maxBatchSize),
			maxCapacity: bs.maxBatchSize * 2, // จำกัดไว้ที่ 2 เท่าของ batch size
		}
		bs.batches[channelID] = batch
	}
	bs.batchMu.Unlock()

	batch.mu.Lock()
	// ป้องกันไม่ให้ batch โตเกินกำหนด
	if len(batch.Messages) >= batch.maxCapacity {
		batch.mu.Unlock()
		bs.flushBatch(channelID)
		batch.mu.Lock()
	}

	batch.Messages = append(batch.Messages, message)
	shouldFlush := len(batch.Messages) >= bs.maxBatchSize
	batch.mu.Unlock()

	// ถ้า batch เต็มแล้ว flush ทันที
	if shouldFlush {
		bs.flushBatch(channelID)
	}
}

// flushBatch ส่งข้อความทั้งหมดใน batch
func (bs *BatchSender) flushBatch(channelID string) {
	bs.batchMu.RLock()
	batch, exists := bs.batches[channelID]
	bs.batchMu.RUnlock()

	if !exists {
		return
	}

	batch.mu.Lock()
	if len(batch.Messages) == 0 {
		batch.mu.Unlock()
		return
	}

	// คัดลอกข้อความและลบออกจาก batch
	messages := make([]string, len(batch.Messages))
	copy(messages, batch.Messages)
	msgCount := len(batch.Messages)
	batch.Messages = batch.Messages[:0] // ล้าง slice แต่เก็บ capacity
	batch.mu.Unlock()

	// อัพเดท counter
	bs.messagesMu.Lock()
	bs.currentMessages -= msgCount
	if bs.currentMessages < 0 {
		bs.currentMessages = 0
	}
	bs.messagesMu.Unlock()

	// ส่งข้อความ (แบบ concurrent)
	bs.wg.Add(1)
	go func(msgs []string) {
		defer bs.wg.Done()
		bs.sendMessages(channelID, msgs)
	}(messages)
}

// sendMessages ส่งข้อความทั้งหมด
func (bs *BatchSender) sendMessages(channelID string, messages []string) {
	for i, msg := range messages {
		// ตรวจสอบ context ก่อนส่ง
		select {
		case <-bs.ctx.Done():
			log.Printf("⚠️ Batch sender stopped, %d messages not sent", len(messages)-i)
			return
		default:
		}

		_, err := bs.session.ChannelMessageSend(channelID, msg)
		if err != nil {
			log.Printf("❌ Error sending batch message %d/%d: %v", i+1, len(messages), err)

			// ถ้าเจอ rate limit ให้รอนานขึ้น
			if strings.Contains(err.Error(), "rate limit") {
				time.Sleep(2 * time.Second)
				// ลองส่งใหม่
				bs.session.ChannelMessageSend(channelID, msg)
			}
		}

		// หน่วงเวลาเล็กน้อยระหว่างข้อความ
		if i < len(messages)-1 {
			time.Sleep(bs.messageDelay)
		}
	}

	UpdateSharedActivity()
	log.Printf("✅ Sent batch of %d messages to channel %s", len(messages), channelID)
}

// autoFlush ทำการ flush อัตโนมัติตาม interval
func (bs *BatchSender) autoFlush() {
	defer bs.wg.Done()

	ticker := time.NewTicker(bs.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-bs.ctx.Done():
			// Flush ทุก batch ก่อนปิด
			bs.FlushAll()
			return
		case <-ticker.C:
			bs.FlushAll()
		}
	}
}

// FlushAll flush ทุก batch
func (bs *BatchSender) FlushAll() {
	bs.batchMu.RLock()
	channels := make([]string, 0, len(bs.batches))
	for channelID := range bs.batches {
		channels = append(channels, channelID)
	}
	bs.batchMu.RUnlock()

	for _, channelID := range channels {
		bs.flushBatch(channelID)
	}
}

// Stop หยุดการทำงานของ batch sender
func (bs *BatchSender) Stop() {
	bs.cancel()  // ส่ง signal ให้ goroutines หยุด
	bs.wg.Wait() // รอให้ทุก goroutine จบ
	log.Println("✅ Batch Sender stopped")
}

// GetStats ดึงสถิติ batch sender
func (bs *BatchSender) GetStats() BatchSenderStats {
	bs.batchMu.RLock()
	batchCount := len(bs.batches)
	bs.batchMu.RUnlock()

	bs.messagesMu.Lock()
	pending := bs.currentMessages
	bs.messagesMu.Unlock()

	return BatchSenderStats{
		ActiveBatches:    batchCount,
		PendingMessages:  pending,
		MaxTotalMessages: bs.maxTotalMessages,
		UsagePercent:     float64(pending) / float64(bs.maxTotalMessages) * 100,
	}
}

type BatchSenderStats struct {
	ActiveBatches    int
	PendingMessages  int
	MaxTotalMessages int
	UsagePercent     float64
}

// ==========================================
// Global Batch Sender Instance
// ==========================================
var GlobalBatchSender *BatchSender

// InitializeBatchSender สร้าง global batch sender
func InitializeBatchSender(session *discordgo.Session) {
	GlobalBatchSender = NewBatchSender(session)
	log.Println("🚀 Global Batch Sender initialized")
}

// SendBatchedMessage ส่งข้อความผ่าน batch sender
func SendBatchedMessage(channelID, message string) {
	if GlobalBatchSender != nil {
		GlobalBatchSender.AddMessage(channelID, message)
	} else {
		// Fallback ถ้ายังไม่ได้ initialize
		SharedSession.ChannelMessageSend(channelID, message)
	}
}

// StopBatchSender หยุด batch sender
func StopBatchSender() {
	if GlobalBatchSender != nil {
		GlobalBatchSender.Stop()
	}
}

// ==========================================
// Optimized Embed Batch Sender
// ==========================================

// EmbedBatch represents a batch of embeds with capacity limits
type EmbedBatch struct {
	ChannelID   string
	Embeds      []*discordgo.MessageEmbed
	mu          sync.Mutex
	maxCapacity int
}

// EmbedBatchSender handles batched embed sending with memory management
type EmbedBatchSender struct {
	session       *discordgo.Session
	batches       map[string]*EmbedBatch
	batchMu       sync.RWMutex
	flushInterval time.Duration
	maxBatchSize  int

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Memory management
	maxTotalEmbeds int
	currentEmbeds  int
	embedsMu       sync.Mutex
}

// NewEmbedBatchSender creates embed batch sender with memory limits
func NewEmbedBatchSender(session *discordgo.Session) *EmbedBatchSender {
	ctx, cancel := context.WithCancel(context.Background())

	ebs := &EmbedBatchSender{
		session:        session,
		batches:        make(map[string]*EmbedBatch),
		flushInterval:  3 * time.Second,
		maxBatchSize:   5,                                  // Discord รองรับสูงสุด 10 ต่อข้อความ
		maxTotalEmbeds: getEnvInt("MAX_TOTAL_EMBEDS", 500), // จำกัด 500 embeds
		ctx:            ctx,
		cancel:         cancel,
	}

	ebs.wg.Add(1)
	go ebs.autoFlush()

	return ebs
}

// AddEmbed เพิ่ม embed เข้า batch พร้อมตรวจสอบ memory
func (ebs *EmbedBatchSender) AddEmbed(channelID string, embed *discordgo.MessageEmbed) {
	// ตรวจสอบ limit
	ebs.embedsMu.Lock()
	if ebs.currentEmbeds >= ebs.maxTotalEmbeds {
		ebs.embedsMu.Unlock()
		ebs.FlushAll()
		ebs.embedsMu.Lock()
	}
	ebs.currentEmbeds++
	ebs.embedsMu.Unlock()

	ebs.batchMu.Lock()
	batch, exists := ebs.batches[channelID]
	if !exists {
		batch = &EmbedBatch{
			ChannelID:   channelID,
			Embeds:      make([]*discordgo.MessageEmbed, 0, ebs.maxBatchSize),
			maxCapacity: ebs.maxBatchSize * 2,
		}
		ebs.batches[channelID] = batch
	}
	ebs.batchMu.Unlock()

	batch.mu.Lock()
	if len(batch.Embeds) >= batch.maxCapacity {
		batch.mu.Unlock()
		ebs.flushBatch(channelID)
		batch.mu.Lock()
	}

	batch.Embeds = append(batch.Embeds, embed)
	shouldFlush := len(batch.Embeds) >= ebs.maxBatchSize
	batch.mu.Unlock()

	if shouldFlush {
		ebs.flushBatch(channelID)
	}
}

// flushBatch ส่ง embeds ทั้งหมด
func (ebs *EmbedBatchSender) flushBatch(channelID string) {
	ebs.batchMu.RLock()
	batch, exists := ebs.batches[channelID]
	ebs.batchMu.RUnlock()

	if !exists {
		return
	}

	batch.mu.Lock()
	if len(batch.Embeds) == 0 {
		batch.mu.Unlock()
		return
	}

	embeds := make([]*discordgo.MessageEmbed, len(batch.Embeds))
	copy(embeds, batch.Embeds)
	embedCount := len(batch.Embeds)
	batch.Embeds = batch.Embeds[:0]
	batch.mu.Unlock()

	// อัพเดท counter
	ebs.embedsMu.Lock()
	ebs.currentEmbeds -= embedCount
	if ebs.currentEmbeds < 0 {
		ebs.currentEmbeds = 0
	}
	ebs.embedsMu.Unlock()

	// ส่ง embeds ทั้งหมดในข้อความเดียว (Discord รองรับสูงสุด 10 embeds)
	ebs.wg.Add(1)
	go func(embs []*discordgo.MessageEmbed) {
		defer ebs.wg.Done()

		// แบ่งเป็นกลุ่มๆ ละ 10 embeds
		for i := 0; i < len(embs); i += 10 {
			select {
			case <-ebs.ctx.Done():
				return
			default:
			}

			end := i + 10
			if end > len(embs) {
				end = len(embs)
			}

			_, err := ebs.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
				Embeds: embs[i:end],
			})

			if err != nil {
				log.Printf("❌ Error sending embed batch: %v", err)
			}

			time.Sleep(100 * time.Millisecond)
		}

		UpdateSharedActivity()
	}(embeds)
}

// autoFlush auto-flush embeds
func (ebs *EmbedBatchSender) autoFlush() {
	defer ebs.wg.Done()

	ticker := time.NewTicker(ebs.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ebs.ctx.Done():
			ebs.FlushAll()
			return
		case <-ticker.C:
			ebs.FlushAll()
		}
	}
}

// FlushAll flush all embed batches
func (ebs *EmbedBatchSender) FlushAll() {
	ebs.batchMu.RLock()
	channels := make([]string, 0, len(ebs.batches))
	for channelID := range ebs.batches {
		channels = append(channels, channelID)
	}
	ebs.batchMu.RUnlock()

	for _, channelID := range channels {
		ebs.flushBatch(channelID)
	}
}

// Stop stops embed batch sender
func (ebs *EmbedBatchSender) Stop() {
	ebs.cancel()
	ebs.wg.Wait()
}

// Global Embed Batch Sender
var GlobalEmbedBatchSender *EmbedBatchSender

// InitializeEmbedBatchSender initialize global embed batch sender
func InitializeEmbedBatchSender(session *discordgo.Session) {
	GlobalEmbedBatchSender = NewEmbedBatchSender(session)
	log.Println("🚀 Global Embed Batch Sender initialized")
}

// StopEmbedBatchSender หยุด embed batch sender
func StopEmbedBatchSender() {
	if GlobalEmbedBatchSender != nil {
		GlobalEmbedBatchSender.Stop()
	}
}
