package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Ticket module specific variables
var (
	ticketMessages         = make(map[string][]TicketMessage)
	ticketMessagesMux      sync.RWMutex
	ticketCreationLocks    = make(map[string]*sync.Mutex)
	ticketCreationLocksMux sync.RWMutex
	activeTickets          = make(map[string]string) // userID -> channelID
	activeTicketsMux       sync.RWMutex
)

// Get or create a mutex for a specific user
func getUserTicketLock(userID string) *sync.Mutex {
	ticketCreationLocksMux.Lock()
	defer ticketCreationLocksMux.Unlock()

	if _, exists := ticketCreationLocks[userID]; !exists {
		ticketCreationLocks[userID] = &sync.Mutex{}
	}
	return ticketCreationLocks[userID]
}

// Check if user has active ticket in our tracking
func hasActiveTicket(userID string) (bool, string) {
	activeTicketsMux.RLock()
	defer activeTicketsMux.RUnlock()

	if channelID, exists := activeTickets[userID]; exists {
		return true, channelID
	}
	return false, ""
}

// Mark ticket as active
func markTicketActive(userID, channelID string) {
	activeTicketsMux.Lock()
	defer activeTicketsMux.Unlock()

	activeTickets[userID] = channelID
	log.Printf("🔵 [TICKET] Marked ticket active for user %s -> channel %s", userID, channelID)
}

// Remove ticket from active tracking
func removeActiveTicket(userID string) {
	activeTicketsMux.Lock()
	defer activeTicketsMux.Unlock()

	if channelID, exists := activeTickets[userID]; exists {
		delete(activeTickets, userID)
		log.Printf("🔴 [TICKET] Removed active ticket for user %s (channel %s)", userID, channelID)
	}
}

// TicketMessage represents a message in a ticket channel
type TicketMessage struct {
	Author            string   `json:"author"`
	AuthorDiscordName string   `json:"author_discord_name"`
	AuthorID          string   `json:"author_id"`
	Content           string   `json:"content"`
	Timestamp         string   `json:"timestamp"`
	Attachments       []string `json:"attachments,omitempty"`
}

// TicketSection represents a ticket type configuration
type TicketSection struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Description    string `json:"description"`
	Emoji          string `json:"emoji"`
	Category       string `json:"category"`
	Color          int    `json:"color"`
	WelcomeMessage string `json:"welcome_message"`
}

// DonatePackage represents a donation package
type DonatePackage struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DonateInfo represents donate information configuration
type DonateInfo struct {
	Title       string          `json:"title"`
	Description string          `json:"description"`
	ImageURL    string          `json:"image_url"`
	Packages    []DonatePackage `json:"packages"`
	Note        string          `json:"note"`
	Contact     string          `json:"contact"`
}

// TicketSettings represents global ticket settings
type TicketSettings struct {
	MaxOpenTicketsPerUser int        `json:"max_open_tickets_per_user"`
	AutoCloseAfterHours   int        `json:"auto_close_after_hours"`
	RequireAdminToClose   bool       `json:"require_admin_to_close"`
	SaveTranscripts       bool       `json:"save_transcripts"`
	AdminRoleName         string     `json:"admin_role_name"`
	DonateInfo            DonateInfo `json:"donate_info"`
}

// TicketJSONConfig represents the ticket.json structure
type TicketJSONConfig struct {
	Sections []TicketSection `json:"sections"`
	Settings TicketSettings  `json:"settings"`
}

// TicketConfig holds ticket system configuration
type TicketConfig struct {
	TicketChannelID string
	ThumbnailURL    string
	ImageURL        string
	FooterIconURL   string
	Sections        []TicketSection
	Settings        TicketSettings
}

var ticketConfig TicketConfig

// InitializeTicket initializes the ticket system
func InitializeTicket() error {
	// Load ticket configuration from JSON
	jsonConfig, err := loadTicketJSON()
	if err != nil {
		log.Printf("⚠️ Warning: Could not load ticket.json: %v", err)
		log.Println("📝 Creating default ticket.json...")
		if err := createDefaultTicketJSON(); err != nil {
			log.Printf("❌ Failed to create default ticket.json: %v", err)
			return err
		}
		// Try loading again
		jsonConfig, err = loadTicketJSON()
		if err != nil {
			return fmt.Errorf("failed to load ticket configuration: %v", err)
		}
	}

	// Use Config from main.go
	ticketConfig = TicketConfig{
		TicketChannelID: Config.TicketChannelID,
		ThumbnailURL:    Config.TicketThumbnailURL,
		ImageURL:        Config.TicketImageURL,
		FooterIconURL:   Config.TicketFooterIconURL,
		Sections:        jsonConfig.Sections,
		Settings:        jsonConfig.Settings,
	}

	// Create history folders for each section
	for _, section := range ticketConfig.Sections {
		folderName := fmt.Sprintf("%s_history", section.ID)
		if err := os.MkdirAll(folderName, 0755); err != nil {
			log.Printf("Warning: Could not create folder %s: %v", folderName, err)
		}
	}

	// Create attachments folder
	if err := os.MkdirAll("attachments", 0755); err != nil {
		log.Printf("Warning: Could not create attachments folder: %v", err)
	}

	log.Printf("✅ Ticket system initialized with %d sections", len(ticketConfig.Sections))
	for _, section := range ticketConfig.Sections {
		log.Printf("   %s %s - Category: %s", section.Emoji, section.Label, section.Category)
	}

	return nil
}

// Load ticket configuration from JSON file
func loadTicketJSON() (*TicketJSONConfig, error) {
	file, err := os.Open("ticket.json")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, err
	}

	var config TicketJSONConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// Validate configuration
	if len(config.Sections) == 0 {
		return nil, fmt.Errorf("no ticket sections defined in ticket.json")
	}

	// Set defaults for settings if not provided
	if config.Settings.AdminRoleName == "" {
		config.Settings.AdminRoleName = "Admin"
	}
	if config.Settings.MaxOpenTicketsPerUser == 0 {
		config.Settings.MaxOpenTicketsPerUser = 1
	}

	return &config, nil
}

// Create default ticket.json file
func createDefaultTicketJSON() error {
	defaultConfig := TicketJSONConfig{
		Sections: []TicketSection{
			{
				ID:             "ticket",
				Label:          "💌 เปิด Ticket",
				Description:    "เปิด Ticket เพื่อถามคำถามหรือแจ้งปัญหา",
				Emoji:          "💌",
				Category:       "Tickets",
				Color:          0x3498db,
				WelcomeMessage: "สวัสดี {mention}! โปรดอธิบายคำถามหรือปัญหาของคุณที่นี่ ทีมงานของเราจะช่วยเหลือคุณโดยเร็วที่สุด",
			},
			{
				ID:             "donate",
				Label:          "💰 เปิด Donate Ticket",
				Description:    "เปิด Donate Ticket เพื่อบริจาคหรือสนับสนุน",
				Emoji:          "💰",
				Category:       "Donations",
				Color:          0xf1c40f,
				WelcomeMessage: "สวัสดี {mention}! ขอบคุณสำหรับความสนใจในการสนับสนุนเรา โปรดแจ้งรายละเอียดการบริจาคของคุณที่นี่",
			},
			{
				ID:             "raid",
				Label:          "🚨 เปิด Raid",
				Description:    "เปิด Raid เพื่อแจ้ง Raid",
				Emoji:          "🚨",
				Category:       "Raids",
				Color:          0xe74c3c,
				WelcomeMessage: "สวัสดี {mention}! โปรดให้รายละเอียดเกี่ยวกับ Raid ที่คุณต้องการจัดการที่นี่",
			},
			{
				ID:             "botshop",
				Label:          "🤖 เช่า Botshop",
				Description:    "สอบถามเช่า Botshop",
				Emoji:          "🤖",
				Category:       "Botshops",
				Color:          0x9b59b6,
				WelcomeMessage: "สวัสดี {mention}! ขอบคุณที่สนใจบริการเช่า Botshop โปรดแจ้งความต้องการของคุณที่นี่",
			},
		},
		Settings: TicketSettings{
			MaxOpenTicketsPerUser: 1,
			AutoCloseAfterHours:   0,
			RequireAdminToClose:   true,
			SaveTranscripts:       true,
			AdminRoleName:         "Admin",
			DonateInfo: DonateInfo{
				Title:       "💰 การสนับสนุนเซิร์ฟเวอร์ TimeSkip",
				Description: "การสนับสนุนนี้เป็นการสนับสนุนระบบของเซิร์ฟเวอร์ เช่น ค่าระบบบอท ค่ารักษาเซิร์ฟเวอร์ และการพัฒนาในอนาคต\nไม่เกี่ยวข้องกับระบบเกมจำลองโดยตรง",
				ImageURL:    "https://cdn.discordapp.com/attachments/1345511362251591750/1362332412469575762/IMG_1665.png",
				Packages: []DonatePackage{
					{
						Name:        "💎 100 บาท",
						Description: "• ขอบคุณด้วย Coin 100,000\n• บางช่วงอาจมีโบนัสพิเศษ เช่น 120,000 หรือ 130,000 Coin",
					},
					{
						Name:        "💎 250 บาท",
						Description: "• Godmode Builder Mode นาน 5 ชั่วโมง\n• ใช้สำหรับสร้างบ้าน เคลียร์พื้นที่ ย้ายสิ่งของ\n• (ไม่มีผลต่อ PvP หรือการต่อสู้)",
					},
					{
						Name:        "💎 80 บาท (Whitelist mini Package)",
						Description: "• เข้าเซิร์ฟเวอร์ได้แม้ว่าเซิร์ฟจะเต็มเพียง 1 วัน (Whitelist Access)",
					},
					{
						Name:        "💎 300 บาท (Whitelist Silver Package)",
						Description: "• เข้าเซิร์ฟเวอร์ได้แม้ว่าเซิร์ฟจะเต็มนาน 30 วัน (Whitelist Access)\n• Coin 200,000",
					},
					{
						Name:        "💎 450 บาท (Whitelist Gold Supporter Package)",
						Description: "• เข้าเซิร์ฟเวอร์ได้แม้ว่าเซิร์ฟจะเต็มนาน 30 วัน (Whitelist Access)\n• Coin 350,000\n• ส่วนลดร้านค้าในเกม 30% นาน 30 วัน",
					},
					{
						Name:        "💎 650 บาท (Whitelist Platinum Supporter Package)",
						Description: "• เข้าเซิร์ฟเวอร์ได้แม้ว่าเซิร์ฟจะเต็มนาน 30 วัน (Whitelist Access)\n• Coin 500,000\n• ส่วนลดร้านค้าในเกม 50% นาน 30 วัน",
					},
					{
						Name:        "💎 800 บาท (Whitelist Diamond Supporter Package)",
						Description: "• เข้าเซิร์ฟเวอร์ได้แม้ว่าเซิร์ฟจะเต็มนาน 30 วัน (Whitelist Access)\n• Coin 700,000\n• ส่วนลดร้านค้าในเกม 60% นาน 30 วัน รับ Coins 30,000 ทุกวันนาน 30 วัน",
					},
					{
						Name:        "📌 โปรโมชั่นสเตตัสสูงสุด",
						Description: "🔹 **ทุกค่าสเตตัสระดับสูงสุด 8 5 5 5**\n• 🔸 โดเนท 600 บาท\nหรือ\n• 🔸 ใช้ Coin แลกได้ในราคา 800,000 Coin **ได้ ค่าสเตตัสทุกค่า ระดับสูงสุด**\n• ระยะเวลา 2 เดือน จะทำการ Wipe ตัวละคร หากผู้เล่น โดเนทช่วงเดือนที่ 2 ก่อนจะ Wipe ราคาจะปรับลด 50% คือ 300 บาท หรือ 400,000 Coin",
					},
				},
				Note:    "**Coin เป็นเพียงหน่วยใช้ภายในระบบเพื่อความบันเทิง**\nผู้เล่นสามารถใช้ Coin เพื่อร่วมกิจกรรมจำลองในเซิร์ฟเวอร์ได้ เช่น เกมจำลองต่าง ๆ\nแต่ Coin ไม่มีมูลค่าเงินจริง และไม่สามารถแลกเปลี่ยนเป็นเงินจริงได้\nการสนับสนุนเป็นความสมัครใจ ทีมงานไม่ได้จำหน่ายสินค้า หรือบริการที่มีมูลค่าทางเงินจริง",
				Contact: "สแกน QR Code ด้านบน หรือติดต่อแอดมินโดยตรง",
			},
		},
	}

	data, err := json.MarshalIndent(defaultConfig, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile("ticket.json", data, 0644)
}

// Get ticket section by ID
func getTicketSection(sectionID string) *TicketSection {
	for _, section := range ticketConfig.Sections {
		if section.ID == sectionID {
			return &section
		}
	}
	return nil
}

// StartTicket starts the ticket system
func StartTicket(ctx context.Context, session *discordgo.Session) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [TICKET START] Panic recovered: %v\n%s", r, debug.Stack())
		}
	}()

	// Send initial ticket embed if channel is configured
	if ticketConfig.TicketChannelID != "" {
		go func() {
			time.Sleep(3 * time.Second) // Wait for bot to be ready
			sendInitialTicketEmbed(session)
		}()
	}

	log.Printf("✅ Ticket system started!")
}

// Send initial ticket embed
func sendInitialTicketEmbed(s *discordgo.Session) {
	if ticketConfig.TicketChannelID == "" {
		return
	}

	channel, err := s.Channel(ticketConfig.TicketChannelID)
	if err != nil {
		log.Printf("Error getting ticket channel: %v", err)
		return
	}

	// Check permissions
	perms, err := s.UserChannelPermissions(s.State.User.ID, ticketConfig.TicketChannelID)
	if err != nil {
		log.Printf("Error checking permissions: %v", err)
		return
	}

	// Clear channel if has manage messages permission
	if perms&discordgo.PermissionManageMessages != 0 {
		messages, err := s.ChannelMessages(ticketConfig.TicketChannelID, 100, "", "", "")
		if err == nil {
			for _, msg := range messages {
				if msg.Author.ID == s.State.User.ID {
					s.ChannelMessageDelete(ticketConfig.TicketChannelID, msg.ID)
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		log.Printf("✅ Cleared ticket channel %s", channel.Name)
	}

	// Send ticket selection embed
	embed := createTicketSelectionEmbed()
	components := createTicketComponents()

	_, err = s.ChannelMessageSendComplex(ticketConfig.TicketChannelID, &discordgo.MessageSend{
		Embed:      embed,
		Components: components,
	})

	if err != nil {
		log.Printf("Error sending ticket embed: %v", err)
	} else {
		log.Printf("✅ Sent ticket selection embed with %d options", len(ticketConfig.Sections))
	}
}

// Get display name from Discord (prioritize nickname over username)
func getDisplayName(s *discordgo.Session, guildID, userID string) string {
	// Try to get guild member first
	member, err := s.GuildMember(guildID, userID)
	if err != nil {
		// If can't get member, fallback to user
		user, err := s.User(userID)
		if err != nil {
			return "Unknown"
		}
		return user.Username
	}

	// Priority: Nickname -> Global Display Name -> Username
	if member.Nick != "" {
		return member.Nick
	}

	if member.User != nil {
		// Check for global display name (new Discord feature)
		if member.User.GlobalName != "" {
			return member.User.GlobalName
		}
		return member.User.Username
	}

	return "Unknown"
}

// Get both display name and username for logging
func getDisplayNameAndUsername(s *discordgo.Session, guildID, userID string) (displayName, username string) {
	member, err := s.GuildMember(guildID, userID)
	if err != nil {
		user, err := s.User(userID)
		if err != nil {
			return "Unknown", "Unknown"
		}
		return user.Username, user.Username
	}

	// Get username
	username = "Unknown"
	if member.User != nil {
		username = member.User.Username
	}

	// Get display name (prioritize nickname)
	if member.Nick != "" {
		displayName = member.Nick
	} else if member.User != nil {
		if member.User.GlobalName != "" {
			displayName = member.User.GlobalName
		} else {
			displayName = member.User.Username
		}
	} else {
		displayName = "Unknown"
	}

	return displayName, username
}

// Sanitize filename
func sanitizeFilename(filename string) string {
	reg := regexp.MustCompile(`[^\p{L}\p{N}\s_-]`)
	sanitized := reg.ReplaceAllString(filename, "")
	sanitized = strings.TrimSpace(sanitized)

	if sanitized == "" {
		sanitized = "unknown"
	}
	return sanitized
}

// Create safe channel name
func createSafeChannelName(displayName, ticketType string, userID string) string {
	reg := regexp.MustCompile(`[<>:"/\\|?*]`)
	safeName := reg.ReplaceAllString(displayName, "")
	safeName = strings.ReplaceAll(safeName, " ", "-")
	safeName = strings.Trim(safeName, "-")

	if safeName == "" {
		safeName = fmt.Sprintf("user-%s", userID)
	}

	channelName := fmt.Sprintf("%s-%s", ticketType, safeName)
	channelName = strings.ToLower(channelName)

	if len(channelName) > 100 {
		maxNameLength := 90 - len(ticketType) - 1
		if maxNameLength > 0 && len(safeName) > maxNameLength {
			safeName = safeName[:maxNameLength]
		}
		channelName = fmt.Sprintf("%s-%s", ticketType, safeName)
		channelName = strings.ToLower(channelName)
	}

	return channelName
}

// Save ticket history
func saveTicketHistory(messages []TicketMessage, username, ticketType string) error {
	if !ticketConfig.Settings.SaveTranscripts {
		return nil
	}

	sanitizedUsername := sanitizeFilename(username)
	folderPath := filepath.Join(fmt.Sprintf("%s_history", ticketType), sanitizedUsername)

	if err := os.MkdirAll(folderPath, 0755); err != nil {
		return err
	}

	timestamp := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("%s_%s_%s.json", sanitizedUsername, ticketType, timestamp)
	filePath := filepath.Join(folderPath, filename)

	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "    ")
	if err := encoder.Encode(messages); err != nil {
		return err
	}

	log.Printf("✅ Saved ticket history for %s in %s", username, ticketType)
	return nil
}

// Check if user has open channel
func userHasOpenChannel(s *discordgo.Session, guildID string, userID string) (bool, *discordgo.Channel) {
	// Get fresh guild data from API to ensure accuracy
	guild, err := s.Guild(guildID)
	if err != nil {
		log.Printf("Error getting guild in userHasOpenChannel: %v", err)
		return false, nil
	}

	for _, channel := range guild.Channels {
		if channel.Type == discordgo.ChannelTypeGuildText && channel.Topic != "" {
			// Check all possible ticket type prefixes
			for _, section := range ticketConfig.Sections {
				prefix := fmt.Sprintf("%s_owner:", section.ID)
				if strings.HasPrefix(channel.Topic, prefix) {
					ownerID := strings.TrimPrefix(channel.Topic, prefix)
					if ownerID == userID {
						log.Printf("⚠️ User %s already has open ticket: %s", userID, channel.Name)
						return true, channel
					}
				}
			}
		}
	}
	return false, nil
}

// Create ticket embed
func createTicketSelectionEmbed() *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Title:       "📨 คลิกเพื่อเปิดช่องสนทนา",
		Description: "เลือกประเภทด้านล่างตามความต้องการของคุณ:",
		Color:       0x9900cc,
		Timestamp:   time.Now().Format(time.RFC3339),
	}

	// Add thumbnail if URL is configured
	if ticketConfig.ThumbnailURL != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
			URL: ticketConfig.ThumbnailURL,
		}
	}

	// Add image if URL is configured
	if ticketConfig.ImageURL != "" {
		embed.Image = &discordgo.MessageEmbedImage{
			URL: ticketConfig.ImageURL,
		}
	}

	// Add footer with icon if URL is configured
	if ticketConfig.FooterIconURL != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text:    "© powered by TimeSkip",
			IconURL: ticketConfig.FooterIconURL,
		}
	} else {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: "© powered by TimeSkip",
		}
	}

	return embed
}

// Create ticket components
func createTicketComponents() []discordgo.MessageComponent {
	// Build options from sections
	var options []discordgo.SelectMenuOption
	for _, section := range ticketConfig.Sections {
		option := discordgo.SelectMenuOption{
			Label: section.Label,
			Value: section.ID,
		}

		// Add emoji if specified
		if section.Emoji != "" {
			option.Emoji = &discordgo.ComponentEmoji{
				Name: section.Emoji,
			}
		}

		options = append(options, option)
	}

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "ticket_select",
					Placeholder: "📩 เลือกประเภท Ticket ที่ต้องการเปิด...",
					MinValues:   intPtr(1),
					MaxValues:   1,
					Options:     options,
				},
			},
		},
	}
}

// Create ticket channel
func createTicketChannel(s *discordgo.Session, i *discordgo.InteractionCreate, ticketType string) error {
	// Get user-specific lock to prevent race conditions
	userLock := getUserTicketLock(i.Member.User.ID)
	userLock.Lock()
	defer userLock.Unlock()

	log.Printf("🔍 [TICKET] User %s attempting to create %s ticket", i.Member.User.ID, ticketType)

	// Get ticket section
	section := getTicketSection(ticketType)
	if section == nil {
		log.Printf("❌ [TICKET] Section not found: %s", ticketType)
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ ไม่พบประเภท ticket นี้",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	// First check: Before responding to interaction
	hasOpen, existingChannel := userHasOpenChannel(s, i.GuildID, i.Member.User.ID)
	if hasOpen {
		log.Printf("⚠️ [TICKET] User %s already has open ticket: %s (First Check)",
			i.Member.User.ID, existingChannel.Name)
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("⚠️ **คุณมีช่อง Ticket ที่เปิดอยู่แล้ว!**\n\nกรุดาปิดช่องเดิมก่อนเปิดใหม่\n\n➡️ %s", existingChannel.Mention()),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	// Defer response
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("❌ [TICKET] Error deferring interaction: %v", err)
		return err
	}

	// Second check: After deferring (double-check for race conditions)
	time.Sleep(200 * time.Millisecond) // Small delay to let Discord API update
	hasOpen, existingChannel = userHasOpenChannel(s, i.GuildID, i.Member.User.ID)
	if hasOpen {
		log.Printf("⚠️ [TICKET] User %s already has open ticket: %s (Second Check - Race Condition Prevented)",
			i.Member.User.ID, existingChannel.Name)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr(fmt.Sprintf("⚠️ **คุณมีช่อง Ticket ที่เปิดอยู่แล้ว!**\n\nกรุดาปิดช่องเดิมก่อนเปิดใหม่\n\n➡️ %s", existingChannel.Mention())),
		})
		return nil
	}

	// Get fresh guild data
	guild, err := s.Guild(i.GuildID)
	if err != nil {
		log.Printf("❌ [TICKET] Error getting guild: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ เกิดข้อผิดพลาดในการดึงข้อมูล server"),
		})
		return err
	}

	// Get or create category with exact name matching
	categoryName := section.Category
	var category *discordgo.Channel

	// Refresh channels list
	channels, err := s.GuildChannels(i.GuildID)
	if err != nil {
		log.Printf("❌ [TICKET] Error getting guild channels: %v", err)
	} else {
		for _, ch := range channels {
			if ch.Type == discordgo.ChannelTypeGuildCategory && ch.Name == categoryName {
				category = ch
				log.Printf("✅ [TICKET] Found existing category: %s (ID: %s)", categoryName, ch.ID)
				break
			}
		}
	}

	if category == nil {
		log.Printf("📝 [TICKET] Creating new category: %s", categoryName)
		category, err = s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
			Name: categoryName,
			Type: discordgo.ChannelTypeGuildCategory,
		})
		if err != nil {
			log.Printf("❌ [TICKET] Error creating category: %v", err)
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content: stringPtr("❌ ไม่สามารถสร้าง Category ได้"),
			})
			return err
		}
		log.Printf("✅ [TICKET] Created new category: %s (ID: %s)", categoryName, category.ID)
		time.Sleep(500 * time.Millisecond) // Wait for category to be created
	}

	// Get display name (nickname priority)
	displayName := getDisplayName(s, i.GuildID, i.Member.User.ID)
	channelName := createSafeChannelName(displayName, section.ID, i.Member.User.ID)

	log.Printf("🎫 [TICKET] Creating ticket for user: %s (Display: %s, ID: %s, Type: %s)",
		i.Member.User.Username, displayName, i.Member.User.ID, section.ID)

	// Create channel
	channel, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
		Name:     channelName,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: category.ID,
		Topic:    fmt.Sprintf("%s_owner:%s", section.ID, i.Member.User.ID),
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:   i.GuildID,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
			{
				ID:    i.Member.User.ID,
				Type:  discordgo.PermissionOverwriteTypeMember,
				Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionReadMessageHistory,
			},
		},
	})

	if err != nil {
		log.Printf("❌ [TICKET] Error creating ticket channel: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ ไม่สามารถสร้างช่องได้ กรุดาติดต่อแอดมิน"),
		})
		return err
	}

	log.Printf("✅ [TICKET] Created ticket channel: %s (ID: %s) in category: %s", channelName, channel.ID, categoryName)

	// Initialize message tracking
	ticketMessagesMux.Lock()
	ticketMessages[channel.ID] = []TicketMessage{}
	ticketMessagesMux.Unlock()

	// Send welcome embed
	welcomeEmbed := createWelcomeEmbed(section, i.Member.User, displayName)
	closeButton := createCloseButton(section)

	welcomeMsg, err := s.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Embed:      welcomeEmbed,
		Components: []discordgo.MessageComponent{closeButton},
	})

	if err != nil {
		log.Printf("⚠️ [TICKET] Error sending welcome message: %v", err)
	}

	// Notify admins
	adminRole := getAdminRole(guild)
	notifyContent := fmt.Sprintf("%s %s", i.Member.User.Mention(), displayName)
	if adminRole != nil {
		notifyContent = fmt.Sprintf("%s %s", adminRole.Mention(), notifyContent)
	}

	notifyEmbed := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("📢 %s ใหม่", strings.TrimPrefix(section.Label, section.Emoji+" ")),
		Description: fmt.Sprintf("มี %s ใหม่ที่ถูกเปิดโดย %s\n**ชื่อที่แสดง:** %s\n**Username:** @%s",
			section.Label, i.Member.User.Mention(), displayName, i.Member.User.Username),
		Color: section.Color,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "ลิงก์ช่อง",
				Value:  fmt.Sprintf("[คลิกที่นี่เพื่อไปยังช่อง](%s)", fmt.Sprintf("https://discord.com/channels/%s/%s/%s", i.GuildID, channel.ID, welcomeMsg.ID)),
				Inline: false,
			},
		},
	}

	s.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Content: notifyContent,
		Embed:   notifyEmbed,
	})

	// Edit interaction response
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: stringPtr(fmt.Sprintf("%s **สร้าง %s สำเร็จ!**\n\n➡️ %s\n\nกรุดาคลิกที่ชื่อช่องด้านบนเพื่อไปยังช่องของคุณ",
			section.Emoji, section.Label, channel.Mention())),
	})

	log.Printf("✅ [TICKET] Ticket creation completed successfully for user %s", i.Member.User.ID)

	return nil
}

// Create welcome embed
func createWelcomeEmbed(section *TicketSection, user *discordgo.User, displayName string) *discordgo.MessageEmbed {
	// Replace {mention} placeholder in welcome message
	welcomeMsg := strings.ReplaceAll(section.WelcomeMessage, "{mention}", user.Mention())

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s %s เปิดแล้ว!", section.Emoji, strings.TrimPrefix(section.Label, section.Emoji+" ")),
		Description: welcomeMsg,
		Color:       section.Color,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "เวลาที่เปิด",
				Value:  fmt.Sprintf("<t:%d:F>", time.Now().Unix()),
				Inline: false,
			},
			{
				Name:   "เปิดโดย",
				Value:  fmt.Sprintf("%s\n**ชื่อที่แสดง:** %s\n**Username:** @%s", user.Mention(), displayName, user.Username),
				Inline: true,
			},
			{
				Name:   "สถานะ",
				Value:  "🟢 เปิดอยู่",
				Inline: true,
			},
		},
		Thumbnail: &discordgo.MessageEmbedThumbnail{
			URL: user.AvatarURL(""),
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Ticket ID: %s", "pending"),
		},
	}

	return embed
}

// Create close button
func createCloseButton(section *TicketSection) discordgo.MessageComponent {
	buttons := []discordgo.MessageComponent{
		discordgo.Button{
			Label:    fmt.Sprintf("❌ ปิด %s", strings.TrimPrefix(section.Label, section.Emoji+" ")),
			Style:    discordgo.DangerButton,
			CustomID: fmt.Sprintf("close_%s", section.ID),
		},
	}

	// เพิ่มปุ่มรายละเอียดการสนับสนุนสำหรับ donate ticket เท่านั้น
	if section.ID == "donate" {
		buttons = append(buttons, discordgo.Button{
			Label:    "💵 รายละเอียดการสนับสนุนเซิร์ฟเวอร์",
			Style:    discordgo.SuccessButton,
			CustomID: "donate_info",
			Emoji: &discordgo.ComponentEmoji{
				Name: "💵",
			},
		})
	}

	return discordgo.ActionsRow{
		Components: buttons,
	}
}

// Get admin role
func getAdminRole(guild *discordgo.Guild) *discordgo.Role {
	roleName := ticketConfig.Settings.AdminRoleName
	for _, role := range guild.Roles {
		if role.Name == roleName {
			return role
		}
	}
	return nil
}

// Close ticket
func closeTicket(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	// Check if user is admin (if required)
	if ticketConfig.Settings.RequireAdminToClose {
		hasAdmin := false
		for _, roleID := range i.Member.Roles {
			role, err := s.State.Role(i.GuildID, roleID)
			if err == nil && (role.Permissions&discordgo.PermissionAdministrator != 0 || role.Name == ticketConfig.Settings.AdminRoleName) {
				hasAdmin = true
				break
			}
		}

		if !hasAdmin {
			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "⛔ คุณไม่สามารถปิดช่องนี้ได้เนื่องจากไม่ใช่แอดมิน",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
		}
	}

	channel, err := s.Channel(i.ChannelID)
	if err != nil || channel.Topic == "" {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ ไม่พบข้อมูล topic ในช่องนี้",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	// Parse ticket info
	parts := strings.Split(channel.Topic, ":")
	if len(parts) != 2 {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ รูปแบบ topic ไม่ถูกต้อง",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	ticketType := strings.TrimSuffix(parts[0], "_owner")
	ownerID := parts[1]

	// Get section
	section := getTicketSection(ticketType)
	if section == nil {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ ไม่พบประเภท ticket นี้",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	ticketOwner, err := s.User(ownerID)
	if err != nil {
		return err
	}

	// Get display name (nickname priority)
	displayName := getDisplayName(s, i.GuildID, ownerID)

	log.Printf("🔒 Closing ticket for user: %s (Display: %s, ID: %s)",
		ticketOwner.Username, displayName, ownerID)

	// Get guild for admin role
	guild, err := s.Guild(i.GuildID)
	if err != nil {
		log.Printf("Error getting guild: %v", err)
	}

	// Send closing message
	closingEmbed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🔒 กำลังปิด %s...", strings.TrimPrefix(section.Label, section.Emoji+" ")),
		Description: "ช่องนี้จะถูกปิดในอีก 5 วินาที\nประวัติการสนทนาจะถูกบันทึกไว้",
		Color:       0xff9900,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "เจ้าของช่อง",
				Value:  fmt.Sprintf("%s\n**ชื่อที่แสดง:** %s\n**Username:** @%s", ticketOwner.Mention(), displayName, ticketOwner.Username),
				Inline: true,
			},
			{
				Name:   "🛑 ปิดโดย",
				Value:  i.Member.User.Mention(),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "กำลังบันทึกประวัติการสนทนา...",
		},
	}

	notifyContent := ticketOwner.Mention()
	if guild != nil {
		adminRole := getAdminRole(guild)
		if adminRole != nil {
			notifyContent = fmt.Sprintf("%s %s", adminRole.Mention(), notifyContent)
		}
	}

	s.ChannelMessageSendComplex(i.ChannelID, &discordgo.MessageSend{
		Content: notifyContent,
		Embed:   closingEmbed,
	})

	// Respond to interaction
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{
				{
					Title:       "✅ ปิดช่องสำเร็จ",
					Description: fmt.Sprintf("%s ถูกปิดและบันทึกประวัติเรียบร้อยแล้ว", section.Label),
					Color:       0x00ff00,
				},
			},
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})

	// Save history
	ticketMessagesMux.RLock()
	messages := ticketMessages[i.ChannelID]
	ticketMessagesMux.RUnlock()

	if len(messages) > 0 {
		saveTicketHistory(messages, displayName, section.ID)
	}

	// Delete messages from map
	ticketMessagesMux.Lock()
	delete(ticketMessages, i.ChannelID)
	ticketMessagesMux.Unlock()

	// Wait and delete channel
	time.Sleep(5 * time.Second)
	_, err = s.ChannelDelete(i.ChannelID)
	if err != nil {
		log.Printf("Error deleting channel: %v", err)
	}

	return nil
}

// Show donate information
func showDonateInfo(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	// Defer the response
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("❌ Error deferring interaction: %v", err)
		return err
	}

	// Get channel info to find ticket owner
	channel, err := s.Channel(i.ChannelID)
	if err != nil || channel.Topic == "" {
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ ไม่พบข้อมูล topic ในช่องนี้"),
		})
		return fmt.Errorf("channel topic not found")
	}

	// Parse ticket info
	parts := strings.Split(channel.Topic, ":")
	if len(parts) != 2 {
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ รูปแบบ topic ไม่ถูกต้อง"),
		})
		return fmt.Errorf("invalid topic format")
	}

	ownerID := parts[1]
	ticketOwner, err := s.User(ownerID)
	if err != nil {
		log.Printf("Error getting ticket owner: %v", err)
	}

	displayName := getDisplayName(s, i.GuildID, ownerID)

	// Get donate info from config
	donateInfo := ticketConfig.Settings.DonateInfo

	// Create donate info embed
	donateEmbed := &discordgo.MessageEmbed{
		Title:       donateInfo.Title,
		Description: donateInfo.Description,
		Color:       0xf1c40f,
	}

	// Add image if URL is provided
	if donateInfo.ImageURL != "" {
		donateEmbed.Image = &discordgo.MessageEmbedImage{
			URL: donateInfo.ImageURL,
		}
	}

	// Add packages as fields
	for _, pkg := range donateInfo.Packages {
		donateEmbed.Fields = append(donateEmbed.Fields, &discordgo.MessageEmbedField{
			Name:   pkg.Name,
			Value:  pkg.Description,
			Inline: false,
		})
	}

	// Add note field
	if donateInfo.Note != "" {
		donateEmbed.Fields = append(donateEmbed.Fields, &discordgo.MessageEmbedField{
			Name:   "📌 หมายเหตุ",
			Value:  donateInfo.Note,
			Inline: false,
		})
	}

	// Add contact field
	if donateInfo.Contact != "" {
		donateEmbed.Fields = append(donateEmbed.Fields, &discordgo.MessageEmbedField{
			Name:   "📲 ช่องทางการสนับสนุน",
			Value:  donateInfo.Contact,
			Inline: false,
		})
	}

	// Get guild and admin role
	guild, err := s.Guild(i.GuildID)
	var mentionContent string
	if err == nil {
		adminRole := getAdminRole(guild)
		if adminRole != nil && ticketOwner != nil {
			mentionContent = fmt.Sprintf("%s %s\n**ผู้ขอข้อมูล:** %s", adminRole.Mention(), ticketOwner.Mention(), displayName)
		} else if ticketOwner != nil {
			mentionContent = fmt.Sprintf("%s\n**ผู้ขอข้อมูล:** %s", ticketOwner.Mention(), displayName)
		}
	}

	// Send the embed
	_, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: mentionContent,
		Embeds:  []*discordgo.MessageEmbed{donateEmbed},
	})

	if err != nil {
		log.Printf("Error sending donate info: %v", err)
		return err
	}

	log.Printf("✅ Sent donate information to channel %s", i.ChannelID)
	return nil
}

// Ticket message handler
func ticketMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [TICKET MESSAGE] Panic recovered: %v", r)
		}
	}()

	if m.Author.Bot {
		return
	}

	// Check if message is in a ticket channel
	ticketMessagesMux.RLock()
	_, exists := ticketMessages[m.ChannelID]
	ticketMessagesMux.RUnlock()

	if exists {
		channel, err := s.Channel(m.ChannelID)
		if err != nil {
			return
		}

		// Get both display name and username for comprehensive logging
		displayName, username := getDisplayNameAndUsername(s, channel.GuildID, m.Author.ID)

		msgData := TicketMessage{
			Author:            displayName,
			AuthorDiscordName: username,
			AuthorID:          m.Author.ID,
			Content:           m.Content,
			Timestamp:         time.Now().Format(time.RFC3339),
		}

		if len(m.Attachments) > 0 {
			attachmentPaths := []string{}
			for _, attachment := range m.Attachments {
				attachmentPaths = append(attachmentPaths, attachment.URL)
			}
			msgData.Attachments = attachmentPaths
		}

		ticketMessagesMux.Lock()
		ticketMessages[m.ChannelID] = append(ticketMessages[m.ChannelID], msgData)
		ticketMessagesMux.Unlock()

		log.Printf("✅ Recorded message from %s in ticket channel", displayName)
	}

	// Handle ticket command
	if strings.HasPrefix(m.Content, "!ticket") {
		handleTicketCommand(s, m)
	}

	// Handle reload ticket config command
	if strings.HasPrefix(m.Content, "!reload_ticket") {
		handleReloadTicketCommand(s, m)
	}
}

// Handle ticket command
func handleTicketCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	channel, err := s.Channel(m.ChannelID)
	if err != nil {
		return
	}

	perms, err := s.UserChannelPermissions(s.State.User.ID, m.ChannelID)
	if err != nil {
		return
	}

	if perms&discordgo.PermissionManageMessages != 0 {
		messages, err := s.ChannelMessages(m.ChannelID, 100, "", "", "")
		if err == nil {
			for _, msg := range messages {
				s.ChannelMessageDelete(m.ChannelID, msg.ID)
				time.Sleep(100 * time.Millisecond)
			}
		}
		log.Printf("✅ Cleared messages in channel %s", channel.Name)
	} else {
		s.ChannelMessageDelete(m.ChannelID, m.ID)
	}

	embed := createTicketSelectionEmbed()
	components := createTicketComponents()

	s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Embed:      embed,
		Components: components,
	})
}

// Handle reload ticket config command
func handleReloadTicketCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Check if user has admin permissions
	perms, err := s.UserChannelPermissions(m.Author.ID, m.ChannelID)
	if err != nil {
		return
	}

	// Check if user is admin or has manage server permission
	hasPermission := false
	if perms&discordgo.PermissionAdministrator != 0 || perms&discordgo.PermissionManageServer != 0 {
		hasPermission = true
	}

	if !hasPermission {
		s.ChannelMessageSend(m.ChannelID, "❌ คุณไม่มีสิทธิ์ใช้คำสั่งนี้ (ต้องการสิทธิ์ Admin)")
		return
	}

	// Reload ticket.json
	jsonConfig, err := loadTicketJSON()
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ ไม่สามารถโหลด ticket.json: %v", err))
		log.Printf("❌ Failed to reload ticket.json: %v", err)
		return
	}

	// Update config
	ticketConfig.Sections = jsonConfig.Sections
	ticketConfig.Settings = jsonConfig.Settings

	// Send success message with embed
	reloadEmbed := &discordgo.MessageEmbed{
		Title: "✅ โหลด ticket.json ใหม่สำเร็จ",
		Description: fmt.Sprintf("โหลดการตั้งค่าใหม่เรียบร้อยแล้ว\n\n**จำนวน Sections:** %d\n**Admin Role:** %s\n**Donate Packages:** %d",
			len(ticketConfig.Sections),
			ticketConfig.Settings.AdminRoleName,
			len(ticketConfig.Settings.DonateInfo.Packages)),
		Color:     0x00ff00,
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("โหลดโดย %s", m.Author.Username),
		},
	}

	s.ChannelMessageSendEmbed(m.ChannelID, reloadEmbed)
	log.Printf("✅ Ticket configuration reloaded by %s", m.Author.Username)
}

// Ticket interaction handler
func ticketInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [TICKET INTERACTION] Panic recovered: %v", r)
		}
	}()

	switch i.Type {
	case discordgo.InteractionMessageComponent:
		data := i.MessageComponentData()

		// Handle select menu
		if data.CustomID == "ticket_select" && len(data.Values) > 0 {
			ticketType := data.Values[0]
			if err := createTicketChannel(s, i, ticketType); err != nil {
				log.Printf("Error creating ticket channel: %v", err)
			}
		}

		// Handle close buttons
		if strings.HasPrefix(data.CustomID, "close_") {
			if err := closeTicket(s, i); err != nil {
				log.Printf("Error closing ticket: %v", err)
			}
		}

		// Handle donate info button
		if data.CustomID == "donate_info" {
			if err := showDonateInfo(s, i); err != nil {
				log.Printf("Error showing donate info: %v", err)
			}
		}
	}
}
