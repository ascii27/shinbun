package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/smtp"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/sashabaranov/go-openai"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type Config struct {
	SlackToken     string
	OpenAIToken    string
	DBHost         string
	DBPort         string
	DBName         string
	DBUser         string
	DBPassword     string
	SourceChannels []string
	// Email configuration
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	EmailFrom    string
	EmailTo      []string
}

func loadConfig() (*Config, error) {
	err := godotenv.Load()
	if err != nil {
		return nil, fmt.Errorf("error loading .env file: %v", err)
	}

	channelsStr := os.Getenv("SOURCE_CHANNELS")
	if channelsStr == "" {
		return nil, fmt.Errorf("SOURCE_CHANNELS environment variable is required")
	}
	channels := strings.Split(channelsStr, ",")

	emailToStr := os.Getenv("EMAIL_TO")
	var emailTo []string
	if emailToStr != "" {
		emailTo = strings.Split(emailToStr, ",")
	}

	config := &Config{
		SlackToken:     os.Getenv("SLACK_BOT_TOKEN"),
		OpenAIToken:    os.Getenv("OPENAI_API_KEY"),
		DBHost:         os.Getenv("DB_HOST"),
		DBPort:         os.Getenv("DB_PORT"),
		DBName:         os.Getenv("DB_NAME"),
		DBUser:         os.Getenv("DB_USER"),
		DBPassword:     os.Getenv("DB_PASSWORD"),
		SourceChannels: channels,
		// Email configuration
		SMTPHost:     os.Getenv("SMTP_HOST"),
		SMTPPort:     os.Getenv("SMTP_PORT"),
		SMTPUser:     os.Getenv("SMTP_USER"),
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		EmailFrom:    os.Getenv("EMAIL_FROM"),
		EmailTo:      emailTo,
	}

	required := map[string]string{
		"SLACK_BOT_TOKEN": config.SlackToken,
		"OPENAI_API_KEY":  config.OpenAIToken,
		"DB_HOST":         config.DBHost,
		"DB_PORT":         config.DBPort,
		"DB_NAME":         config.DBName,
		"DB_USER":         config.DBUser,
		"DB_PASSWORD":     config.DBPassword,
	}

	for k, v := range required {
		if v == "" {
			return nil, fmt.Errorf("%s is required", k)
		}
	}

	return config, nil
}

type Update struct {
	Text      string
	Timestamp string
	Link      string
	Channel   string
	Category  string
	Priority  int
}

func connectDB(config *Config) (*sql.DB, error) {
	psqlInfo := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.DBHost, config.DBPort, config.DBUser, config.DBPassword, config.DBName)

	db, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %v", err)
	}

	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("error pinging database: %v", err)
	}

	return db, nil
}

func getChannelID(api *slack.Client, db *sql.DB, channelName string, logger *zap.Logger) (string, error) {
	// First try to get the channel ID from the database
	var slackID string
	query := `SELECT slack_id FROM channels WHERE name = $1`
	err := db.QueryRow(query, channelName).Scan(&slackID)
	if err == nil {
		logger.Debug("Found channel ID in database",
			zap.String("channel_name", channelName),
			zap.String("slack_id", slackID))
		return slackID, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("error querying channel from database: %v", err)
	}

	// If not in database, fetch it from Slack
	logger.Info("Channel not found in database, fetching from Slack",
		zap.String("channel_name", channelName))

	params := &slack.GetConversationsParameters{
		ExcludeArchived: true,
		Limit:           100,
		Types:           []string{"public_channel", "private_channel"},
	}

	for {
		channels, nextCursor, err := api.GetConversations(params)
		if err != nil {
			return "", fmt.Errorf("error getting conversations: %v", err)
		}

		for _, channel := range channels {
			if channel.Name == channelName {
				logger.Info("Found channel in Slack",
					zap.String("channel_name", channelName),
					zap.String("channel_id", channel.ID))

				// Store in database for future use
				_, err := upsertChannel(db, channel.ID, channelName, logger)
				if err != nil {
					logger.Error("Failed to store channel in database",
						zap.String("channel_name", channelName),
						zap.Error(err))
				}

				return channel.ID, nil
			}
		}

		if nextCursor == "" {
			break
		}
		params.Cursor = nextCursor
	}

	return "", fmt.Errorf("channel %s not found", channelName)
}

func upsertChannel(db *sql.DB, slackID, name string, logger *zap.Logger) (int, error) {
	var id int
	query := `
		INSERT INTO channels (slack_id, name)
		VALUES ($1, $2)
		ON CONFLICT (slack_id) 
		DO UPDATE SET name = EXCLUDED.name, updated_at = CURRENT_TIMESTAMP
		RETURNING id`

	logger.Debug("Upserting channel",
		zap.String("slack_id", slackID),
		zap.String("name", name))

	err := db.QueryRow(query, slackID, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("error upserting channel: %v", err)
	}

	return id, nil
}

func getLastFetchTime(db *sql.DB, channelID int, logger *zap.Logger) (time.Time, error) {
	var lastFetched sql.NullTime
	query := `SELECT last_fetched FROM channels WHERE id = $1`

	logger.Debug("Getting last fetch time", zap.Int("channel_id", channelID))
	err := db.QueryRow(query, channelID).Scan(&lastFetched)
	if err != nil {
		return time.Time{}, fmt.Errorf("error getting last fetch time: %v", err)
	}

	if !lastFetched.Valid {
		// If no last fetch time, return a time far in the past
		return time.Now().AddDate(0, 0, -7), nil
	}

	return lastFetched.Time, nil
}

func updateLastFetchTime(db *sql.DB, channelID int, logger *zap.Logger) error {
	query := `UPDATE channels SET last_fetched = CURRENT_TIMESTAMP WHERE id = $1`

	logger.Debug("Updating last fetch time", zap.Int("channel_id", channelID))
	_, err := db.Exec(query, channelID)
	if err != nil {
		return fmt.Errorf("error updating last fetch time: %v", err)
	}

	return nil
}

func saveMessage(db *sql.DB, channelID int, msg Update, logger *zap.Logger) error {
	// Parse the Slack timestamp to a time.Time
	msgTime, err := formatTimestamp(msg.Timestamp)
	if err != nil {
		return fmt.Errorf("error parsing timestamp: %v", err)
	}

	query := `
		INSERT INTO messages (slack_id, channel_id, text, timestamp, permalink)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (slack_id) DO UPDATE
		SET text = EXCLUDED.text,
		    permalink = EXCLUDED.permalink`

	logger.Debug("Saving message",
		zap.Int("channel_id", channelID),
		zap.String("slack_id", msg.Timestamp),
		zap.Time("parsed_time", msgTime))

	_, err = db.Exec(query, msg.Timestamp, channelID, msg.Text, msgTime, msg.Link)
	if err != nil {
		return fmt.Errorf("error saving message: %v", err)
	}

	return nil
}

func getMessagesFromDB(db *sql.DB, channelID int, since time.Time, logger *zap.Logger) ([]Update, error) {
	query := `
		SELECT text, timestamp, permalink, c.name
		FROM messages m
		JOIN channels c ON m.channel_id = c.id
		WHERE channel_id = $1 AND timestamp >= $2
		ORDER BY timestamp DESC`

	rows, err := db.Query(query, channelID, since)
	if err != nil {
		return nil, fmt.Errorf("error querying messages: %v", err)
	}
	defer rows.Close()

	var updates []Update
	for rows.Next() {
		var update Update
		if err := rows.Scan(&update.Text, &update.Timestamp, &update.Link, &update.Channel); err != nil {
			return nil, fmt.Errorf("error scanning message row: %v", err)
		}
		updates = append(updates, update)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating message rows: %v", err)
	}

	return updates, nil
}

func summarizeChannel(api *slack.Client, db *sql.DB, channelID string, channelName string, since time.Time, logger *zap.Logger) ([]Update, error) {
	var updates []Update

	history, err := api.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Oldest:    fmt.Sprintf("%d", since.Unix()),
		Limit:     100,
	})
	if err != nil {
		return nil, fmt.Errorf("error getting channel history: %v", err)
	}

	skippedBots := 0
	threadReplies := 0
	processedMessages := 0

	for _, msg := range history.Messages {
		// Skip bot messages
		if msg.BotID != "" {
			skippedBots++
			continue
		}

		// Skip thread replies (only process parent messages)
		if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
			threadReplies++
			continue
		}

		// Get permalink to message
		permalink, err := api.GetPermalink(&slack.PermalinkParameters{
			Channel: channelID,
			Ts:      msg.Timestamp,
		})
		if err != nil {
			logger.Warn("Couldn't get permalink for message",
				zap.String("channel_name", channelName),
				zap.String("timestamp", msg.Timestamp),
				zap.Error(err))
			permalink = "N/A"
		}

		category, priority := categorizeMessage(channelName, msg.Text)
		updates = append(updates, Update{
			Text:      msg.Text,
			Timestamp: msg.Timestamp,
			Link:      permalink,
			Channel:   channelName,
			Category:  category,
			Priority:  priority,
		})
		processedMessages++
	}

	logger.Info("Processed messages",
		zap.String("channel_name", channelName),
		zap.Int("total_messages", len(history.Messages)),
		zap.Int("skipped_bots", skippedBots),
		zap.Int("thread_replies", threadReplies),
		zap.Int("processed_messages", processedMessages))

	return updates, nil
}

func categorizeMessage(channelName string, text string) (category string, priority int) {
	// Default to general category with low priority
	category = "general"
	priority = 1

	// Check channel name for categorization
	switch {
	case strings.Contains(channelName, "alert") || strings.Contains(channelName, "incident"):
		category = "alert"
		priority = 3
	case strings.Contains(channelName, "support"):
		category = "support"
		priority = 2
	}

	// Check message content for additional prioritization
	lowercaseText := strings.ToLower(text)
	urgentTerms := []string{"urgent", "emergency", "critical", "outage", "down", "broken", "failed", "error"}
	for _, term := range urgentTerms {
		if strings.Contains(lowercaseText, term) {
			priority++
		}
	}

	return category, priority
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatMessage(text string) string {
	// Remove Slack formatting
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "_", "")
	text = strings.ReplaceAll(text, "`", "")
	text = strings.ReplaceAll(text, "•", "-")

	// Clean up newlines
	text = strings.ReplaceAll(text, "\n\n\n", "\n")
	text = strings.ReplaceAll(text, "\n\n", "\n")

	return text
}

func formatTimestamp(timestamp string) (time.Time, error) {
	tsFloat := float64(0)
	if _, err := fmt.Sscanf(timestamp, "%f", &tsFloat); err != nil {
		return time.Time{}, fmt.Errorf("error parsing timestamp: %v", err)
	}

	// Load JST timezone
	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return time.Time{}, fmt.Errorf("error loading JST timezone: %v", err)
	}

	// Convert Unix timestamp to JST
	return time.Unix(int64(tsFloat), 0).In(jst), nil
}

func generateSummary(client *openai.Client, updates []Update, logger *zap.Logger) (string, error) {
	// Sort updates by priority
	sort.Slice(updates, func(i, j int) bool {
		return updates[i].Priority > updates[j].Priority
	})

	// Group updates by category
	var alertUpdates []Update
	var supportUpdates []Update
	var generalUpdates []Update
	var highPriorityUpdates []Update

	for _, update := range updates {
		if update.Priority >= 3 {
			highPriorityUpdates = append(highPriorityUpdates, update)
		}
		switch update.Category {
		case "alert":
			alertUpdates = append(alertUpdates, update)
		case "support":
			supportUpdates = append(supportUpdates, update)
		default:
			generalUpdates = append(generalUpdates, update)
		}
	}

	var sb strings.Builder
	sb.WriteString("Here are the messages from the last week, grouped by category:\n\n")

	// Helper function to write updates
	writeUpdates := func(updates []Update, section string) {
		if len(updates) > 0 {
			sb.WriteString(fmt.Sprintf("%s:\n", section))
			for _, update := range updates {
				msgTime, err := formatTimestamp(update.Timestamp)
				timeStr := "unknown time"
				if err == nil {
					timeStr = msgTime.Format("2006-01-02 15:04:05 JST")
				}

				sb.WriteString(fmt.Sprintf("Channel: %s\n", update.Channel))
				sb.WriteString(fmt.Sprintf("Time: %s\n", timeStr))
				sb.WriteString(fmt.Sprintf("Message: %s\n", formatMessage(update.Text)))
				sb.WriteString(fmt.Sprintf("Link: %s\n\n", update.Link))
			}
		}
	}

	// Write updates for each category
	writeUpdates(highPriorityUpdates, "High Priority Messages")
	writeUpdates(alertUpdates, "Alert Messages")
	writeUpdates(supportUpdates, "Support Messages")
	writeUpdates(generalUpdates, "General Messages")

	prompt := `You are an assistant that is providing me with important updates and information. You are going to give me key information for the week prior. I like my information presented
like a newspaper, with key information at the top, important highlights, and any urgent topics clearly called out. The remaining information should
be presented as a short summary with key highlights or takeaways that I should be aware of.

Each message includes a timestamp in JST (Japan Standard Time). Use these timestamps to provide accurate timing information in your summary.
For example, if a message is from "2025-02-01 14:30:00 JST", say "yesterday at 2:30 PM" or "on February 1st" as appropriate.
The current time is ` + time.Now().Format("2025-02-02 15:04:05 JST") + `.

Structure the summary in the following sections:

1. "Top highlights" - 3-5 bullet points of the most important items, with links to the relevant Slack messages.
2. "Urgent Incidents and Support Issues" - Bullet points of major support issues and incidents, with links to the relevant Slack message. Include any data in the information like when the incident started.
3. "General Updates" - Group and summarize other interesting topics and announcements, provide any takeaways.
4. "Support and Incident Summary" - Provide an overview of support requests and incidents, provide any takeaways and identify any follow up actions that I need.

IMPORTANT: Each message below includes a "Link:" field containing the exact Slack message URL. When referencing messages in your summary, you MUST use these exact URLs in your markdown links. Do not modify the URLs or use placeholders. Format your links as [Message Description](exact-slack-url-from-message).

Format the response in Markdown, and when including links, use proper markdown link syntax: [description](url)

After you create your summary, review the above context to make sure the summary meets those expectations both in terms of format and content. 
Also you need to double-check that the links to the slack message are correct and working links. They should be exactly the link provided in the 'Link:' field.

As for the tone, I want you to sound cheery and bright. Make it happy and fun to read with little jokes and fun comments.

Messages to summarize:
` + sb.String() + "\n\nPlease summarize these messages, making sure to use the exact Slack message URLs provided in the Link: fields above."

	logger.Info("Prompt to Open AI:" + prompt)

	logger.Info("Generating summary with OpenAI",
		zap.Int("message_count", len(updates)))

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: openai.GPT4oMini20240718,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: `You are an assistant that provides important updates and information in a newspaper style format. You analyze Slack messages and present key information, with a focus on highlighting urgent matters and providing clear takeaways.`,
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			Temperature: 0.7,
		},
	)

	if err != nil {
		return "", fmt.Errorf("error generating summary: %v", err)
	}

	return resp.Choices[0].Message.Content, nil
}

func listChannels(api *slack.Client, logger *zap.Logger) error {
	params := &slack.GetConversationsParameters{
		ExcludeArchived: true,
		Limit:           1000,
		Types:           []string{"public_channel", "private_channel"},
	}

	logger.Info("Fetching channel list from Slack")
	fmt.Println("\nAvailable channels:")

	for {
		channels, nextCursor, err := api.GetConversations(params)
		if err != nil {
			return fmt.Errorf("error getting conversations: %v", err)
		}

		for _, channel := range channels {
			fmt.Printf("- %s (ID: %s)%s\n",
				channel.Name,
				channel.ID,
				func() string {
					if channel.IsPrivate {
						return " (private)"
					}
					return ""
				}())
		}

		if nextCursor == "" {
			break
		}
		params.Cursor = nextCursor
	}

	return nil
}

func markdownToHTML(md string) string {
	// Create markdown parser with extensions
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs | parser.NoEmptyLineBeforeBlock
	p := parser.NewWithExtensions(extensions)
	doc := p.Parse([]byte(md))

	// Create HTML renderer with extensions
	htmlFlags := html.CommonFlags | html.HrefTargetBlank
	opts := html.RendererOptions{Flags: htmlFlags}
	renderer := html.NewRenderer(opts)

	return string(markdown.Render(doc, renderer))
}

func sendEmail(config *Config, subject, body string, logger *zap.Logger) error {
	if len(config.EmailTo) == 0 {
		logger.Info("No email recipients configured, skipping email send")
		return nil
	}

	if config.SMTPHost == "" || config.SMTPPort == "" {
		logger.Info("SMTP configuration not provided, skipping email send")
		return nil
	}

	auth := smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, config.SMTPHost)

	// Convert markdown to HTML
	htmlBody := markdownToHTML(body)

	// Add CSS styling
	styledHTML := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<style>
	body {
		font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
		line-height: 1.6;
		color: #333;
		max-width: 800px;
		margin: 0 auto;
		padding: 20px;
	}
	h1, h2, h3 {
		color: #2c3e50;
		margin-top: 24px;
		margin-bottom: 16px;
	}
	h1 { font-size: 28px; }
	h2 { font-size: 24px; }
	h3 { font-size: 20px; }
	a {
		color: #3498db;
		text-decoration: none;
	}
	a:hover {
		text-decoration: underline;
	}
	ul {
		padding-left: 20px;
	}
	li {
		margin: 8px 0;
	}
	code {
		background-color: #f8f9fa;
		padding: 2px 4px;
		border-radius: 3px;
		font-family: Monaco, monospace;
		font-size: 0.9em;
	}
	blockquote {
		border-left: 4px solid #e9ecef;
		margin: 0;
		padding-left: 16px;
		color: #6c757d;
	}
</style>
</head>
<body>
%s
</body>
</html>`, htmlBody)

	// Format email headers
	headers := make(map[string]string)
	headers["From"] = config.EmailFrom
	headers["To"] = strings.Join(config.EmailTo, ", ")
	headers["Subject"] = subject
	headers["MIME-Version"] = "1.0"
	headers["Content-Type"] = "text/html; charset=UTF-8"

	// Build email message
	var message strings.Builder
	for key, value := range headers {
		message.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
	}
	message.WriteString("\r\n")
	message.WriteString(styledHTML)

	// Send email
	err := smtp.SendMail(
		fmt.Sprintf("%s:%s", config.SMTPHost, config.SMTPPort),
		auth,
		config.EmailFrom,
		config.EmailTo,
		[]byte(message.String()),
	)
	if err != nil {
		return fmt.Errorf("failed to send email: %v", err)
	}

	logger.Info("Email sent successfully",
		zap.Strings("recipients", config.EmailTo))
	return nil
}

func main() {
	// Parse command line flags
	listFlag := flag.Bool("list", false, "List available channels")
	flag.Parse()

	// Initialize logger
	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Sync()

	// Load configuration
	config, err := loadConfig()
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Initialize Slack client
	slackAPI := slack.New(config.SlackToken)

	// Connect to database (needed for both modes)
	db, err := connectDB(config)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer db.Close()

	// Handle list flag
	if *listFlag {
		if err := listChannels(slackAPI, logger); err != nil {
			logger.Fatal("Failed to list channels", zap.Error(err))
		}
		return
	}

	// Initialize OpenAI client
	openaiAPI := openai.NewClient(config.OpenAIToken)

	logger.Info("Processing channels", zap.Strings("channels", config.SourceChannels))
	var allUpdates []Update
	var totalMessagesSaved int

	// Process each configured channel
	for _, channelName := range config.SourceChannels {
		channelName = strings.TrimSpace(channelName)
		if channelName == "" {
			continue
		}

		// Get Slack channel ID (from database or Slack API)
		id, err := getChannelID(slackAPI, db, channelName, logger)
		if err != nil {
			logger.Error("Failed to get channel ID",
				zap.String("channel_name", channelName),
				zap.Error(err))
			continue
		}

		// Get last fetch time
		dbChannelID, err := upsertChannel(db, id, channelName, logger)
		if err != nil {
			logger.Error("Failed to upsert channel",
				zap.String("channel_name", channelName),
				zap.Error(err))
			continue
		}

		lastFetch, err := getLastFetchTime(db, dbChannelID, logger)
		if err != nil {
			logger.Error("Failed to get last fetch time",
				zap.String("channel_name", channelName),
				zap.Error(err))
			continue
		}

		// Get updates since last fetch from Slack
		slackUpdates, err := summarizeChannel(slackAPI, db, id, channelName, lastFetch, logger)
		if err != nil {
			logger.Error("Failed to summarize channel",
				zap.String("channel_name", channelName),
				zap.Error(err))
			continue
		}

		// Get messages from the last week from database
		oneWeekAgo := time.Now().AddDate(0, 0, -7)
		dbUpdates, err := getMessagesFromDB(db, dbChannelID, oneWeekAgo, logger)
		if err != nil {
			logger.Error("Failed to get messages from database",
				zap.String("channel_name", channelName),
				zap.Error(err))
			continue
		}

		// Combine updates, avoiding duplicates
		seenMessages := make(map[string]bool)
		var updates []Update

		// Add Slack updates first (they're newer)
		for _, update := range slackUpdates {
			if !seenMessages[update.Timestamp] {
				seenMessages[update.Timestamp] = true
				updates = append(updates, update)
			}
		}

		// Add database updates
		for _, update := range dbUpdates {
			if !seenMessages[update.Timestamp] {
				seenMessages[update.Timestamp] = true
				updates = append(updates, update)
			}
		}

		logger.Info("Processing messages for channel",
			zap.String("channel_name", channelName),
			zap.Int("total_messages", len(updates)),
			zap.Int("new_messages", len(slackUpdates)),
			zap.Int("db_messages", len(dbUpdates)))

		messagesSaved := 0
		// Save only new messages to database
		for _, update := range slackUpdates {
			if err := saveMessage(db, dbChannelID, update, logger); err != nil {
				logger.Error("Failed to save message",
					zap.String("channel_name", channelName),
					zap.Error(err))
				continue
			}
			messagesSaved++
		}

		logger.Info("Saved messages for channel",
			zap.String("channel_name", channelName),
			zap.Int("messages_saved", messagesSaved),
			zap.Int("total_messages", len(updates)))

		totalMessagesSaved += messagesSaved

		// Only update last fetch time if we successfully saved messages
		if messagesSaved > 0 {
			if err := updateLastFetchTime(db, dbChannelID, logger); err != nil {
				logger.Error("Failed to update last fetch time",
					zap.String("channel_name", channelName),
					zap.Error(err))
			}
		}

		allUpdates = append(allUpdates, updates...)
	}

	logger.Info("Finished processing all channels",
		zap.Int("total_messages_saved", totalMessagesSaved),
		zap.Int("total_updates", len(allUpdates)))

	if len(allUpdates) == 0 {
		logger.Info("No new messages found")
		fmt.Println("\nNo new messages found in the last week.")
		return
	}

	// Generate summary using OpenAI
	summary, err := generateSummary(openaiAPI, allUpdates, logger)
	if err != nil {
		logger.Fatal("Failed to generate summary", zap.Error(err))
	}

	fmt.Println("\nSummary:")
	fmt.Println(summary)

	// Send email if configured
	if len(config.EmailTo) > 0 {
		subject := fmt.Sprintf("Slack Channel Summary - %s", time.Now().Format("2006-01-02"))
		if err := sendEmail(config, subject, summary, logger); err != nil {
			logger.Error("Failed to send email", zap.Error(err))
		}
	}
}
