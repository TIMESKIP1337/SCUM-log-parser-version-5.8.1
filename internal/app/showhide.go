package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ShowHide module specific variables
var (
	showHideConfig ShowHideConfig
)

// ShowHideConfig represents the showhide.json structure
type ShowHideConfig struct {
	RoleID           string   `json:"role_id"`
	ChannelIDs       []string `json:"channel_ids"`
	TargetChannelID  string   `json:"target_channel_id"`
	EmbedTitle       string   `json:"embed_title"`
	EmbedDescription string   `json:"embed_description"`
	EmbedColor       int      `json:"embed_color"`
	FooterText       string   `json:"footer_text"`
	HideButtonLabel  string   `json:"hide_button_label"`
	ShowButtonLabel  string   `json:"show_button_label"`
	HideEmoji        string   `json:"hide_emoji"`
	ShowEmoji        string   `json:"show_emoji"`
	SuccessMessage   string   `json:"success_message"`
}

// Note: SafeUTF8String is defined in main.go and used globally

// InitializeShowHide initializes the show/hide system
func InitializeShowHide() error {
	// Load configuration from JSON
	config, err := loadShowHideJSON()
	if err != nil {
		log.Printf("⚠️ Warning: Could not load showhide.json: %v", err)
		log.Println("📝 Creating default showhide.json...")
		if err := createDefaultShowHideJSON(); err != nil {
			log.Printf("❌ Failed to create default showhide.json: %v", err)
			return err
		}
		// Try loading again
		config, err = loadShowHideJSON()
		if err != nil {
			return fmt.Errorf("failed to load showhide configuration: %v", err)
		}
	}

	showHideConfig = *config

	log.Printf("✅ ShowHide system initialized")
	log.Printf("   Role ID: %s", showHideConfig.RoleID)
	log.Printf("   Target Channel: %s", showHideConfig.TargetChannelID)
	log.Printf("   Managing %d channels", len(showHideConfig.ChannelIDs))

	return nil
}

// Load showhide configuration from JSON file
func loadShowHideJSON() (*ShowHideConfig, error) {
	data, err := ioutil.ReadFile("showhide.json")
	if err != nil {
		return nil, err
	}

	// Ensure UTF-8 validity
	dataStr := SafeUTF8String(string(data))
	data = []byte(dataStr)

	var config ShowHideConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// Validate configuration
	if config.RoleID == "" {
		return nil, fmt.Errorf("role_id is required in showhide.json")
	}
	if config.TargetChannelID == "" {
		return nil, fmt.Errorf("target_channel_id is required in showhide.json")
	}
	if len(config.ChannelIDs) == 0 {
		return nil, fmt.Errorf("channel_ids cannot be empty in showhide.json")
	}

	// Sanitize all string fields for UTF-8
	config.EmbedTitle = SafeUTF8String(config.EmbedTitle)
	config.EmbedDescription = SafeUTF8String(config.EmbedDescription)
	config.FooterText = SafeUTF8String(config.FooterText)
	config.HideButtonLabel = SafeUTF8String(config.HideButtonLabel)
	config.ShowButtonLabel = SafeUTF8String(config.ShowButtonLabel)
	config.HideEmoji = SafeUTF8String(config.HideEmoji)
	config.ShowEmoji = SafeUTF8String(config.ShowEmoji)
	config.SuccessMessage = SafeUTF8String(config.SuccessMessage)

	// Set defaults if not provided
	if config.EmbedTitle == "" {
		config.EmbedTitle = "🔒 Channel Control"
	}
	if config.EmbedColor == 0 {
		config.EmbedColor = 0x3498db
	}
	if config.FooterText == "" {
		config.FooterText = "© powered by TimeSkip"
	}
	if config.HideButtonLabel == "" {
		config.HideButtonLabel = "🔒 Hide Channels"
	}
	if config.ShowButtonLabel == "" {
		config.ShowButtonLabel = "🔓 Show Channels"
	}
	if config.SuccessMessage == "" {
		config.SuccessMessage = "ปิดเปิดห้องสำหรับยศ %s แล้ว!"
	}

	return &config, nil
}

// Save showhide configuration to JSON file
func saveShowHideJSON() error {
	data, err := json.MarshalIndent(showHideConfig, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile("showhide.json", data, 0644)
}

// Create default showhide.json file
func createDefaultShowHideJSON() error {
	defaultConfig := ShowHideConfig{
		RoleID: "YOUR_ROLE_ID_HERE",
		ChannelIDs: []string{
			"CHANNEL_ID_1",
			"CHANNEL_ID_2",
			"CHANNEL_ID_3",
		},
		TargetChannelID:  "YOUR_TARGET_CHANNEL_ID_HERE",
		EmbedTitle:       "🔒 Channel Control",
		EmbedDescription: "ปุ่มสำหรับปิดเปิดชาแนลที่เรากำหนด {role}",
		EmbedColor:       0x3498db,
		FooterText:       "© powered by TimeSkip",
		HideButtonLabel:  "🔒 Hide Channels",
		ShowButtonLabel:  "🔓 Show Channels",
		HideEmoji:        "🔒",
		ShowEmoji:        "🔓",
		SuccessMessage:   "ปิดเปิดห้องสำหรับยศ %s แล้ว!",
	}

	data, err := json.MarshalIndent(defaultConfig, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile("showhide.json", data, 0644)
}

// StartShowHide starts the show/hide system
func StartShowHide(ctx context.Context, session *discordgo.Session) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [SHOWHIDE START] Panic recovered: %v\n%s", r, debug.Stack())
		}
	}()

	// Send initial control embed if channel is configured
	if showHideConfig.TargetChannelID != "" {
		go func() {
			time.Sleep(3 * time.Second) // Wait for bot to be ready
			sendShowHideControlEmbed(session)
		}()
	}

	log.Printf("✅ ShowHide system started!")
}

// Send show/hide control embed
func sendShowHideControlEmbed(s *discordgo.Session) {
	if showHideConfig.TargetChannelID == "" {
		return
	}

	channel, err := s.Channel(showHideConfig.TargetChannelID)
	if err != nil {
		log.Printf("Error getting target channel: %v", err)
		return
	}

	// Check permissions
	perms, err := s.UserChannelPermissions(s.State.User.ID, showHideConfig.TargetChannelID)
	if err != nil {
		log.Printf("Error checking permissions: %v", err)
		return
	}

	// Clear old control messages if has manage messages permission
	if perms&discordgo.PermissionManageMessages != 0 {
		messages, err := s.ChannelMessages(showHideConfig.TargetChannelID, 100, "", "", "")
		if err == nil {
			for _, msg := range messages {
				if msg.Author.ID == s.State.User.ID && len(msg.Embeds) > 0 {
					// Check if it's our control message
					if msg.Embeds[0].Title == showHideConfig.EmbedTitle {
						s.ChannelMessageDelete(showHideConfig.TargetChannelID, msg.ID)
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
		}
		log.Printf("✅ Cleared old control messages in channel %s", channel.Name)
	}

	// Get role for mention in description
	guild, err := s.Guild(channel.GuildID)
	if err != nil {
		log.Printf("Error getting guild: %v", err)
		return
	}

	var roleMention string
	for _, role := range guild.Roles {
		if role.ID == showHideConfig.RoleID {
			roleMention = role.Mention()
			break
		}
	}

	// Replace {role} placeholder in description
	description := showHideConfig.EmbedDescription
	if roleMention != "" {
		description = strings.ReplaceAll(description, "{role}", roleMention)
	}

	// Ensure UTF-8 safety
	description = SafeUTF8String(description)

	// Create embed
	embed := &discordgo.MessageEmbed{
		Title:       SafeUTF8String(showHideConfig.EmbedTitle),
		Description: description,
		Color:       showHideConfig.EmbedColor,
		Footer: &discordgo.MessageEmbedFooter{
			Text: SafeUTF8String(showHideConfig.FooterText),
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	// Create buttons (2 rows)
	components := []discordgo.MessageComponent{
		// Row 1: Hide/Show buttons
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    SafeUTF8String(showHideConfig.HideButtonLabel),
					Style:    discordgo.DangerButton,
					CustomID: "showhide_hide",
					Emoji: &discordgo.ComponentEmoji{
						Name: showHideConfig.HideEmoji,
					},
				},
				discordgo.Button{
					Label:    SafeUTF8String(showHideConfig.ShowButtonLabel),
					Style:    discordgo.SuccessButton,
					CustomID: "showhide_show",
					Emoji: &discordgo.ComponentEmoji{
						Name: showHideConfig.ShowEmoji,
					},
				},
			},
		},
		// Row 2: Settings and Manage buttons
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "ดูการตั้งค่า",
					Style:    discordgo.SecondaryButton,
					CustomID: "showhide_settings",
					Emoji: &discordgo.ComponentEmoji{
						Name: "⚙️",
					},
				},
				discordgo.Button{
					Label:    "จัดการช่อง",
					Style:    discordgo.PrimaryButton,
					CustomID: "showhide_manage",
					Emoji: &discordgo.ComponentEmoji{
						Name: "📝",
					},
				},
			},
		},
	}

	_, err = s.ChannelMessageSendComplex(showHideConfig.TargetChannelID, &discordgo.MessageSend{
		Embed:      embed,
		Components: components,
	})

	if err != nil {
		log.Printf("Error sending show/hide control embed: %v", err)
	} else {
		log.Printf("✅ Sent show/hide control embed to channel %s", channel.Name)
	}
}

// Handle show/hide button interactions
func handleShowHideInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, show bool) error {
	// Defer the response
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("❌ Error deferring interaction: %v", err)
		return err
	}

	// Get the role
	guild, err := s.Guild(i.GuildID)
	if err != nil {
		content := "❌ ไม่สามารถดึงข้อมูล server ได้"
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: &content,
		})
		return err
	}

	var role *discordgo.Role
	for _, r := range guild.Roles {
		if r.ID == showHideConfig.RoleID {
			role = r
			break
		}
	}

	if role == nil {
		content := fmt.Sprintf("❌ ไม่พบ role ID: %s", showHideConfig.RoleID)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: &content,
		})
		return fmt.Errorf("role not found: %s", showHideConfig.RoleID)
	}

	// Update channel permissions
	errors := []string{}
	successCount := 0

	for _, channelID := range showHideConfig.ChannelIDs {
		channel, err := s.Channel(channelID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Channel %s: not found", channelID))
			continue
		}

		// Set permissions - determine allow and deny
		var allow, deny int64
		if show {
			allow = discordgo.PermissionViewChannel
			deny = 0
		} else {
			allow = 0
			deny = discordgo.PermissionViewChannel
		}

		err = s.ChannelPermissionSet(channelID, showHideConfig.RoleID, discordgo.PermissionOverwriteTypeRole, allow, deny)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Channel %s: %v", channel.Name, err))
			continue
		}

		successCount++
		log.Printf("✅ Updated permissions for channel %s (show: %v)", channel.Name, show)
	}

	// Prepare response message
	var responseContent string
	action := "แสดง"
	emoji := showHideConfig.ShowEmoji
	if !show {
		action = "ซ่อน"
		emoji = showHideConfig.HideEmoji
	}

	if len(errors) > 0 {
		responseContent = fmt.Sprintf("⚠️ %s ช่องบางส่วนสำหรับ %s\n\n**สำเร็จ:** %d/%d\n**ข้อผิดพลาด:**\n%s",
			emoji, role.Mention(), successCount, len(showHideConfig.ChannelIDs), strings.Join(errors, "\n"))
	} else {
		responseContent = fmt.Sprintf("%s %s ช่องทั้งหมดสำหรับ %s สำเร็จ! (%d ช่อง)",
			emoji, action, role.Mention(), successCount)
	}

	// Ensure UTF-8 safety for response
	responseContent = SafeUTF8String(responseContent)

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &responseContent,
	})

	return nil
}

// Check channel visibility status for a role
func getChannelVisibilityStatus(s *discordgo.Session, channelID, roleID string) string {
	ch, err := s.Channel(channelID)
	if err != nil {
		return "❓ (ไม่สามารถตรวจสอบได้)"
	}

	// Check permission overwrites for the role
	for _, overwrite := range ch.PermissionOverwrites {
		if overwrite.ID == roleID && overwrite.Type == discordgo.PermissionOverwriteTypeRole {
			// Check if ViewChannel is allowed or denied
			if overwrite.Allow&discordgo.PermissionViewChannel != 0 {
				return "🟢 Show"
			}
			if overwrite.Deny&discordgo.PermissionViewChannel != 0 {
				return "🔴 Hide"
			}
		}
	}

	// If no specific permission overwrite, it's visible by default
	return "🟡 Default (Show)"
}

// Handle settings view interaction
func handleShowHideSettings(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	// Defer the response
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("❌ Error deferring interaction: %v", err)
		return err
	}

	// Get guild for role name
	guild, err := s.Guild(i.GuildID)
	var roleName string = "Unknown"
	if err == nil {
		for _, role := range guild.Roles {
			if role.ID == showHideConfig.RoleID {
				roleName = role.Name
				break
			}
		}
	}

	// Build channel list with names and status
	var channelList []string
	var showCount, hideCount, defaultCount int

	for idx, channelID := range showHideConfig.ChannelIDs {
		if err != nil {
			channelList = append(channelList, fmt.Sprintf("%d. `%s` ❌ (ไม่พบช่อง)", idx+1, channelID))
			continue
		}

		// Get visibility status
		status := getChannelVisibilityStatus(s, channelID, showHideConfig.RoleID)

		// Count statuses
		if strings.Contains(status, "🟢") {
			showCount++
		} else if strings.Contains(status, "🔴") {
			hideCount++
		} else if strings.Contains(status, "🟡") {
			defaultCount++
		}

		channelList = append(channelList, fmt.Sprintf("%d. <#%s> - %s", idx+1, channelID, status))
	}

	// Create status summary
	statusSummary := fmt.Sprintf("🟢 Show: %d | 🔴 Hide: %d | 🟡 Default: %d", showCount, hideCount, defaultCount)

	// Create settings embed
	settingsEmbed := &discordgo.MessageEmbed{
		Title:       "⚙️ การตั้งค่า ShowHide System",
		Description: "รายละเอียดการตั้งค่าและสถานะปัจจุบัน",
		Color:       0x3498db,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Role ที่ใช้",
				Value:  fmt.Sprintf("<@&%s>\n**ชื่อ:** %s\n**ID:** `%s`", showHideConfig.RoleID, roleName, showHideConfig.RoleID),
				Inline: false,
			},
			{
				Name:   "ช่องควบคุม",
				Value:  fmt.Sprintf("<#%s>\n**ID:** `%s`", showHideConfig.TargetChannelID, showHideConfig.TargetChannelID),
				Inline: false,
			},
			{
				Name:   "สถานะช่องทั้งหมด",
				Value:  statusSummary,
				Inline: false,
			},
			{
				Name:   fmt.Sprintf("รายการช่องที่จัดการ (%d ช่อง)", len(showHideConfig.ChannelIDs)),
				Value:  strings.Join(channelList, "\n"),
				Inline: false,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ดูโดย %s | 🟢 = แสดง | 🔴 = ซ่อน | 🟡 = ค่าเริ่มต้น", i.Member.User.Username),
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{settingsEmbed},
	})

	log.Printf("✅ Showed settings to user %s (Show: %d, Hide: %d, Default: %d)",
		i.Member.User.Username, showCount, hideCount, defaultCount)
	return nil
}

// Handle manage channels interaction
func handleShowHideManage(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	// Check if user has admin permissions
	hasPermission := false
	for _, roleID := range i.Member.Roles {
		role, err := s.State.Role(i.GuildID, roleID)
		if err == nil && (role.Permissions&discordgo.PermissionAdministrator != 0 || role.Permissions&discordgo.PermissionManageServer != 0) {
			hasPermission = true
			break
		}
	}

	if !hasPermission {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ คุณไม่มีสิทธิ์ใช้งานฟีเจอร์นี้ (ต้องการสิทธิ์ Admin)",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	// Send modal for adding/removing channels
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: "showhide_manage_modal",
			Title:    "📝 จัดการช่อง ShowHide",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "add_channels",
							Label:       "เพิ่มช่อง (คั่นด้วย , หรือเว้นวรรค)",
							Style:       discordgo.TextInputParagraph,
							Placeholder: "1234567890123456789, 9876543210987654321",
							Required:    false,
							MaxLength:   500,
						},
					},
				},
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "remove_channels",
							Label:       "ลบช่อง (คั่นด้วย , หรือเว้นวรรค)",
							Style:       discordgo.TextInputParagraph,
							Placeholder: "1234567890123456789, 9876543210987654321",
							Required:    false,
							MaxLength:   500,
						},
					},
				},
			},
		},
	})

	if err != nil {
		log.Printf("❌ Error sending modal: %v", err)
		return err
	}

	log.Printf("✅ Sent manage modal to user %s", i.Member.User.Username)
	return nil
}

// Handle manage modal submission
func handleShowHideManageModal(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	// Defer the response
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("❌ Error deferring interaction: %v", err)
		return err
	}

	data := i.ModalSubmitData()

	var addChannelsInput, removeChannelsInput string
	for _, component := range data.Components {
		actionRow := component.(*discordgo.ActionsRow)
		for _, comp := range actionRow.Components {
			textInput := comp.(*discordgo.TextInput)
			if textInput.CustomID == "add_channels" {
				addChannelsInput = textInput.Value
			} else if textInput.CustomID == "remove_channels" {
				removeChannelsInput = textInput.Value
			}
		}
	}

	// Parse channel IDs to add
	var addedChannels []string
	if addChannelsInput != "" {
		// Split by comma or space
		rawIDs := strings.FieldsFunc(addChannelsInput, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n'
		})

		for _, rawID := range rawIDs {
			rawID = strings.TrimSpace(rawID)
			if rawID == "" {
				continue
			}

			// Check if channel exists
			channel, err := s.Channel(rawID)
			if err != nil {
				log.Printf("⚠️ Channel %s not found", rawID)
				continue
			}

			// Check if already exists
			exists := false
			for _, existingID := range showHideConfig.ChannelIDs {
				if existingID == rawID {
					exists = true
					break
				}
			}

			if !exists {
				showHideConfig.ChannelIDs = append(showHideConfig.ChannelIDs, rawID)
				addedChannels = append(addedChannels, fmt.Sprintf("✅ <#%s> (`%s`)", rawID, channel.Name))
			}
		}
	}

	// Parse channel IDs to remove
	var removedChannels []string
	if removeChannelsInput != "" {
		rawIDs := strings.FieldsFunc(removeChannelsInput, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n'
		})

		for _, rawID := range rawIDs {
			rawID = strings.TrimSpace(rawID)
			if rawID == "" {
				continue
			}

			// Find and remove
			for idx, existingID := range showHideConfig.ChannelIDs {
				if existingID == rawID {
					channel, _ := s.Channel(rawID)
					channelName := "Unknown"
					if channel != nil {
						channelName = channel.Name
					}

					showHideConfig.ChannelIDs = append(showHideConfig.ChannelIDs[:idx], showHideConfig.ChannelIDs[idx+1:]...)
					removedChannels = append(removedChannels, fmt.Sprintf("❌ `%s` (%s)", rawID, channelName))
					break
				}
			}
		}
	}

	// Save to JSON
	if len(addedChannels) > 0 || len(removedChannels) > 0 {
		if err := saveShowHideJSON(); err != nil {
			content := fmt.Sprintf("❌ ไม่สามารถบันทึกการเปลี่ยนแปลงได้: %v", err)
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content: &content,
			})
			return err
		}
	}

	// Build response
	var response strings.Builder
	response.WriteString("### 📝 ผลการจัดการช่อง\n\n")

	if len(addedChannels) > 0 {
		response.WriteString("**เพิ่มช่อง:**\n")
		response.WriteString(strings.Join(addedChannels, "\n"))
		response.WriteString("\n\n")
	}

	if len(removedChannels) > 0 {
		response.WriteString("**ลบช่อง:**\n")
		response.WriteString(strings.Join(removedChannels, "\n"))
		response.WriteString("\n\n")
	}

	if len(addedChannels) == 0 && len(removedChannels) == 0 {
		response.WriteString("ℹ️ ไม่มีการเปลี่ยนแปลง\n\n")
	}

	response.WriteString(fmt.Sprintf("**ช่องทั้งหมดที่จัดการ:** %d ช่อง", len(showHideConfig.ChannelIDs)))

	responseContent := response.String()
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &responseContent,
	})

	log.Printf("✅ Managed channels for user %s: Added %d, Removed %d", i.Member.User.Username, len(addedChannels), len(removedChannels))
	return nil
}

// ShowHide message handler
func showHideMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("❌ [SHOWHIDE MESSAGE] Panic recovered: %v", r)
		}
	}()

	if m.Author.Bot {
		return
	}

	// Handle reload command
	if m.Content == "!reload_showhide" {
		handleReloadShowHideCommand(s, m)
	}

	// Handle send controls command
	if m.Content == "!send_showhide_controls" {
		handleSendShowHideControlsCommand(s, m)
	}
}

// Handle reload showhide config command
func handleReloadShowHideCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
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

	// Reload showhide.json
	config, err := loadShowHideJSON()
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❌ ไม่สามารถโหลด showhide.json: %v", err))
		log.Printf("❌ Failed to reload showhide.json: %v", err)
		return
	}

	// Update config
	showHideConfig = *config

	// Send success message with embed
	reloadEmbed := &discordgo.MessageEmbed{
		Title: "✅ โหลด showhide.json ใหม่สำเร็จ",
		Description: fmt.Sprintf("โหลดการตั้งค่าใหม่เรียบร้อยแล้ว\n\n**Role ID:** %s\n**Target Channel:** <#%s>\n**จำนวนช่องที่จัดการ:** %d",
			showHideConfig.RoleID,
			showHideConfig.TargetChannelID,
			len(showHideConfig.ChannelIDs)),
		Color:     0x00ff00,
		Timestamp: time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("โหลดโดย %s", m.Author.Username),
		},
	}

	s.ChannelMessageSendEmbed(m.ChannelID, reloadEmbed)
	log.Printf("✅ ShowHide configuration reloaded by %s", m.Author.Username)
}

// Handle send controls command
func handleSendShowHideControlsCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
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

	sendShowHideControlEmbed(s)
	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✅ ส่งปุ่มควบคุมไปยังช่อง <#%s> เรียบร้อยแล้ว", showHideConfig.TargetChannelID))
}
