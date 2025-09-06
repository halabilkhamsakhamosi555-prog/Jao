package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"crypto/rand"
	"math/big"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Configuration
const (
	BOT_TOKEN              = "8177342782:AAFCpxonEcOUl1K3TgfqYgoshflzxXQ_r4I"
	REQUIRED_GROUP_ID      = "@ExpectedSoon"
	REQUIRED_GROUP_CHAT_ID = -1002617494113
	MAX_FILE_SIZE          = 70 * 1024 * 1024 //700MB
	DOWNLOADS_DIR          = "downloads"
	RATE_LIMIT_DURATION    = 20 * time.Second // 30 seconds cooldown
)

// Supported platforms
var SUPPORTED_PLATFORMS = map[string]string{
	"youtube.com":   "YouTube",
	"youtu.be":      "YouTube",
	"instagram.com": "Instagram",
	"tiktok.com":    "TikTok",
	"twitter.com":   "Twitter",
	"x.com":         "Twitter",
}

// RateLimiter manages user download cooldowns
type RateLimiter struct {
	userLastDownload map[int64]time.Time
	mutex           sync.RWMutex
}

// NewRateLimiter creates a new RateLimiter instance
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		userLastDownload: make(map[int64]time.Time),
	}
}

// IsAllowed checks if user can download (not in cooldown)
func (rl *RateLimiter) IsAllowed(userID int64) bool {
	rl.mutex.RLock()
	defer rl.mutex.RUnlock()
	
	lastDownload, exists := rl.userLastDownload[userID]
	if !exists {
		return true
	}
	
	return time.Since(lastDownload) >= RATE_LIMIT_DURATION
}

// GetRemainingCooldown returns remaining cooldown time for user
func (rl *RateLimiter) GetRemainingCooldown(userID int64) time.Duration {
	rl.mutex.RLock()
	defer rl.mutex.RUnlock()
	
	lastDownload, exists := rl.userLastDownload[userID]
	if !exists {
		return 0
	}
	
	elapsed := time.Since(lastDownload)
	if elapsed >= RATE_LIMIT_DURATION {
		return 0
	}
	
	return RATE_LIMIT_DURATION - elapsed
}

// UpdateLastDownload updates the last download time for user
func (rl *RateLimiter) UpdateLastDownload(userID int64) {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	
	rl.userLastDownload[userID] = time.Now()
}

// CleanupOldEntries removes old entries to prevent memory leak
func (rl *RateLimiter) CleanupOldEntries() {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	
	cutoff := time.Now().Add(-RATE_LIMIT_DURATION * 2) // Keep entries for 2x the rate limit duration
	
	for userID, lastDownload := range rl.userLastDownload {
		if lastDownload.Before(cutoff) {
			delete(rl.userLastDownload, userID)
		}
	}
}

// VideoDownloader handles video downloading functionality
type VideoDownloader struct {
	DownloadsDir string
}

// NewVideoDownloader creates a new VideoDownloader instance
func NewVideoDownloader() *VideoDownloader {
	// Create downloads directory
	err := os.MkdirAll(DOWNLOADS_DIR, 0755)
	if err != nil {
		log.Printf("Error creating downloads directory: %v", err)
	}

	return &VideoDownloader{
		DownloadsDir: DOWNLOADS_DIR,
	}
}

// IsSupportedURL checks if URL is from supported platform
func (vd *VideoDownloader) IsSupportedURL(rawURL string) bool {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	domain := strings.ToLower(parsedURL.Host)
	// Remove www. prefix if present
	domain = strings.TrimPrefix(domain, "www.")

	for platform := range SUPPORTED_PLATFORMS {
		if strings.Contains(domain, platform) {
			return true
		}
	}
	return false
}

// GetPlatformName returns platform name from URL
func (vd *VideoDownloader) GetPlatformName(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "Unknown"
	}

	domain := strings.ToLower(parsedURL.Host)
	domain = strings.TrimPrefix(domain, "www.")

	for platform, name := range SUPPORTED_PLATFORMS {
		if strings.Contains(domain, platform) {
			return name
		}
	}
	return "Unknown"
}

// DownloadVideo downloads video from URL using yt-dlp
func (vd *VideoDownloader) DownloadVideo(videoURL string) (string, error) {
	// Generate unique filename
	randomstr, err := generateRandomString(9)
	if err != nil {
		log.Printf("Error generating random string: %v", err)
		randomstr = "fallback" // optional fallback or handle error appropriately
		
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	outputTemplate := filepath.Join(vd.DownloadsDir, fmt.Sprintf("%s_%s_%%(id)s.%%(ext)s", timestamp, randomstr))
	

	// yt-dlp command arguments
	args := []string{
		"--format", "best[filesize<70M]/best[height<=720]/best",
		"--output", outputTemplate,
		"--max-filesize", "70M",
		"--no-warnings",
		"--no-write-info-json",
		"--no-write-thumbnail",
		"--cookies", "instagram_cookies.txt",
		videoURL,
	}

	// Execute yt-dlp command
	cmd := exec.Command("yt-dlp", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("yt-dlp error: %v, output: %s", err, string(output))
		return "", fmt.Errorf("download failed:", " an error occurred while downloading the video please contact the bot owner")
	}

	// Find the downloaded file
	files, err := filepath.Glob(filepath.Join(vd.DownloadsDir, fmt.Sprintf("%s_*", timestamp)))
	if err != nil || len(files) == 0 {
		return "", fmt.Errorf("downloaded file not found")
	}

	filename := files[0]

	// Check file size
	fileInfo, err := os.Stat(filename)
	if err != nil {
		return "", fmt.Errorf("error checking file size: %v", err)
	}

	if fileInfo.Size() > MAX_FILE_SIZE {
		os.Remove(filename) // Clean up oversized file
		return "", fmt.Errorf("video is too large (>70MB)")
	}

	return filename, nil
}

// Bot represents the Telegram bot
type Bot struct {
	api         *tgbotapi.BotAPI
	downloader  *VideoDownloader
	rateLimiter *RateLimiter
}

// NewBot creates a new Bot instance
func NewBot(token string) (*Bot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	return &Bot{
		api:         bot,
		downloader:  NewVideoDownloader(),
		rateLimiter: NewRateLimiter(),
	}, nil
}

// CheckGroupMembership checks if user is member of required channel
func (b *Bot) CheckGroupMembership(userID int64) bool {
	getChatMemberConfig := tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: REQUIRED_GROUP_CHAT_ID,
			UserID: userID,
		},
	}

	member, err := b.api.GetChatMember(getChatMemberConfig)
	if err != nil {
		log.Printf("Error checking membership: %v", err)
		return false
	}

	return member.Status == "member" || member.Status == "administrator" || member.Status == "creator"
}

// HandleStart handles /start command
func (b *Bot) HandleStart(update tgbotapi.Update) {
	welcomeText := `🎬 **Social Media Video Downloader Bot**

I can download videos from:
• YouTube
• Instagram (Posts & Reels)
• TikTok
• Twitter/X

**Requirements:**
You must join our channel to use this bot!

**Rate Limiting:**
After downloading a video, you must wait 20 seconds before downloading another one.

Simply send me any video URL and I'll download it for you.

**Commands:**
/start - Show this message
/help - Get help
/check - Check channel membership`

	joinButton := tgbotapi.NewInlineKeyboardButtonURL("Join Channel", 
		fmt.Sprintf("https://t.me/%s", strings.TrimPrefix(REQUIRED_GROUP_ID, "@")))
	keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(joinButton))

	msg := tgbotapi.NewMessage(update.Message.Chat.ID, welcomeText)
	msg.ParseMode = tgbotapi.ModeMarkdown
	msg.ReplyMarkup = keyboard

	b.api.Send(msg)
}

// HandleHelp handles /help command
func (b *Bot) HandleHelp(update tgbotapi.Update) {
	helpText := `**How to use:**

1️⃣ First, join our required group
2️⃣ Send any video URL from supported platforms
3️⃣ Wait for the download to complete
4️⃣ Wait 20 seconds before downloading another video
5️⃣ Enjoy your videos!

**Supported platforms:**
• YouTube (youtube.com, youtu.be)
• Instagram (instagram.com)
• TikTok (tiktok.com)
• Twitter/X (twitter.com, x.com)

**Limitations:**
• Max file size: 70MB
• Must be a member of our group
• Videos only (no audio-only content)
• 20-second cooldown between downloads

**Commands:**
/start - Welcome message
/help - This help message
/check - Check your group membership status`

	msg := tgbotapi.NewMessage(update.Message.Chat.ID, helpText)
	msg.ParseMode = tgbotapi.ModeMarkdown
	b.api.Send(msg)
}

// HandleCheckMembership handles /check command
func (b *Bot) HandleCheckMembership(update tgbotapi.Update) {
	userID := update.Message.From.ID
	isMember := b.CheckGroupMembership(userID)

	if isMember {
		// Also show rate limit status
		remainingCooldown := b.rateLimiter.GetRemainingCooldown(userID)
		statusText := "✅ You are a member of our group! You can download videos."
		
		if remainingCooldown > 0 {
			statusText += fmt.Sprintf("\n\n⏳ Cooldown remaining: %d seconds", int(remainingCooldown.Seconds()))
		} else {
			statusText += "\n\n🟢 You can download a video now!"
		}
		
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, statusText)
		b.api.Send(msg)
	} else {
		joinButton := tgbotapi.NewInlineKeyboardButtonURL("Join Group", 
			fmt.Sprintf("https://t.me/%s", strings.TrimPrefix(REQUIRED_GROUP_ID, "@")))
		keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(joinButton))

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "❌ You are not a member of our group. Please join to use this bot!")
		msg.ReplyMarkup = keyboard
		b.api.Send(msg)
	}
}

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func generateRandomString(length int) (string, error) {
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[num.Int64()]
	}
	return string(result), nil
}

// HandleURL handles video URL messages
func (b *Bot) HandleURL(update tgbotapi.Update) {
	userID := update.Message.From.ID
	videoURL := strings.TrimSpace(update.Message.Text)

	// Check if user is member of required group
	isMember := b.CheckGroupMembership(userID)
	if !isMember {
		joinButton := tgbotapi.NewInlineKeyboardButtonURL("Join Group", 
			fmt.Sprintf("https://t.me/%s", strings.TrimPrefix(REQUIRED_GROUP_ID, "@")))
		keyboard := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(joinButton))

		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "❌ You must join our group to download videos!")
		msg.ReplyMarkup = keyboard
		b.api.Send(msg)
		return
	}

	// Check rate limiting
	if !b.rateLimiter.IsAllowed(userID) {
		remainingCooldown := b.rateLimiter.GetRemainingCooldown(userID)
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, 
			fmt.Sprintf("⏳ Please wait %d seconds before downloading another video!", int(remainingCooldown.Seconds())))
		b.api.Send(msg)
		return
	}

	// Check if URL is supported
	if !b.downloader.IsSupportedURL(videoURL) {
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, 
			"❌ Unsupported URL. Please send a link from:\n• YouTube\n• Instagram\n• TikTok\n• Twitter/X")
		b.api.Send(msg)
		return
	}

	// Show downloading message
	platform := b.downloader.GetPlatformName(videoURL)
	statusMsg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("⬇️ Downloading %s video...", platform))
	sentMsg, err := b.api.Send(statusMsg)
	if err != nil {
		log.Printf("Error sending status message: %v", err)
		return
	}

	// Download the video
	filename, err := b.downloader.DownloadVideo(videoURL)
	if err != nil {
		log.Printf("❌ Error: %v", err)
		editMsg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, 
			fmt.Sprintf("❌ Error: ", "please contact bot owner" ))
		b.api.Send(editMsg)
		return
	}

	// Update rate limiter (user successfully initiated download)
	b.rateLimiter.UpdateLastDownload(userID)

	// Update status
	editMsg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, "📤 Uploading video...")
	b.api.Send(editMsg)

	// Send the video
	videoMsg := tgbotapi.NewVideo(update.Message.Chat.ID, tgbotapi.FilePath(filename))
	videoMsg.Caption = fmt.Sprintf("✅ Downloaded from %s\n\n⏳ Next download available in 20 seconds", platform)
	videoMsg.ReplyToMessageID = update.Message.MessageID

	_, err = b.api.Send(videoMsg)
	if err != nil {
		log.Printf("Error sending video: %v", err)
		editMsg := tgbotapi.NewEditMessageText(update.Message.Chat.ID, sentMsg.MessageID, 
			fmt.Sprintf("❌ Error uploading video: %v", err))
		b.api.Send(editMsg)
	} else {
		// Delete status message
		deleteMsg := tgbotapi.NewDeleteMessage(update.Message.Chat.ID, sentMsg.MessageID)
		b.api.Send(deleteMsg)
	}

	
	os.Remove(filename)
}

// HandleMessage handles non-URL messages
func (b *Bot) HandleMessage(update tgbotapi.Update) {
	text := strings.ToLower(update.Message.Text)

	var responseText string
	if strings.Contains(text, "youtube") || strings.Contains(text, "instagram") || 
	   strings.Contains(text, "tiktok") || strings.Contains(text, "twitter") {
		responseText = "Please send a direct URL to the video you want to download."
	} else {
		responseText = "Send me a video URL from YouTube, Instagram, TikTok, or Twitter/X to download it!"
	}

	msg := tgbotapi.NewMessage(update.Message.Chat.ID, responseText)
	b.api.Send(msg)
}

// startCleanupRoutine starts a background routine to clean up old rate limit entries
func (b *Bot) startCleanupRoutine() {
	ticker := time.NewTicker(5 * time.Minute) // Clean up every 5 minutes
	go func() {
		for range ticker.C {
			b.rateLimiter.CleanupOldEntries()
		}
	}()
}

// Start starts the bot
func (b *Bot) Start() {
	// Start cleanup routine
	b.startCleanupRoutine()
	
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	// URL regex pattern
	urlRegex := regexp.MustCompile(`https?://(?:[a-zA-Z]|[0-9]|[$-_@.&+]|[!*\\(\\),]|(?:%[0-9a-fA-F][0-9a-fA-F]))+`)

	log.Println("🤖 Social Media Downloader Bot starting...")
	log.Printf("📢 Required group: %s", REQUIRED_GROUP_ID)
	log.Printf("⏳ Rate limit: %v", RATE_LIMIT_DURATION)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		// Handle commands
		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				b.HandleStart(update)
			case "help":
				b.HandleHelp(update)
			case "check":
				b.HandleCheckMembership(update)
			}
			continue
		}

		// Handle URLs
		if urlRegex.MatchString(update.Message.Text) {
			go b.HandleURL(update) // Handle in goroutine to avoid blocking
			continue
		}

		// Handle other messages
		b.HandleMessage(update)
	}
}

func main() {
	bot, err := NewBot(BOT_TOKEN)
	if err != nil {
		log.Panic(err)
	}

	bot.Start()
}

// go.mod file content:
/*
module telegram-video-downloader

go 1.21

require github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
*/