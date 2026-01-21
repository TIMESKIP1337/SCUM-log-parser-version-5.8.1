package app

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ChannelConfig defines a channel and its .env variable
type ChannelConfig struct {
	Name   string // Channel name (e.g., "kill-feed")
	EnvVar string // .env variable name (e.g., "KILL_CHANNEL_ID")
}

// SetupResult tracks what was created
type SetupResult struct {
	CategoryCreated bool
	ChannelsCreated int
	ChannelsSkipped int
	EnvUpdated      bool
	Errors          []string
	CreatedChannels map[string]string // EnvVar -> ChannelID
}

// All channels to create under "TIMESKIP BOT" category
var setupChannels = []ChannelConfig{
	// Kill Feed
	{Name: "kill-feed", EnvVar: "KILL_CHANNEL_ID"},
	{Name: "kill-count", EnvVar: "KILL_COUNT_CHANNEL_ID"},

	// General Logs
	{Name: "login-logs", EnvVar: "LOGIN_CHANNEL_ID"},
	{Name: "economy-logs", EnvVar: "ECONOMY_CHANNEL_ID"},
	{Name: "lockpick-logs", EnvVar: "LOCKPICK_CHANNEL_ID"},
	{Name: "trap-logs", EnvVar: "TRAP_CHANNEL_ID"},

	// Extended Logs
	{Name: "name-change-logs", EnvVar: "LOGS_ECONOMY_NAME_CHANNEL_ID"},
	{Name: "bank-logs", EnvVar: "LOGS_ECONOMY_BANK_CHANNEL_ID"},
	{Name: "vehicle-destruction", EnvVar: "LOGS_VEHICLE_DESTRUCTION_CHANNEL_ID"},
	{Name: "chest-ownership", EnvVar: "LOGS_CHEST_OWNERSHIP_CHANNEL_ID"},
	{Name: "gameplay-logs", EnvVar: "LOGS_GAMEPLAY_CHANNEL_ID"},
	{Name: "violations-logs", EnvVar: "LOGS_VIOLATIONS_CHANNEL_ID"},

	// Chat
	{Name: "chat-logs", EnvVar: "CHAT_CHANNEL_ID"},
	{Name: "global-chat", EnvVar: "CHAT_GLOBAL_CHANNEL_ID"},

	// Admin
	{Name: "admin-logs", EnvVar: "ADMIN_CHANNEL_ID"},
	{Name: "admin-public", EnvVar: "ADMIN_PUBLIC_CHANNEL_ID"},

	// Lockpicking Rankings
	{Name: "lockpick-rankings", EnvVar: "LOCKPICK_RANKING_CHANNEL_ID"},

	// Ticket
	{Name: "ticket-panel", EnvVar: "TICKET_CHANNEL_ID"},
}

const setupCategoryName = "TIMESKIP LOG"

// RegisterSetupCommand registers the /setup-logs slash command
func RegisterSetupCommand(s *discordgo.Session, guildID string) error {
	// Define the slash command (Admin only)
	adminPerm := int64(discordgo.PermissionAdministrator)
	cmd := &discordgo.ApplicationCommand{
		Name:                     "setup-logs",
		Description:              "Auto-create all log channels for TIMESKIP Log Parser and update .env",
		DefaultMemberPermissions: &adminPerm,
	}

	// Register with Discord (guild-specific for instant availability)
	_, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, cmd)
	if err != nil {
		return fmt.Errorf("failed to create /setup-logs command: %v", err)
	}

	log.Printf("Registered /setup-logs slash command for guild %s", guildID)
	return nil
}

// HandleSetupCommand handles the /setup slash command interaction
func HandleSetupCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SETUP] Panic recovered: %v\n%s", r, debug.Stack())
		}
	}()

	// Check if user has Administrator permission
	if !hasSetupAdminPermission(s, i) {
		respondSetupEphemeral(s, i, "You need Administrator permission to use this command.")
		return
	}

	// Defer response (setup takes time)
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("[SETUP] Error deferring response: %v", err)
		return
	}

	// Perform the setup
	result := performSetup(s, i.GuildID)

	// Build summary embed
	embed := buildSetupSummaryEmbed(result)

	// Edit the deferred response with the result
	_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	})
	if err != nil {
		log.Printf("[SETUP] Error editing response: %v", err)
	}
}

// hasSetupAdminPermission checks if the user has Administrator permission
func hasSetupAdminPermission(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if i.Member == nil {
		return false
	}

	// Get member permissions
	perms, err := s.UserChannelPermissions(i.Member.User.ID, i.ChannelID)
	if err != nil {
		log.Printf("[SETUP] Error checking permissions: %v", err)
		return false
	}

	return perms&discordgo.PermissionAdministrator != 0
}

// performSetup creates the category and all channels
func performSetup(s *discordgo.Session, guildID string) *SetupResult {
	result := &SetupResult{
		CreatedChannels: make(map[string]string),
		Errors:          []string{},
	}

	log.Printf("[SETUP] Starting auto-setup for guild %s", guildID)

	// Get existing channels to avoid duplicates
	existingChannels, err := s.GuildChannels(guildID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to get guild channels: %v", err))
		return result
	}

	// Build lookup maps
	existingCategories := make(map[string]*discordgo.Channel)

	for _, ch := range existingChannels {
		if ch.Type == discordgo.ChannelTypeGuildCategory {
			existingCategories[strings.ToLower(ch.Name)] = ch
		}
	}

	// Create or get the category
	categoryID, created := getOrCreateCategory(s, guildID, setupCategoryName, existingCategories)
	if created {
		result.CategoryCreated = true
		log.Printf("[SETUP] Created category: %s", setupCategoryName)
	} else if categoryID != "" {
		log.Printf("[SETUP] Using existing category: %s", setupCategoryName)
	}

	if categoryID == "" {
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to create category: %s", setupCategoryName))
		return result
	}

	// Build lookup map for channels ONLY in our category
	existingChannelsInCategory := make(map[string]*discordgo.Channel)
	for _, ch := range existingChannels {
		if ch.Type == discordgo.ChannelTypeGuildText && ch.ParentID == categoryID {
			existingChannelsInCategory[strings.ToLower(ch.Name)] = ch
		}
	}

	// Create all channels
	for _, chConfig := range setupChannels {
		channelID, created := getOrCreateChannel(s, guildID, categoryID, chConfig.Name, existingChannelsInCategory)
		if created {
			result.ChannelsCreated++
			log.Printf("[SETUP] Created channel: %s (ID: %s)", chConfig.Name, channelID)
		} else if channelID != "" {
			result.ChannelsSkipped++
			log.Printf("[SETUP] Channel already exists: %s (ID: %s)", chConfig.Name, channelID)
		}

		if channelID != "" {
			result.CreatedChannels[chConfig.EnvVar] = channelID
		} else {
			result.Errors = append(result.Errors, fmt.Sprintf("Failed to create channel: %s", chConfig.Name))
		}

		// Rate limit protection (Discord allows ~2 requests/second for channel creation)
		time.Sleep(150 * time.Millisecond)
	}

	// Update .env file
	if len(result.CreatedChannels) > 0 {
		err := updateEnvFile(result.CreatedChannels)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Failed to update .env: %v", err))
			log.Printf("[SETUP] Error updating .env: %v", err)
		} else {
			result.EnvUpdated = true
			log.Printf("[SETUP] Successfully updated .env with %d channel IDs", len(result.CreatedChannels))
		}
	}

	log.Printf("[SETUP] Setup completed: %d channels created, %d skipped, %d errors",
		result.ChannelsCreated, result.ChannelsSkipped, len(result.Errors))

	return result
}

// getOrCreateCategory finds or creates a category
func getOrCreateCategory(s *discordgo.Session, guildID, name string, existing map[string]*discordgo.Channel) (string, bool) {
	// Check if category already exists
	if cat, exists := existing[strings.ToLower(name)]; exists {
		return cat.ID, false
	}

	// Create new category
	category, err := s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name: name,
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		log.Printf("[SETUP] Error creating category %s: %v", name, err)
		return "", false
	}

	return category.ID, true
}

// getOrCreateChannel finds or creates a channel
func getOrCreateChannel(s *discordgo.Session, guildID, categoryID, name string, existing map[string]*discordgo.Channel) (string, bool) {
	// Check if channel already exists
	if ch, exists := existing[strings.ToLower(name)]; exists {
		return ch.ID, false
	}

	// Create new channel under the category
	channel, err := s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:     name,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: categoryID,
	})
	if err != nil {
		log.Printf("[SETUP] Error creating channel %s: %v", name, err)
		return "", false
	}

	return channel.ID, true
}

// updateEnvFile updates the .env file with new channel IDs
func updateEnvFile(channelIDs map[string]string) error {
	envPath := ".env"

	// Check if .env exists
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		return fmt.Errorf(".env file not found - please create it first from .env.example")
	}

	// Read current .env file
	content, err := os.ReadFile(envPath)
	if err != nil {
		return fmt.Errorf("failed to read .env: %v", err)
	}

	lines := strings.Split(string(content), "\n")
	updated := make(map[string]bool)

	// Update existing lines
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Parse KEY=value
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])

		// Check if this key should be updated
		if newValue, exists := channelIDs[key]; exists {
			lines[i] = fmt.Sprintf("%s=%s", key, newValue)
			updated[key] = true
			log.Printf("[SETUP] Updated .env: %s=%s", key, newValue)
		}
	}

	// Add any missing keys at the end
	for key, value := range channelIDs {
		if !updated[key] {
			lines = append(lines, fmt.Sprintf("%s=%s", key, value))
			log.Printf("[SETUP] Added to .env: %s=%s", key, value)
		}
	}

	// Write back to file
	output := strings.Join(lines, "\n")
	err = os.WriteFile(envPath, []byte(output), 0644)
	if err != nil {
		return fmt.Errorf("failed to write .env: %v", err)
	}

	return nil
}

// buildSetupSummaryEmbed creates the summary embed
func buildSetupSummaryEmbed(result *SetupResult) *discordgo.MessageEmbed {
	// Determine status color
	var color int
	var statusEmoji string
	var statusText string

	if len(result.Errors) == 0 {
		color = 0x00ff00 // Green - success
		statusEmoji = "SUCCESS"
		statusText = "All channels configured successfully!"
	} else if result.ChannelsCreated > 0 || result.ChannelsSkipped > 0 {
		color = 0xffff00 // Yellow - partial
		statusEmoji = "PARTIAL SUCCESS"
		statusText = "Some channels were configured, but there were errors."
	} else {
		color = 0xff0000 // Red - failed
		statusEmoji = "FAILED"
		statusText = "Setup failed. Please check errors below."
	}

	// Build channel list (grouped)
	var channelList strings.Builder
	for _, chConfig := range setupChannels {
		if channelID, exists := result.CreatedChannels[chConfig.EnvVar]; exists {
			channelList.WriteString(fmt.Sprintf("<#%s> `%s`\n", channelID, chConfig.EnvVar))
		}
	}
	if channelList.Len() == 0 {
		channelList.WriteString("No channels configured")
	}

	// Build error list
	var errorList string
	if len(result.Errors) > 0 {
		errorList = strings.Join(result.Errors, "\n")
		if len(errorList) > 1000 {
			errorList = errorList[:997] + "..."
		}
	} else {
		errorList = "None"
	}

	// Category status
	categoryStatus := "Already exists"
	if result.CategoryCreated {
		categoryStatus = "Created"
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("TIMESKIP Bot Setup - %s", statusEmoji),
		Description: statusText,
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Category",
				Value:  fmt.Sprintf("`%s` - %s", setupCategoryName, categoryStatus),
				Inline: false,
			},
			{
				Name:   "Channels Created",
				Value:  fmt.Sprintf("%d", result.ChannelsCreated),
				Inline: true,
			},
			{
				Name:   "Channels Skipped",
				Value:  fmt.Sprintf("%d (already exist)", result.ChannelsSkipped),
				Inline: true,
			},
			{
				Name:   ".env Updated",
				Value:  fmt.Sprintf("%v", result.EnvUpdated),
				Inline: true,
			},
			{
				Name:   "Channel Mappings",
				Value:  truncateSetupString(channelList.String(), 1024),
				Inline: false,
			},
			{
				Name:   "Errors",
				Value:  errorList,
				Inline: false,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "IMPORTANT: Restart the bot to apply .env changes!",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	return embed
}

// truncateSetupString truncates a string to max length
func truncateSetupString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// respondSetupEphemeral sends an ephemeral response
func respondSetupEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}
