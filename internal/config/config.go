package config

import (
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// Config holds the shared configuration for the application
type Config struct {
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
	CheckInterval     int
	UseLocalFiles     bool
	UseRemote         bool // Replaces UseSFTP
	EnableFileWatcher bool

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

// Global instance
var AppConfig *Config

// Load initializes and returns the application configuration
func Load() *Config {
	// Enhanced UTF-8 support for different operating systems
	setupUTF8Environment()

	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	// Load configuration
	AppConfig = &Config{
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
		CheckInterval:     getEnvInt("CHECK_INTERVAL", 30),
		EnableFileWatcher: getEnvBool("ENABLE_FILE_WATCHER", true),

		// Weapon Images
		WeaponsFolder:      getEnv("WEAPONS_FOLDER", "weapons_images"),
		DefaultWeaponImage: getEnv("DEFAULT_WEAPON_IMAGE", "https://cdn.discordapp.com/attachments/1311333990682333275/1362432259646423071/747d0b2e-5215-4c38-9f88-43ee15f40733-removebg-preview.png"),

		// Cache Settings
		CacheCapacity: getEnvInt("CACHE_CAPACITY", 50000),
		MaxMemoryMB:   uint64(getEnvInt("MAX_MEMORY_MB", 2048)),
		MaxGoroutines: getEnvInt("MAX_GOROUTINES", runtime.NumCPU()*2),
	}

	// Determine which mode to use
	AppConfig.UseLocalFiles = AppConfig.LocalLogsPath != ""

	// Validate configuration
	if AppConfig.DiscordToken == "" {
		log.Fatal("Missing required environment variable: DISCORD_TOKEN")
	}

	// Create weapons folder if it doesn't exist
	if err := os.MkdirAll(AppConfig.WeaponsFolder, 0755); err != nil {
		log.Printf("Warning: Could not create weapons folder: %v", err)
	}

	// Log configuration
	logConfiguration()

	return AppConfig
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

func logConfiguration() {
	log.Printf("🔧 Main Configuration loaded with Enhanced UTF-8 Support:")
	log.Printf("   Remote Mode: %v (Protocol: %s)", AppConfig.UseRemote, AppConfig.ConnectionType)
	log.Printf("   Local Mode: %v", AppConfig.UseLocalFiles)
	log.Printf("   File Watcher: %v", AppConfig.EnableFileWatcher)
	log.Printf("   Check Interval: %d seconds", AppConfig.CheckInterval)
	log.Printf("   Cache Capacity: %d entries", AppConfig.CacheCapacity)
	log.Printf("   Max Memory: %d MB", AppConfig.MaxMemoryMB)
	log.Printf("   Max Goroutines: %d", AppConfig.MaxGoroutines)
	log.Printf("   🌍 UTF-8 Support: ENHANCED")
	log.Printf("   Kill Feed Channel ID: %s", AppConfig.KillChannelID)
	log.Printf("   Kill Count Channel ID: %s", AppConfig.KillCountChannelID)
	log.Printf("   Login Channel ID: %s", AppConfig.LoginChannelID)
	log.Printf("   Economy Channel ID: %s", AppConfig.EconomyChannelID)
	log.Printf("   Lockpick Channel ID: %s", AppConfig.LockpickChannelID)
	log.Printf("   Chat Channel ID: %s", AppConfig.ChatChannelID)
	log.Printf("   Admin Channel ID: %s", AppConfig.AdminChannelID)
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
func (c *Config) IsPasteModuleActive() bool {
	for _, id := range c.LogsChannelIDs {
		if id != "" {
			return true
		}
	}
	return false
}

func (c *Config) IsChatModuleActive() bool {
	return c.ChatChannelID != "" || c.ChatGlobalChannelID != ""
}

func (c *Config) IsAdminModuleActive() bool {
	return c.AdminChannelID != "" || c.AdminPublicChannelID != ""
}

func (c *Config) IsLockpickingModuleActive() bool {
	return c.LockpickRankingChannelID != ""
}

func (c *Config) IsTicketModuleActive() bool {
	return c.TicketChannelID != ""
}
