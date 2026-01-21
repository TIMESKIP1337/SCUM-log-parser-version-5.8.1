package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// Shared configuration structure
type SharedConfig struct {
	DiscordToken string

	// Remote Configuration (FTP/SFTP)
	FTPHost        string
	FTPPort        string
	FTPUser        string
	FTPPassword    string
	RemoteLogsPath string
	Protocol       string // "FTP" or "SFTP"
	ConnectionType string // Set at runtime: "FTP", "SFTP", or ""

	// Local Configuration
	LocalLogsPath string

	// Discord Channel IDs for killfeed
	KillChannelID string

	// Discord Channel IDs for logs
	KillCountChannelID string
	LoginChannelID     string
	EconomyChannelID   string
	LockpickChannelID  string
	TrapChannelID      string

	// Discord Channel IDs for paste module
	LogsChannelIDs map[string]string

	// Discord Channel IDs for chat module
	ChatChannelID       string
	ChatGlobalChannelID string

	// Discord Channel IDs for admin module
	AdminChannelID       string
	AdminPublicChannelID string

	// Discord Channel ID for lockpicking rankings
	LockpickRankingChannelID string

	// Discord Channel IDs for ticket system
	TicketChannelID     string
	TicketThumbnailURL  string
	TicketImageURL      string
	TicketFooterIconURL string

	// General Settings
	CheckInterval         int
	KillfeedCheckInterval int // Dedicated interval for killfeed (faster)
	UseLocalFiles         bool
	UseRemote             bool // Replaces UseSFTP
	EnableFileWatcher     bool

	// Weapon Images (for killfeed)
	WeaponsFolder      string
	DefaultWeaponImage string

	// Cache Settings
	CacheCapacity int
	MaxMemoryMB   uint64
	MaxGoroutines int
}

// Global shared variables
var (
	SharedSession      *discordgo.Session
	SharedLastActivity = time.Now()
	SharedActivityMux  sync.RWMutex
)

// Shared configuration instance
var Config SharedConfig

func init() {
	// Enhanced UTF-8 support for different operating systems
	setupUTF8Environment()

	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	// Load shared configuration
	Config = SharedConfig{
		DiscordToken: getEnv("DISCORD_TOKEN", ""),

		// Remote Configuration
		FTPHost:        getEnv("FTP_HOST", ""),
		FTPPort:        getEnv("FTP_PORT", "22"),
		FTPUser:        getEnv("FTP_USER", ""),
		FTPPassword:    getEnv("FTP_PASSWORD", ""),
		RemoteLogsPath: getEnv("LOGS_PATH", "/79.127.225.146_7022/SaveFiles/Logs/"),
		Protocol:       getEnv("PROTOCOL", ""), // Can be "FTP" or "SFTP"

		// Local Configuration
		LocalLogsPath: getEnv("LOCAL_LOGS_PATH", "./logs/"),

		// Discord Channel IDs
		KillChannelID:      getEnv("KILL_CHANNEL_ID", "1362515256252960838"),
		KillCountChannelID: getEnv("KILL_COUNT_CHANNEL_ID", ""),
		LoginChannelID:     getEnv("LOGIN_CHANNEL_ID", ""),
		EconomyChannelID:   getEnv("ECONOMY_CHANNEL_ID", ""),
		LockpickChannelID:  getEnv("LOCKPICK_CHANNEL_ID", ""),
		TrapChannelID:      getEnv("TRAP_CHANNEL_ID", ""),

		// Paste module channel IDs
		LogsChannelIDs: map[string]string{
			"economy_name":        getEnv("LOGS_ECONOMY_NAME_CHANNEL_ID", "1395096670781571112"),
			"economy_bank":        getEnv("LOGS_ECONOMY_BANK_CHANNEL_ID", "1395096717166383126"),
			"vehicle_destruction": getEnv("LOGS_VEHICLE_DESTRUCTION_CHANNEL_ID", "1395096754722177034"),
			"chest_ownership":     getEnv("LOGS_CHEST_OWNERSHIP_CHANNEL_ID", "1395096789333315654"),
			"gameplay":            getEnv("LOGS_GAMEPLAY_CHANNEL_ID", "1395096836364177569"),
			"violations":          getEnv("LOGS_VIOLATIONS_CHANNEL_ID", "1395096879066386505"),
		},

		// Chat module channel IDs
		ChatChannelID:       getEnv("CHAT_CHANNEL_ID", "1366002560900530257"),
		ChatGlobalChannelID: getEnv("CHAT_GLOBAL_CHANNEL_ID", "1366002435231055892"),

		// Admin module channel IDs
		AdminChannelID:       getEnv("ADMIN_CHANNEL_ID", "1385489239386492928"),
		AdminPublicChannelID: getEnv("ADMIN_PUBLIC_CHANNEL_ID", "1385489112185700413"),

		// Lockpicking module channel ID
		LockpickRankingChannelID: getEnv("LOCKPICK_RANKING_CHANNEL_ID", "1388917389676118137"),

		// Ticket system configuration
		TicketChannelID:     getEnv("TICKET_CHANNEL_ID", ""),
		TicketThumbnailURL:  getEnv("TICKET_THUMBNAIL_URL", ""),
		TicketImageURL:      getEnv("TICKET_IMAGE_URL", ""),
		TicketFooterIconURL: getEnv("TICKET_FOOTER_ICON_URL", ""),

		// General Settings
		CheckInterval:         getEnvInt("CHECK_INTERVAL", 30),
		KillfeedCheckInterval: getEnvInt("KILLFEED_CHECK_INTERVAL", 2), // Faster interval for killfeed
		EnableFileWatcher:     getEnvBool("ENABLE_FILE_WATCHER", true),

		// Weapon Images
		WeaponsFolder:      getEnv("WEAPONS_FOLDER", "weapons_images"),
		DefaultWeaponImage: getEnv("DEFAULT_WEAPON_IMAGE", "https://cdn.discordapp.com/attachments/1311333990682333275/1362432259646423071/747d0b2e-5215-4c38-9f88-43ee15f40733-removebg-preview.png"),

		// Cache Settings - ปรับค่าเริ่มต้นให้เหมาะสม
		CacheCapacity: getEnvInt("CACHE_CAPACITY", 50000),
		MaxMemoryMB:   uint64(getEnvInt("MAX_MEMORY_MB", 2048)), // เพิ่มจาก 500 เป็น 2048 MB
		MaxGoroutines: getEnvInt("MAX_GOROUTINES", runtime.NumCPU()*2),
	}

	// Determine which mode to use
	Config.UseLocalFiles = Config.LocalLogsPath != ""

	// Initialize remote connections (will set UseRemote and ConnectionType)
	InitializeRemoteConnections()

	// Validate configuration
	if Config.DiscordToken == "" {
		log.Fatal("Missing required environment variable: DISCORD_TOKEN")
	}

	if !Config.UseRemote && !Config.UseLocalFiles {
		log.Fatal("Must configure either remote connection (FTP/SFTP) or LOCAL_LOGS_PATH")
	}

	// Create weapons folder if it doesn't exist
	if err := os.MkdirAll(Config.WeaponsFolder, 0755); err != nil {
		log.Printf("Warning: Could not create weapons folder: %v", err)
	}

	// Initialize new systems with UTF-8 support
	initializeNewSystems()

	// Log configuration
	logConfiguration()
}

// Setup UTF-8 environment for better international character support
func setupUTF8Environment() {
	// Set UTF-8 environment variables for better cross-platform support
	os.Setenv("LC_ALL", "en_US.UTF-8")
	os.Setenv("LANG", "en_US.UTF-8")

	// For Windows specifically
	if runtime.GOOS == "windows" {
		// Set console code page to UTF-8 if possible
		os.Setenv("PYTHONIOENCODING", "utf-8")
		os.Setenv("CHCP", "65001") // UTF-8 code page
	}

	// Verify UTF-8 support
	testString := "ทดสอบภาษาไทย 中文测试 🎉"
	if utf8.ValidString(testString) {
		log.Printf("✅ UTF-8 environment initialized successfully")
		log.Printf("🌍 Unicode test: %s", testString)
	} else {
		log.Printf("⚠️ UTF-8 environment setup incomplete")
	}
}

func initializeNewSystems() {
	// Initialize LRU Cache with UTF-8 support
	log.Printf("🔧 Initializing LRU Cache System with Unicode support...")
	InitializeSentLogsCache()

	// Initialize Resource Manager
	log.Printf("🔧 Initializing Resource Manager...")
	GlobalResourceManager = NewResourceManager(Config.MaxMemoryMB, Config.MaxGoroutines)

	// Initialize Buffer Pool with UTF-8 considerations
	log.Printf("🔧 Initializing Buffer Pool with UTF-8 support...")
	GlobalBufferPool = NewBufferPool(64 * 1024) // Increased to 64KB for better Unicode handling
}

func logConfiguration() {
	log.Printf("🔧 Main Configuration loaded with Enhanced UTF-8 Support:")
	log.Printf("   Remote Mode: %v (Protocol: %s)", Config.UseRemote, Config.ConnectionType)
	log.Printf("   Local Mode: %v", Config.UseLocalFiles)
	log.Printf("   File Watcher: %v", Config.EnableFileWatcher)
	log.Printf("   Check Interval: %d seconds", Config.CheckInterval)
	log.Printf("   Cache Capacity: %d entries", Config.CacheCapacity)
	log.Printf("   Max Memory: %d MB", Config.MaxMemoryMB)
	log.Printf("   Max Goroutines: %d", Config.MaxGoroutines)
	log.Printf("   🌍 UTF-8 Support: ENHANCED")
	log.Printf("   Kill Feed Channel ID: %s", Config.KillChannelID)
	log.Printf("   Kill Count Channel ID: %s", Config.KillCountChannelID)
	log.Printf("   Login Channel ID: %s", Config.LoginChannelID)
	log.Printf("   Economy Channel ID: %s", Config.EconomyChannelID)
	log.Printf("   Lockpick Channel ID: %s", Config.LockpickChannelID)
	log.Printf("   Chat Channel ID: %s", Config.ChatChannelID)
	log.Printf("   Admin Channel ID: %s", Config.AdminChannelID)
}

// UpdateSharedActivity updates the last activity timestamp
func UpdateSharedActivity() {
	SharedActivityMux.Lock()
	SharedLastActivity = time.Now()
	SharedActivityMux.Unlock()
}

// GetSharedLastActivity gets the last activity timestamp safely
func GetSharedLastActivity() time.Time {
	SharedActivityMux.RLock()
	defer SharedActivityMux.RUnlock()
	return SharedLastActivity
}

// SafeUTF8String ensures string is valid UTF-8
func SafeUTF8String(s string) string {
	if utf8.ValidString(s) {
		return s
	}

	// Convert invalid UTF-8 sequences to replacement character
	v := make([]rune, 0, len(s))
	for i, r := range s {
		if r == utf8.RuneError {
			_, size := utf8.DecodeRuneInString(s[i:])
			if size == 1 {
				continue // skip invalid byte
			}
		}
		v = append(v, r)
	}
	return string(v)
}

// Helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return strings.ToLower(value) == "true"
	}
	return defaultValue
}

// Module activity check functions
func isPasteModuleActive() bool {
	for _, id := range Config.LogsChannelIDs {
		if id != "" {
			return true
		}
	}
	return false
}

func isChatModuleActive() bool {
	return Config.ChatChannelID != "" || Config.ChatGlobalChannelID != ""
}

func isAdminModuleActive() bool {
	return Config.AdminChannelID != "" || Config.AdminPublicChannelID != ""
}

func isLockpickingModuleActive() bool {
	return Config.LockpickRankingChannelID != ""
}

func isTicketModuleActive() bool {
	return Config.TicketChannelID != ""
}

func isShowHideModuleActive() bool {
	return showHideConfig.TargetChannelID != ""
}

func isWelcomeModuleActive() bool {
	return welcomeConfig.WelcomeChannelID != "" || welcomeConfig.GoodbyeChannelID != ""
}

// Combined message handler
func combinedMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [MESSAGE HANDLER] Panic recovered: %v\n%s", r, debug.Stack())
		}
	}()

	// Check for nil to prevent panic
	if m.Author == nil {
		return
	}
	if s.State != nil && s.State.User != nil && m.Author.ID == s.State.User.ID {
		return
	}

	UpdateSharedActivity()

	// Ensure message content is UTF-8 safe
	m.Content = SafeUTF8String(m.Content)

	if Config.KillChannelID != "" {
		killFeedMessageCreate(s, m)
	}
	if isChatModuleActive() {
		chatMessageCreate(s, m)
	}
	if isAdminModuleActive() {
		adminMessageCreate(s, m)
	}
	if isLockpickingModuleActive() {
		lockpickingMessageCreate(s, m)
	}
	if isTicketModuleActive() {
		ticketMessageCreate(s, m)
	}
	if isShowHideModuleActive() {
		showHideMessageCreate(s, m)
	}
	if isWelcomeModuleActive() {
		welcomeMessageCreate(s, m)
	}
}

// Enhanced interaction handler for all modules
func combinedInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [INTERACTION HANDLER] Panic recovered: %v\n%s", r, debug.Stack())
		}
	}()

	UpdateSharedActivity()

	// Handle slash commands (like /setup-logs)
	if i.Type == discordgo.InteractionApplicationCommand {
		switch i.ApplicationCommandData().Name {
		case "setup-logs":
			HandleSetupCommand(s, i)
			return
		}
	}

	// Handle ticket interactions
	if isTicketModuleActive() {
		ticketInteractionCreate(s, i)
	}

	// Handle showhide interactions
	if isShowHideModuleActive() {
		if i.Type == discordgo.InteractionMessageComponent {
			data := i.MessageComponentData()
			if data.CustomID == "showhide_hide" {
				handleShowHideInteraction(s, i, false)
			} else if data.CustomID == "showhide_show" {
				handleShowHideInteraction(s, i, true)
			} else if data.CustomID == "showhide_settings" {
				handleShowHideSettings(s, i)
			} else if data.CustomID == "showhide_manage" {
				handleShowHideManage(s, i)
			}
		} else if i.Type == discordgo.InteractionModalSubmit {
			data := i.ModalSubmitData()
			if data.CustomID == "showhide_manage_modal" {
				handleShowHideManageModal(s, i)
			}
		}
	}
}

// Safe goroutine wrapper with panic recovery and UTF-8 logging
func safeGo(name string, fn func()) {
	GlobalResourceManager.AcquireGoroutine()
	go func() {
		defer GlobalResourceManager.ReleaseGoroutine()
		defer func() {
			if r := recover(); r != nil {
				// Ensure error message is UTF-8 safe
				errorMsg := SafeUTF8String(fmt.Sprintf("%v", r))
				log.Printf("❌ [%s] Panic recovered: %s\n%s", name, errorMsg, debug.Stack())
			}
		}()
		fn()
	}()
}

// heartbeat logs system status periodically with enhanced monitoring
func heartbeat(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastActivity := GetSharedLastActivity()
			elapsed := time.Since(lastActivity)

			var m runtime.MemStats
			runtime.ReadMemStats(&m)

			log.Printf("💓 Heartbeat: Last activity %v ago | Memory: %dMB | Goroutines: %d | GC: %d",
				elapsed.Round(time.Second),
				m.Alloc/1024/1024,
				runtime.NumGoroutine(),
				m.NumGC)

			// Log Memory Manager stats if available
			if GlobalMemoryManager != nil {
				stats := GlobalMemoryManager.GetStats()
				log.Printf("📊 Memory Manager: %.0fMB / %dMB (%.1f%%), Force GC: %d",
					float64(stats.CurrentMB), stats.MaxMB, stats.UsagePercent, stats.ForceGCCount)
			}

			// Log Batch Sender stats if available
			if GlobalBatchSender != nil {
				batchStats := GlobalBatchSender.GetStats()
				log.Printf("📊 Batch Sender: %d messages pending (%d batches, %.1f%% full)",
					batchStats.PendingMessages, batchStats.ActiveBatches, batchStats.UsagePercent)
			}

			// Log Cache stats if available
			if SentLogsCache != nil {
				cacheStats := SentLogsCache.Stats()
				log.Printf("📊 Cache: %d/%d items, Hit Rate: %.1f%%, Evictions: %d",
					cacheStats.Size, cacheStats.Capacity, cacheStats.HitRate, cacheStats.Evictions)
			}
		}
	}
}

// mainWatchdog monitors for issues and performs corrective actions
func mainWatchdog(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [MAIN WATCHDOG] Panic recovered: %v\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("✅ [MAIN WATCHDOG] Shutting down gracefully")
			return
		case <-ticker.C:
			// Check for excessive goroutines
			numGoroutines := runtime.NumGoroutine()
			if numGoroutines > Config.MaxGoroutines*2 {
				log.Printf("⚠️ [WATCHDOG] High goroutine count detected: %d (limit: %d)",
					numGoroutines, Config.MaxGoroutines)
			}

			// Check memory usage
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			currentMB := m.Alloc / 1024 / 1024

			if currentMB > Config.MaxMemoryMB*80/100 {
				log.Printf("⚠️ [WATCHDOG] High memory usage: %dMB (limit: %dMB)",
					currentMB, Config.MaxMemoryMB)
				// Force GC if memory is high
				runtime.GC()
			}

			// Check last activity
			lastActivity := GetSharedLastActivity()
			if time.Since(lastActivity) > 30*time.Minute {
				log.Printf("⚠️ [WATCHDOG] No activity for %v - system may be idle",
					time.Since(lastActivity).Round(time.Minute))
			}

			// Log cache stats
			if SentLogsCache != nil {
				stats := SentLogsCache.Stats()
				log.Printf("📊 [WATCHDOG] Cache Stats - Size: %d/%d, Hit Rate: %.2f%% 🌍",
					stats.Size, stats.Capacity, stats.HitRate)
			}
		}
	}
}

// Run starts the application with all modules
func Run() {
	log.Println("🚀 Starting Combined Discord Bot System with Memory Management...")

	// ==========================================
	// 1. Setup Context for Graceful Shutdown
	// ==========================================
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ==========================================
	// 2. Setup Signal Handler
	// ==========================================
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// ==========================================
	// 3. Initialize Memory Manager (CRITICAL - Must be first!)
	// ==========================================
	log.Printf("🧠 Initializing Memory Manager (Max: %d MB)...", Config.MaxMemoryMB)
	InitializeMemoryManager(Config.MaxMemoryMB)
	defer StopMemoryManager()

	// Set GC percentage for better memory management
	debug.SetGCPercent(50) // More aggressive GC
	log.Printf("♻️ GC configured with 50%% trigger threshold")

	// ==========================================
	// 4. Initialize Module Databases
	// ==========================================

	// Initialize kill feed module if kill channel is configured
	if Config.KillChannelID != "" {
		log.Println("🎯 Initializing Kill Feed module...")
		if err := InitializeKillFeed(); err != nil {
			log.Fatalf("Failed to initialize Kill Feed module: %v", err)
		}
	}

	// Initialize logs module if any log channels are configured
	if Config.KillCountChannelID != "" || Config.LoginChannelID != "" ||
		Config.EconomyChannelID != "" || Config.LockpickChannelID != "" {
		log.Println("📊 Initializing Logs module...")
		if err := InitializeLogs(); err != nil {
			log.Fatalf("Failed to initialize Logs module: %v", err)
		}
	}

	// Initialize paste module if any paste channels are configured
	if isPasteModuleActive() {
		log.Println("📝 Initializing Extended Logs module...")
		if err := InitializePaste(); err != nil {
			log.Fatalf("Failed to initialize Extended Logs module: %v", err)
		}
	}

	// Initialize chat module if any chat channels are configured
	if isChatModuleActive() {
		log.Println("💬 Initializing Chat module...")
		if err := InitializeChat(); err != nil {
			log.Fatalf("Failed to initialize Chat module: %v", err)
		}
	}

	// Initialize admin module if any admin channels are configured
	if isAdminModuleActive() {
		log.Println("👮 Initializing Admin module with UTF-8 support...")
		if err := InitializeAdmin(); err != nil {
			log.Fatalf("Failed to initialize Admin module: %v", err)
		}
	}

	// Initialize lockpicking module if lockpicking ranking channel is configured
	if isLockpickingModuleActive() {
		log.Println("🔓 Initializing Lockpicking module with Enhanced UTF-8 support...")
		if err := InitializeLockpicking(); err != nil {
			log.Fatalf("Failed to initialize Lockpicking module: %v", err)
		}
	}

	// Initialize ticket module if ticket channel is configured
	if isTicketModuleActive() {
		log.Println("🎫 Initializing Ticket module with UTF-8 support...")
		if err := InitializeTicket(); err != nil {
			log.Fatalf("Failed to initialize Ticket module: %v", err)
		}
	}

	// Initialize showhide module - must initialize first to load config
	log.Println("🔒 Initializing ShowHide module with UTF-8 support...")
	if err := InitializeShowHide(); err != nil {
		log.Printf("⚠️ ShowHide module initialization failed (will continue): %v", err)
	}

	// Initialize welcome module - must initialize first to load config
	log.Println("👋 Initializing Welcome module with UTF-8 support...")
	if err := InitializeWelcome(); err != nil {
		log.Printf("⚠️ Welcome module initialization failed (will continue): %v", err)
	}

	// ==========================================
	// 5. Create Discord Session
	// ==========================================
	var err error
	SharedSession, err = discordgo.New("Bot " + Config.DiscordToken)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	// Set required intents for all modules
	SharedSession.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsMessageContent

	// ==========================================
	// 6. Initialize Batch Senders (AFTER Discord session)
	// ==========================================
	log.Println("📦 Initializing Batch Senders...")
	InitializeBatchSender(SharedSession)
	InitializeEmbedBatchSender(SharedSession)

	// ==========================================
	// 7. Add Event Handlers
	// ==========================================
	SharedSession.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		userName := SafeUTF8String(r.User.Username)
		log.Printf("🤖 Combined Discord Bot logged in as %s 🌍", userName)
		UpdateSharedActivity()
	})

	SharedSession.AddHandler(func(s *discordgo.Session, d *discordgo.Disconnect) {
		log.Printf("⚠️ Discord disconnected")
		UpdateSharedActivity()
	})

	SharedSession.AddHandler(func(s *discordgo.Session, r *discordgo.Resumed) {
		log.Printf("✅ Discord connection resumed")
		UpdateSharedActivity()
	})

	SharedSession.AddHandler(combinedMessageCreate)
	SharedSession.AddHandler(combinedInteractionCreate)

	// Add welcome/goodbye handlers
	if isWelcomeModuleActive() {
		SharedSession.AddHandler(handleMemberJoin)
		SharedSession.AddHandler(handleMemberRemove)
	}

	// ==========================================
	// 8. Open Discord Connection
	// ==========================================
	log.Println("🔌 Opening Discord connection...")
	err = SharedSession.Open()
	if err != nil {
		log.Fatalf("Error opening Discord session: %v", err)
	}
	defer SharedSession.Close()

	// ==========================================
	// 8.5 Register Slash Commands
	// ==========================================
	if guildID := os.Getenv("GUILD_ID"); guildID != "" {
		log.Println("📝 Registering slash commands...")
		if err := RegisterSetupCommand(SharedSession, guildID); err != nil {
			log.Printf("⚠️ Failed to register /setup command: %v", err)
		} else {
			log.Printf("✅ /setup command registered for guild %s", guildID)
		}
	} else {
		log.Printf("⚠️ GUILD_ID not set in .env - /setup command not registered")
	}

	// ==========================================
	// 9. Start Background Services
	// ==========================================

	// Start cache auto-save
	safeGo("CACHE-AUTOSAVE", func() { StartCacheAutoSave(ctx) })

	// Start heartbeat
	safeGo("HEARTBEAT", func() { heartbeat(ctx) })

	// Start main watchdog
	safeGo("WATCHDOG", func() { mainWatchdog(ctx) })

	// ==========================================
	// 10. Start Module Workers with Unified Scheduler
	// ==========================================

	// Use Unified Scheduler for TRUE parallel processing of all log modules
	log.Println("🚀 Starting Unified Log Scheduler (TRUE parallel processing)...")
	scheduler := NewUnifiedLogScheduler(ctx)
	scheduler.Start()

	if isTicketModuleActive() {
		log.Println("🎫 Starting Ticket module...")
		safeGo("TICKET", func() { StartTicket(ctx, SharedSession) })
	}

	if isShowHideModuleActive() {
		log.Println("🔒 Starting ShowHide module...")
		safeGo("SHOWHIDE", func() { StartShowHide(ctx, SharedSession) })
	}

	if isWelcomeModuleActive() {
		log.Println("👋 Starting Welcome module...")
		safeGo("WELCOME", func() { StartWelcome(ctx, SharedSession) })
	}

	// ==========================================
	// 11. Log Active Modules
	// ==========================================
	log.Printf("✅ Combined Discord Bot System is running with Enhanced Memory Management!")
	logActiveModules()

	// ==========================================
	// 12. Wait for Shutdown Signal
	// ==========================================
	<-sigChan
	log.Println("\n🛑 Shutdown signal received! Starting graceful shutdown...")

	// ==========================================
	// 13. Graceful Shutdown with Timeout
	// ==========================================
	shutdownWithTimeout(ctx, cancel)
}

// shutdownWithTimeout handles graceful shutdown with a timeout
func shutdownWithTimeout(ctx context.Context, cancel context.CancelFunc) {
	log.Println("🔄 Starting graceful shutdown sequence...")

	// Create shutdown context with 30 second timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Signal all goroutines to stop
	cancel()

	// Wait for cleanup with timeout
	done := make(chan struct{})
	go func() {
		performCleanup()
		close(done)
	}()

	select {
	case <-done:
		log.Println("✅ Graceful shutdown completed successfully")
	case <-shutdownCtx.Done():
		log.Println("⚠️ Shutdown timeout reached, forcing exit")
	}
}

// performCleanup performs all cleanup tasks in order
func performCleanup() {
	log.Println("🧹 Performing cleanup tasks...")

	// 1. Stop batch senders first (flush pending messages)
	log.Println("  → Stopping batch senders and flushing messages...")
	StopBatchSender()
	StopEmbedBatchSender()

	// 2. Save cache to disk
	log.Println("  → Saving cache to disk...")
	if err := SaveSentLogsCache(); err != nil {
		log.Printf("⚠️ Error saving cache: %v", err)
	}

	// 3. Close remote connections
	log.Println("  → Closing remote connections...")
	CloseRemoteConnections()

	// 4. Close databases
	if isLockpickingModuleActive() {
		log.Println("  → Closing lockpicking database...")
		CloseLockpickingDatabase()
	}

	// 5. Stop memory manager
	log.Println("  → Stopping memory manager...")
	StopMemoryManager()

	// 6. Force final garbage collection
	log.Println("  → Running final garbage collection...")
	runtime.GC()
	debug.FreeOSMemory()

	// Log final stats
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("📊 Final Stats: Memory: %dMB, Goroutines: %d, GC Runs: %d",
		m.Alloc/1024/1024, runtime.NumGoroutine(), m.NumGC)

	log.Println("✅ All cleanup tasks completed")
}

func logActiveModules() {
	if Config.KillChannelID != "" {
		log.Printf("   ⚔️ Kill Feed Module: ACTIVE (Channel: %s)", Config.KillChannelID)
	}
	if Config.KillCountChannelID != "" {
		log.Printf("   📊 Kill Count Module: ACTIVE (Channel: %s)", Config.KillCountChannelID)
	}
	if Config.LoginChannelID != "" {
		log.Printf("   🚪 Login Module: ACTIVE (Channel: %s)", Config.LoginChannelID)
	}
	if Config.EconomyChannelID != "" {
		log.Printf("   💰 Economy Module: ACTIVE (Channel: %s)", Config.EconomyChannelID)
	}
	if Config.LockpickChannelID != "" {
		log.Printf("   🔑 Lockpick Module: ACTIVE (Channel: %s)", Config.LockpickChannelID)
	}
	if Config.ChatChannelID != "" {
		log.Printf("   💬 Chat Module: ACTIVE (Channel: %s)", Config.ChatChannelID)
	}
	if Config.ChatGlobalChannelID != "" {
		log.Printf("   🌍 Chat Global Module: ACTIVE (Channel: %s)", Config.ChatGlobalChannelID)
	}
	if Config.AdminChannelID != "" {
		log.Printf("   👮 Admin Module: ACTIVE (Channel: %s)", Config.AdminChannelID)
	}
	if Config.AdminPublicChannelID != "" {
		log.Printf("   👥 Admin Public Module: ACTIVE (Channel: %s)", Config.AdminPublicChannelID)
	}
	if Config.LockpickRankingChannelID != "" {
		log.Printf("   🏆 Lockpicking Rankings Module: ACTIVE with Enhanced UTF-8 (Channel: %s)", Config.LockpickRankingChannelID)
	}
	if Config.TicketChannelID != "" {
		log.Printf("   🎫 Ticket Module: ACTIVE (Channel: %s)", Config.TicketChannelID)
		if Config.TicketThumbnailURL != "" || Config.TicketImageURL != "" || Config.TicketFooterIconURL != "" {
			log.Printf("      └─ Custom images configured ✅")
		}
	}

	// ShowHide module
	if showHideConfig.TargetChannelID != "" {
		log.Printf("   🔒 ShowHide Module: ACTIVE")
		log.Printf("      └─ Target Channel: %s", showHideConfig.TargetChannelID)
		log.Printf("      └─ Managing %d channels", len(showHideConfig.ChannelIDs))
	}

	// Welcome module
	if welcomeConfig.WelcomeChannelID != "" || welcomeConfig.GoodbyeChannelID != "" {
		log.Printf("   👋 Welcome Module: ACTIVE")
		if welcomeConfig.WelcomeChannelID != "" {
			log.Printf("      └─ Welcome Channel: %s", welcomeConfig.WelcomeChannelID)
		}
		if welcomeConfig.GoodbyeChannelID != "" {
			log.Printf("      └─ Goodbye Channel: %s", welcomeConfig.GoodbyeChannelID)
		}
	}

	pasteActive := false
	for name, id := range Config.LogsChannelIDs {
		if id != "" {
			if !pasteActive {
				log.Printf("   📝 Extended Logs Module:")
				pasteActive = true
			}
			log.Printf("      - %s: ACTIVE (Channel: %s)", name, id)
		}
	}

	if Config.UseRemote {
		log.Printf("   📡 Remote Mode: ACTIVE (Protocol: %s)", Config.ConnectionType)
		log.Printf("   🌐 Remote Server: %s:%s", Config.FTPHost, Config.FTPPort)
		log.Printf("   📂 Remote Path: %s", Config.RemoteLogsPath)
		log.Printf("   📄 Check Interval: %d seconds", Config.CheckInterval)
	}
	if Config.UseLocalFiles {
		log.Printf("   📁 Local Mode: ACTIVE (%s)", Config.LocalLogsPath)
		if Config.EnableFileWatcher {
			log.Printf("   👁️ File Watcher: ENABLED")
		}
	}

	log.Printf("   🚀 Performance Mode: ENABLED")
	log.Printf("   💾 Cache System: ACTIVE (Capacity: %d)", Config.CacheCapacity)
	log.Printf("   🧵 Resource Manager: ACTIVE (Max Goroutines: %d)", Config.MaxGoroutines)
	log.Printf("   🧠 Memory Manager: ACTIVE (Max: %d MB)", Config.MaxMemoryMB)
	log.Printf("   ♻️ GC Mode: Aggressive (50%% trigger)")
	log.Printf("   🗄️ SQLite Database: ENABLED with UTF-8 Collation")
	log.Printf("   🌍 UTF-8 Support: ENHANCED for International Characters")
	if Config.TrapChannelID != "" {
		log.Printf("   💣 Trap Log Module: ACTIVE (Channel: %s)", Config.TrapChannelID)
	}
	log.Println("   Press Ctrl+C to exit gracefully.")
}
