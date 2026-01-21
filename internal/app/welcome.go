package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Welcome module specific variables
var (
	welcomeConfig WelcomeConfig
)

// Note: SafeUTF8String is defined in main.go and used globally

// WelcomeConfig represents the welcome.json structure
type WelcomeConfig struct {
	WelcomeChannelID string `json:"welcome_channel_id"`
	GoodbyeChannelID string `json:"goodbye_channel_id"`
	ImageURL         string `json:"image_url"`
	FooterIconURL    string `json:"footer_icon_url"`

	// Welcome settings
	WelcomeTitle       string `json:"welcome_title"`
	WelcomeDescription string `json:"welcome_description"`
	WelcomeMessage     string `json:"welcome_message"`
	WelcomeColor       string `json:"welcome_color"` // "random" or hex color

	// Goodbye settings
	GoodbyeTitle       string `json:"goodbye_title"`
	GoodbyeDescription string `json:"goodbye_description"`
	GoodbyeMessage     string `json:"goodbye_message"`
	GoodbyeColor       int    `json:"goodbye_color"`

	// Field labels
	CreatedAtLabel string `json:"created_at_label"`
	JoinedAtLabel  string `json:"joined_at_label"`
	UserLabel      string `json:"user_label"`

	// Footer
	FooterText string `json:"footer_text"`
}

// InitializeWelcome initializes the welcome/goodbye system
func InitializeWelcome() error {
	// Load configuration from JSON
	config, err := loadWelcomeJSON()
	if err != nil {
		log.Printf("⚠️ Warning: Could not load welcome.json: %v", err)
		log.Println("📝 Creating default welcome.json...")
		if err := createDefaultWelcomeJSON(); err != nil {
			log.Printf("❌ Failed to create default welcome.json: %v", err)
			return err
		}
		// Try loading again
		config, err = loadWelcomeJSON()
		if err != nil {
			return fmt.Errorf("failed to load welcome configuration: %v", err)
		}
	}

	welcomeConfig = *config

	log.Printf("✅ Welcome system initialized")
	if welcomeConfig.WelcomeChannelID != "" {
		log.Printf("   Welcome Channel: %s", welcomeConfig.WelcomeChannelID)
	}
	if welcomeConfig.GoodbyeChannelID != "" {
		log.Printf("   Goodbye Channel: %s", welcomeConfig.GoodbyeChannelID)
	}

	return nil
}

// Load welcome configuration from JSON file
func loadWelcomeJSON() (*WelcomeConfig, error) {
	data, err := ioutil.ReadFile("welcome.json")
	if err != nil {
		return nil, err
	}

	// Ensure data is valid UTF-8
	dataStr := SafeUTF8String(string(data))
	data = []byte(dataStr)

	var config WelcomeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// Sanitize all string fields for UTF-8
	config.WelcomeTitle = SafeUTF8String(config.WelcomeTitle)
	config.WelcomeDescription = SafeUTF8String(config.WelcomeDescription)
	config.WelcomeMessage = SafeUTF8String(config.WelcomeMessage)
	config.GoodbyeTitle = SafeUTF8String(config.GoodbyeTitle)
	config.GoodbyeDescription = SafeUTF8String(config.GoodbyeDescription)
	config.GoodbyeMessage = SafeUTF8String(config.GoodbyeMessage)
	config.CreatedAtLabel = SafeUTF8String(config.CreatedAtLabel)
	config.JoinedAtLabel = SafeUTF8String(config.JoinedAtLabel)
	config.UserLabel = SafeUTF8String(config.UserLabel)
	config.FooterText = SafeUTF8String(config.FooterText)

	// Set defaults if not provided
	if config.WelcomeTitle == "" {
		config.WelcomeTitle = "Welcome"
	}
	if config.WelcomeColor == "" {
		config.WelcomeColor = "random"
	}
	if config.GoodbyeTitle == "" {
		config.GoodbyeTitle = "Goodbye"
	}
	if config.GoodbyeColor == 0 {
		config.GoodbyeColor = 0xFF0000
	}
	if config.FooterText == "" {
		config.FooterText = "© powered by TimeSkip"
	}
	if config.CreatedAtLabel == "" {
		config.CreatedAtLabel = "📅 สร้างบัญชีเมื่อ"
	}
	if config.JoinedAtLabel == "" {
		config.JoinedAtLabel = "🗓️ เข้าดิสเมื่อ"
	}
	if config.UserLabel == "" {
		config.UserLabel = "ผู้ใช้"
	}

	return &config, nil
}

// Create default welcome.json file
func createDefaultWelcomeJSON() error {
	defaultConfig := WelcomeConfig{
		WelcomeChannelID: "YOUR_WELCOME_CHANNEL_ID",
		GoodbyeChannelID: "YOUR_GOODBYE_CHANNEL_ID",
		ImageURL:         "https://cdn.discordapp.com/attachments/1347264410087067709/1364553843316363304/raw.png",
		FooterIconURL:    "https://cdn.discordapp.com/attachments/1347264410087067709/1364554397396504676/747d0b2e-5215-4c38-9f88-43ee15f40733-removebg-preview.png",

		WelcomeTitle:       "Welcome",
		WelcomeDescription: "สวัสดีจ้าาา! ยินดีต้อนรับเข้าสู่เซิร์ฟเวอร์ SCUM D.M.A_Loader 🥰",
		WelcomeMessage:     "ยินดีต้อนรับ {mention} เข้าสู่เซิร์ฟเวอร์ SCUM D.M.A_Loader!",
		WelcomeColor:       "random",

		GoodbyeTitle:       "Goodbye",
		GoodbyeDescription: "ถ้ามีโอกาสหน้าเอาไว้เจอกันใหม่นะครับ 🥰",
		GoodbyeMessage:     "{mention} ได้ออกจากเซิร์ฟเวอร์แล้ว!",
		GoodbyeColor:       0xFF0000,

		CreatedAtLabel: "📅 สร้างบัญชีเมื่อ",
		JoinedAtLabel:  "🗓️ เข้าดิสเมื่อ",
		UserLabel:      "ผู้ใช้",
		FooterText:     "© powered by TimeSkip",
	}

	data, err := json.MarshalIndent(defaultConfig, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile("welcome.json", data, 0644)
}

// StartWelcome starts the welcome/goodbye system
func StartWelcome(ctx context.Context, session *discordgo.Session) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [WELCOME START] Panic recovered: %v\n%s", r, debug.Stack())
		}
	}()

	log.Printf("✅ Welcome/Goodbye system started!")
}

// Handle member join event
func handleMemberJoin(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [MEMBER JOIN] Panic recovered: %v", r)
		}
	}()

	if welcomeConfig.WelcomeChannelID == "" {
		return
	}

	channel, err := s.Channel(welcomeConfig.WelcomeChannelID)
	if err != nil {
		log.Printf("Error getting welcome channel: %v", err)
		return
	}

	// Get avatar URL
	avatarURL := m.User.AvatarURL("256")
	if avatarURL == "" {
		// Use default avatar based on discriminator
		avatarURL = fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", rand.Intn(5))
	}

	// Determine color
	var color int
	if welcomeConfig.WelcomeColor == "random" {
		color = rand.Intn(0xFFFFFF)
	} else {
		// Parse hex color (you can implement hex parsing if needed)
		color = 0x3498db // default blue
	}

	// Parse user ID to int64 for timestamp calculation
	userIDInt, err := strconv.ParseInt(m.User.ID, 10, 64)
	if err != nil {
		userIDInt = 0
	}

	// Calculate creation timestamp (Discord Snowflake)
	createdTimestamp := int64(0)
	if userIDInt > 0 {
		createdTimestamp = ((userIDInt >> 22) + 1420070400000) / 1000
	}

	// Create welcome embed
	embed := &discordgo.MessageEmbed{
		Title:       SafeUTF8String(welcomeConfig.WelcomeTitle),
		Description: SafeUTF8String(welcomeConfig.WelcomeDescription),
		Color:       color,
		Thumbnail: &discordgo.MessageEmbedThumbnail{
			URL: avatarURL,
		},
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   SafeUTF8String(welcomeConfig.CreatedAtLabel),
				Value:  fmt.Sprintf("<t:%d:F>", createdTimestamp),
				Inline: true,
			},
			{
				Name:   SafeUTF8String(welcomeConfig.JoinedAtLabel),
				Value:  fmt.Sprintf("<t:%d:F>", time.Now().Unix()),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: SafeUTF8String(welcomeConfig.FooterText),
		},
	}

	// Add image if configured
	if welcomeConfig.ImageURL != "" {
		embed.Image = &discordgo.MessageEmbedImage{
			URL: welcomeConfig.ImageURL,
		}
	}

	// Add footer icon if configured
	if welcomeConfig.FooterIconURL != "" {
		embed.Footer.IconURL = welcomeConfig.FooterIconURL
	}

	// Replace {mention} placeholder in welcome message
	welcomeMsg := strings.ReplaceAll(welcomeConfig.WelcomeMessage, "{mention}", m.User.Mention())
	welcomeMsg = SafeUTF8String(welcomeMsg)

	_, err = s.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Content: welcomeMsg,
		Embed:   embed,
	})

	if err != nil {
		log.Printf("Error sending welcome message: %v", err)
	} else {
		log.Printf("✅ Sent welcome message for user: %s (%s)", m.User.Username, m.User.ID)
	}
}

// Handle member remove event
func handleMemberRemove(s *discordgo.Session, m *discordgo.GuildMemberRemove) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [MEMBER REMOVE] Panic recovered: %v", r)
		}
	}()

	if welcomeConfig.GoodbyeChannelID == "" {
		return
	}

	channel, err := s.Channel(welcomeConfig.GoodbyeChannelID)
	if err != nil {
		log.Printf("Error getting goodbye channel: %v", err)
		return
	}

	// Get user info
	userID := m.User.ID
	userName := m.User.Username
	userMention := m.User.Mention()

	// Get avatar URL
	avatarURL := m.User.AvatarURL("256")
	if avatarURL == "" {
		// Use default avatar based on discriminator
		avatarURL = fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", rand.Intn(5))
	}

	// Create goodbye embed
	embed := &discordgo.MessageEmbed{
		Title:       SafeUTF8String(welcomeConfig.GoodbyeTitle),
		Description: SafeUTF8String(welcomeConfig.GoodbyeDescription),
		Color:       welcomeConfig.GoodbyeColor,
		Thumbnail: &discordgo.MessageEmbedThumbnail{
			URL: avatarURL,
		},
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   SafeUTF8String(welcomeConfig.UserLabel),
				Value:  SafeUTF8String(userName),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: SafeUTF8String(welcomeConfig.FooterText),
		},
	}

	// Add image if configured
	if welcomeConfig.ImageURL != "" {
		embed.Image = &discordgo.MessageEmbedImage{
			URL: welcomeConfig.ImageURL,
		}
	}

	// Add footer icon if configured
	if welcomeConfig.FooterIconURL != "" {
		embed.Footer.IconURL = welcomeConfig.FooterIconURL
	}

	// Replace {mention} placeholder in goodbye message
	goodbyeMsg := strings.ReplaceAll(welcomeConfig.GoodbyeMessage, "{mention}", userMention)
	goodbyeMsg = SafeUTF8String(goodbyeMsg)

	_, err = s.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Content: goodbyeMsg,
		Embed:   embed,
	})

	if err != nil {
		log.Printf("Error sending goodbye message: %v", err)
	} else {
		log.Printf("✅ Sent goodbye message for user: %s (ID: %s)", userName, userID)
	}
}

// Handle reload welcome config command
func handleReloadWelcomeCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Check if user has admin permissions
	perms, err := s.UserChannelPermissions(m.Author.ID, m.ChannelID)
	if err != nil {
		return
	}

	hasPermission := false
	if perms&discordgo.PermissionAdministrator != 0 || perms&discordgo.PermissionManageServer != 0 {
		hasPermission = true
	}

	if !hasPermission {
		s.ChannelMessageSend(m.ChannelID, "❌ คุณไม่มีสิทธิ์ใช้คำสั่งนี้ (ต้องการสิทธิ์ Admin)")
		return
	}

	// Reload welcome.json
	config, err := loadWelcomeJSON()
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ ไม่สามารถโหลด welcome.json: %v", err))
		log.Printf("❌ Failed to reload welcome.json: %v", err)
		return
	}

	// Update config
	welcomeConfig = *config

	// Send success message with embed
	reloadEmbed := &discordgo.MessageEmbed{
		Title: "✅ โหลด welcome.json ใหม่สำเร็จ",
		Description: fmt.Sprintf("โหลดการตั้งค่าใหม่เรียบร้อยแล้ว\n\n**Welcome Channel:** <#%s>\n**Goodbye Channel:** <#%s>",
			welcomeConfig.WelcomeChannelID,
			welcomeConfig.GoodbyeChannelID),
		Color:     0x00ff00,
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("โหลดโดย %s", m.Author.Username),
		},
	}

	s.ChannelMessageSendEmbed(m.ChannelID, reloadEmbed)
	log.Printf("✅ Welcome configuration reloaded by %s", m.Author.Username)
}

// Welcome message handler
func welcomeMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [WELCOME MESSAGE] Panic recovered: %v", r)
		}
	}()

	if m.Author.Bot {
		return
	}

	// Handle reload command
	if m.Content == "!reload_welcome" {
		handleReloadWelcomeCommand(s, m)
	}
}
