package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/smtp"
	"os"
	"sort"
	"strconv"
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
	SlackToken           string
	OpenAIToken          string
	DBHost               string
	DBPort               string
	DBName               string
	DBUser               string
	DBPassword           string
	DefaultFocusChannels []string
	SupportFocusChannels []string
	// Email configuration
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	EmailFrom    string
	EmailTo      []string
}

type Flags struct {
	ListChannels bool
	Focus        string
	FromDateStr  string
	DryRun       bool
}

type Update struct {
	Text      string
	Timestamp string
	Link      string
	Channel   string
	Category  string
	Priority  int
}

func loadConfig() (*Config, error) {
	err := godotenv.Load()
	if err != nil {
		return nil, fmt.Errorf("error loading .env file: %v", err)
	}

	defaultChannelsStr := os.Getenv("DEFAULT_FOCUS_CHANNELS")
	if defaultChannelsStr == "" {
		return nil, fmt.Errorf("DEFAULT_FOCUS_CHANNELS environment variable is required")
	}
	defaultChannels := strings.Split(defaultChannelsStr, ",")

	supportChannelsStr := os.Getenv("SUPPORT_FOCUS_CHANNELS")
	var supportChannels []string
	if supportChannelsStr != "" {
		supportChannels = strings.Split(supportChannelsStr, ",")
	}

	emailToStr := os.Getenv("EMAIL_TO")
	var emailTo []string
	if emailToStr != "" {
		emailTo = strings.Split(emailToStr, ",")
	}

	config := &Config{
		SlackToken:           os.Getenv("SLACK_BOT_TOKEN"),
		OpenAIToken:          os.Getenv("OPENAI_API_KEY"),
		DBHost:               os.Getenv("DB_HOST"),
		DBPort:               os.Getenv("DB_PORT"),
		DBName:               os.Getenv("DB_NAME"),
		DBUser:               os.Getenv("DB_USER"),
		DBPassword:           os.Getenv("DB_PASSWORD"),
		DefaultFocusChannels: defaultChannels,
		SupportFocusChannels: supportChannels,
		SMTPHost:             os.Getenv("SMTP_HOST"),
		SMTPPort:             os.Getenv("SMTP_PORT"),
		SMTPUser:             os.Getenv("SMTP_USER"),
		SMTPPassword:         os.Getenv("SMTP_PASSWORD"),
		EmailFrom:            os.Getenv("EMAIL_FROM"),
		EmailTo:              emailTo,
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

func parseFromDate(fromDateStr string) (time.Time, error) {
	if fromDateStr == "" {
		return time.Time{}, nil
	}

	layout := "2006-01-02"
	t, err := time.Parse(layout, fromDateStr)
	if err == nil {
		year, month, day := t.Date()
		return time.Date(year, month, day, 0, 0, 0, 0, time.Local), nil
	}

	// Try parsing as a duration relative to now (e.g., "24h", "168h", "7d")
	// Handle "d" for days separately as time.ParseDuration doesn't support it
	if strings.HasSuffix(fromDateStr, "d") {
		daysStr := strings.TrimSuffix(fromDateStr, "d")
		days, err := strconv.Atoi(daysStr)
		if err == nil && days > 0 {
			// Convert days to hours
			hours := days * 24
			fromDateStr = fmt.Sprintf("%dh", hours)
		} else {
			// Invalid number of days format
			return time.Time{}, errors.New("invalid day format in --from-date duration")
		}
	}

	// Now parse the duration (original or converted from days)
	dur, err := time.ParseDuration(fromDateStr)
	if err == nil {
		if dur > 0 {
			dur = -dur
		}
		return time.Now().Add(dur), nil
	}

	return time.Time{}, errors.New("invalid --from-date format. Use YYYY-MM-DD or duration (e.g., 24h, 7d)")
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

func getChannelID(api *slack.Client, db *sql.DB, channelName string, logger *zap.Logger) (slackID string, dbID int, err error) {
	query := `SELECT id, slack_id FROM channels WHERE name = $1`
	err = db.QueryRow(query, channelName).Scan(&dbID, &slackID)
	if err == nil {
		logger.Debug("Found channel in database",
			zap.String("channel_name", channelName),
			zap.String("slack_id", slackID),
			zap.Int("db_id", dbID))
		return slackID, dbID, nil
	}
	if err != sql.ErrNoRows {
		return "", 0, fmt.Errorf("error querying channel from database: %v", err)
	}

	params := &slack.GetConversationsParameters{
		ExcludeArchived: true,
		Limit:           100,
		Types:           []string{"public_channel", "private_channel"},
	}

	for {
		channels, nextCursor, err := api.GetConversations(params)
		if err != nil {
			return "", 0, fmt.Errorf("error getting conversations: %v", err)
		}

		for _, channel := range channels {
			if channel.Name == channelName {
				logger.Info("Found channel in Slack",
					zap.String("channel_name", channelName),
					zap.String("channel_id", channel.ID))

				dbID, err := upsertChannel(db, channel.ID, channelName, logger)
				if err != nil {
					logger.Error("Failed to store channel in database",
						zap.String("channel_name", channelName),
						zap.Error(err))
				}
				return channel.ID, dbID, nil
			}
		}

		if nextCursor == "" {
			break
		}
		params.Cursor = nextCursor
	}

	return "", 0, fmt.Errorf("channel %s not found", channelName)
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
	// Aggregate stats across pages
	totalMessagesFetched := 0
	totalSkippedBots := 0
	totalThreadReplies := 0
	totalProcessedMessages := 0
	cursor := "" // Start with no cursor

	for {
		params := &slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Oldest:    fmt.Sprintf("%d", since.Unix()),
			Limit:     200, // Increased limit
			Cursor:    cursor,
		}
		history, err := api.GetConversationHistory(params)
		if err != nil {
			return nil, fmt.Errorf("error getting channel history (cursor: %s): %v", cursor, err)
		}

		totalMessagesFetched += len(history.Messages)
		pageSkippedBots := 0
		pageThreadReplies := 0
		pageProcessedMessages := 0

		// Process messages from the current page
		for _, msg := range history.Messages {
			// Skip bots, non-messages, and thread replies
			if msg.BotID != "" || msg.Type != "message" || (msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp) {
				if msg.BotID != "" || msg.Type != "message" {
					pageSkippedBots++
				}
				if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
					pageThreadReplies++
				}
				continue
			}

			permalink, err := api.GetPermalink(&slack.PermalinkParameters{
				Channel: channelID,
				Ts:      msg.Timestamp,
			})
			if err != nil {
				logger.Warn("Couldn't get permalink for message",
					zap.String("channel_name", channelName),
					zap.String("timestamp", msg.Timestamp),
					zap.Error(err))
				permalink = "N/A" // Keep original behavior
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
			pageProcessedMessages++
		}

		// Accumulate stats for this page
		totalSkippedBots += pageSkippedBots
		totalThreadReplies += pageThreadReplies
		totalProcessedMessages += pageProcessedMessages

		// Check if we need to fetch more pages
		if !history.HasMore || history.ResponseMetaData.NextCursor == "" {
			break // Exit loop if no more pages
		}
		cursor = history.ResponseMetaData.NextCursor // Set cursor for the next iteration
	}

	logger.Info("Processed messages from channel",
		zap.String("channel_name", channelName),
		zap.Int("total_messages_fetched", totalMessagesFetched),
		zap.Int("skipped_bots", totalSkippedBots),
		zap.Int("thread_replies", totalThreadReplies),
		zap.Int("processed_messages", totalProcessedMessages))

	return updates, nil
}

func categorizeMessage(channelName string, text string) (category string, priority int) {
	category = "general"
	priority = 1

	switch {
	case strings.Contains(channelName, "alert") || strings.Contains(channelName, "incident"):
		category = "alert"
		priority = 3
	case strings.Contains(channelName, "support"):
		category = "support"
		priority = 2
	}

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
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "_", "")
	text = strings.ReplaceAll(text, "`", "")
	text = strings.ReplaceAll(text, "•", "-")

	text = strings.ReplaceAll(text, "\n\n\n", "\n")
	text = strings.ReplaceAll(text, "\n\n", "\n")

	return text
}

func formatTimestamp(timestamp string) (time.Time, error) {
	tsFloat := float64(0)
	if _, err := fmt.Sscanf(timestamp, "%f", &tsFloat); err != nil {
		return time.Time{}, fmt.Errorf("error parsing timestamp: %v", err)
	}

	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return time.Time{}, fmt.Errorf("error loading JST timezone: %v", err)
	}

	return time.Unix(int64(tsFloat), 0).In(jst), nil
}

func generateSummary(client *openai.Client, updates []Update, focus string, logger *zap.Logger) (string, error) {
	sort.Slice(updates, func(i, j int) bool {
		return updates[i].Priority > updates[j].Priority
	})

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

	writeUpdates(highPriorityUpdates, "High Priority Messages")
	writeUpdates(alertUpdates, "Alert Messages")
	writeUpdates(supportUpdates, "Support Messages")
	writeUpdates(generalUpdates, "General Messages")

	var prompt string
	var systemMessage string

	switch focus {
	case "support":
		systemMessage = `You are a highly efficient support team assistant. You analyze Slack messages from support channels and provide a concise, actionable summary focused on customer issues, escalations, and resolutions. Prioritize clarity and urgency.`
		prompt = `Summarize the following support-related messages. Structure the summary into these sections:

1.  **Critical/Urgent Issues:** Bullet points for any urgent matters needing immediate attention.
2.  **New Support Requests:** Briefly list new issues raised.
3.  **Updates & Resolutions:** Summarize progress on ongoing issues or confirmed resolutions.
4.  **Statistics:** Provide a brief statistical overview including: the total number of requests/messages summarized, a breakdown of request types (if possible), components frequently mentioned, and teams involved/mentioned.

IMPORTANT: Each message below includes a \"Link:\" field containing the exact Slack message URL. When referencing messages, MUST use these exact URLs in markdown links: [Description](exact-slack-url).

Use a professional and direct tone. Focus on actionable information.

Current time for context: ` + time.Now().Format("2006-01-02 15:04 JST") + `.

Messages:
` + sb.String() + `
Please provide the support-focused summary.`

	default: // Default focus
		systemMessage = `You are a helpful assistant providing a fun, newspaper-style summary of Slack channel updates. Highlight key info and urgent items clearly.`
		prompt = `You are an assistant that is providing me with important updates and information. You are going to give me key information for the week prior. I like my information presented
like a newspaper, with key information at the top, important highlights, and any urgent topics clearly called out. The remaining information should
be presented as a short summary with key highlights or takeaways that I should be aware of.

Each message includes a timestamp in JST (Japan Standard Time). Use these timestamps to provide accurate timing information in your summary.
For example, if a message is from "2025-02-01 14:30:00 JST", say "yesterday at 2:30 PM" or "on February 1st" as appropriate.
The current time is ` + time.Now().Format("2006-01-02 15:04:05 JST") + `.

Structure the summary in the following sections:

1. "Top highlights" - 3-5 bullet points of the most important items, with links to the relevant Slack messages.
2. "Urgent Incidents and Support Issues" - Bullet points of major support issues and incidents, with links to the relevant Slack message. Include any data in the information like when the incident started.
3. "General Updates" - Group and summarize other interesting topics and announcements, provide any takeaways.
4. "Support and Incident Summary" - Provide an overview of support requests and incidents, provide any takeaways and identify any follow up actions that I need.

IMPORTANT: Each message below includes a "Link:" field containing the exact Slack message URL. When referencing messages in your summary, you MUST use these exact URLs in your markdown links. Do not modify the URLs or use placeholders. Format your links as [description](url)

After you create your summary, review the above context to make sure the summary meets those expectations both in terms of format and content. 
Also you need to double-check that the links to the slack message are correct and working links. They should be exactly the link provided in the 'Link:' field.

As for the tone, I want you to sound cheery and bright. Make it happy and fun to read with little jokes and fun comments.

Messages to summarize:
` + sb.String() + `

Please summarize these messages, making sure to use the exact Slack message URLs provided in the Link: fields above.` // End of prompt assignment

	}
	logger.Debug("Prompt to OpenAI", zap.String("focus", focus), zap.String("system_message", systemMessage), zap.String("user_prompt_prefix", prompt[:min(500, len(prompt))])) // Log prefix only

	logger.Info("Generating summary with OpenAI",
		zap.String("focus", focus),
		zap.Int("message_count", len(updates)))

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: openai.GPT4oMini20240718,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: systemMessage, // Use the selected system message
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
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs | parser.NoEmptyLineBeforeBlock
	p := parser.NewWithExtensions(extensions)
	doc := p.Parse([]byte(md))

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

	htmlBody := markdownToHTML(body)

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

	headers := make(map[string]string)
	headers["From"] = config.EmailFrom
	headers["To"] = strings.Join(config.EmailTo, ", ")
	headers["Subject"] = subject
	headers["MIME-Version"] = "1.0"
	headers["Content-Type"] = "text/html; charset=UTF-8"

	var message strings.Builder
	for key, value := range headers {
		message.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
	}
	message.WriteString("\r\n")
	message.WriteString(styledHTML)

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
	flags := Flags{}
	flag.BoolVar(&flags.ListChannels, "list-channels", false, "List available Slack channels and exit")
	flag.StringVar(&flags.Focus, "focus", "default", "Specify the channel focus category (e.g., 'default', 'support')")
	flag.StringVar(&flags.FromDateStr, "from-date", "", "Fetch messages starting from this date (YYYY-MM-DD) or duration (e.g., '24h', '7d'). Defaults to last fetch time.")
	flag.BoolVar(&flags.DryRun, "dry-run", false, "Run without sending email")
	flag.Parse()

	logger, _ := zap.NewProduction()

	config, err := loadConfig()
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	db, err := connectDB(config)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer db.Close()

	fromDate, err := parseFromDate(flags.FromDateStr)
	if err != nil {
		logger.Fatal("Invalid --from-date value", zap.Error(err))
	}

	api := slack.New(config.SlackToken)

	if flags.ListChannels {
		if err := listChannels(api, logger); err != nil {
			logger.Fatal("Failed to list channels", zap.Error(err))
		}
		return
	}

	var targetChannels []string
	switch flags.Focus {
	case "support":
		targetChannels = config.SupportFocusChannels
		if len(targetChannels) == 0 {
			logger.Fatal("Focus 'support' selected, but SUPPORT_FOCUS_CHANNELS is not defined or empty in .env")
		}
	case "default":
		targetChannels = config.DefaultFocusChannels
	default:
		logger.Warn("Unknown focus specified, using default channels", zap.String("focus", flags.Focus))
		targetChannels = config.DefaultFocusChannels
	}

	logger.Info("Starting shinbun process",
		zap.String("focus", flags.Focus),
		zap.Strings("channels", targetChannels),
		zap.String("from_date_flag", flags.FromDateStr),
		zap.Time("parsed_from_date", fromDate),
		zap.Bool("dry_run", flags.DryRun),
	)

	client := openai.NewClient(config.OpenAIToken)

	var allUpdates []Update
	var totalMessagesSaved int

	for _, channelName := range targetChannels {
		channelName = strings.TrimSpace(channelName)
		if channelName == "" {
			continue
		}

		logger.Info("Fetching channel ID", zap.String("channel", channelName))
		channelSlackID, channelDbID, err := getChannelID(api, db, channelName, logger)
		if err != nil {
			logger.Error("Failed to get channel ID", zap.String("channel", channelName), zap.Error(err))
			continue // Skip this channel if we can't get its ID
		}

		var since time.Time
		if !fromDate.IsZero() {
			since = fromDate
			logger.Info("Using --from-date flag for fetch start time",
				zap.String("channel", channelName),
				zap.Time("since", since))
		} else {
			lastFetch, err := getLastFetchTime(db, channelDbID, logger)
			if err != nil {
				logger.Error("Failed to get last fetch time", zap.String("channel", channelName), zap.Error(err))
				lastFetch = time.Now().Add(-24 * time.Hour)
				logger.Warn("Defaulting fetch time to 24 hours ago", zap.String("channel", channelName))
			}
			since = lastFetch
			logger.Info("Using last fetch time from database for fetch start time",
				zap.String("channel", channelName),
				zap.Time("since", since))
		}

		logger.Info("Summarizing channel",
			zap.String("channel", channelName),
		)

		slackUpdates, err := summarizeChannel(api, db, channelSlackID, channelName, since, logger)
		if err != nil {
			logger.Error("Failed to summarize channel", zap.String("channel", channelName), zap.Error(err))
			continue
		}

		dbUpdates, err := getMessagesFromDB(db, channelDbID, time.Now().AddDate(0, 0, -7), logger)
		if err != nil {
			logger.Error("Failed to get messages from database", zap.String("channel", channelName), zap.Error(err))
			continue
		}

		var updates []Update
		seenMessages := make(map[string]bool)

		for _, update := range slackUpdates {
			if !seenMessages[update.Timestamp] {
				seenMessages[update.Timestamp] = true
				updates = append(updates, update)
			}
		}

		for _, update := range dbUpdates {
			if !seenMessages[update.Timestamp] {
				seenMessages[update.Timestamp] = true
				updates = append(updates, update)
			}
		}

		logger.Info("Processing messages for channel",
			zap.String("channel", channelName),
			zap.Int("total_messages", len(updates)),
			zap.Int("new_messages", len(slackUpdates)),
			zap.Int("db_messages", len(dbUpdates)),
		)

		messagesSaved := 0
		for _, update := range slackUpdates {
			if err := saveMessage(db, channelDbID, update, logger); err != nil {
				logger.Error("Failed to save message", zap.String("channel", channelName), zap.Error(err))
				continue
			}
			messagesSaved++
		}

		logger.Info("Saved messages for channel",
			zap.String("channel", channelName),
			zap.Int("messages_saved", messagesSaved),
			zap.Int("total_messages", len(updates)),
		)

		totalMessagesSaved += messagesSaved

		if messagesSaved > 0 {
			err = updateLastFetchTime(db, channelDbID, logger)
			if err != nil {
				logger.Error("Failed to update last fetch time", zap.String("channel", channelName), zap.Error(err))
			}
		}

		allUpdates = append(allUpdates, updates...)
	}

	logger.Info("Finished processing all channels",
		zap.Int("total_messages_saved", totalMessagesSaved),
		zap.Int("total_updates", len(allUpdates)),
	)

	if len(allUpdates) == 0 {
		logger.Info("No updates found across monitored channels.")
		fmt.Println("\nNo new messages found in the last week.")
		return
	}

	summary, err := generateSummary(client, allUpdates, flags.Focus, logger)
	if err != nil {
		logger.Fatal("Failed to generate summary", zap.Error(err))
	}

	fmt.Println("\nSummary:")
	fmt.Println(summary)

	emailSubject := fmt.Sprintf("Shinbun Summary [%s] - %s", flags.Focus, time.Now().Format("2006-01-02"))

	if !flags.DryRun {
		if err := sendEmail(config, emailSubject, summary, logger); err != nil {
			logger.Error("Failed to send email", zap.Error(err))
		}
	} else {
		logger.Info("Dry run enabled, skipping email send.")
		fmt.Println("\n--- Email Subject ---")
		fmt.Println(emailSubject)
		fmt.Println("\n--- Email Body (HTML) ---")
		fmt.Println(summary)
	}
}
