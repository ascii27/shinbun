package slack

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"shinbun/internal/commontypes"
)

// GetChannelID finds Slack and DB IDs for a channel name.
func GetChannelID(api *slack.Client, db *sql.DB, channelName string, logger *zap.Logger) (slackID string, dbID int, err error) {
	query := `SELECT id, slack_id FROM channels WHERE name = $1`
	err = db.QueryRow(query, channelName).Scan(&dbID, &slackID)
	if err == nil {
		return slackID, dbID, nil // Found in DB
	}
	if err != sql.ErrNoRows {
		return "", 0, fmt.Errorf("db query error: %v", err)
	}

	logger.Info("Channel not in DB, querying Slack API", zap.String("channel", channelName))
	params := &slack.GetConversationsParameters{
		ExcludeArchived: true,
		Limit:           1000,
		Types:           []string{"public_channel", "private_channel"},
	}
	for {
		channels, nextCursor, err_api := api.GetConversations(params)
		if err_api != nil {
			return "", 0, fmt.Errorf("slack api error: %v", err_api)
		}
		for _, ch := range channels {
			if ch.Name == channelName {
				dbID, err_upsert := UpsertChannel(db, ch.ID, ch.Name, logger)
				if err_upsert != nil {
					logger.Error("Failed to upsert found channel", zap.Error(err_upsert))
					// Return slack ID even if upsert fails
					return ch.ID, 0, nil
				}
				return ch.ID, dbID, nil
			}
		}
		if nextCursor == "" {
			break
		}
		params.Cursor = nextCursor
		time.Sleep(500 * time.Millisecond) // Basic rate limit
	}
	return "", 0, fmt.Errorf("channel '%s' not found", channelName)
}

// UpsertChannel inserts or updates channel name/ID in DB.
func UpsertChannel(db *sql.DB, slackID, name string, logger *zap.Logger) (int, error) {
	var id int
	query := `INSERT INTO channels (slack_id, name) VALUES ($1, $2) ON CONFLICT (slack_id) DO UPDATE SET name = $2 RETURNING id`
	err := db.QueryRow(query, slackID, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert error: %v", err)
	}
	return id, nil
}

// SummarizeChannel fetches and processes messages from a channel since a time.
func SummarizeChannel(api *slack.Client, db *sql.DB, channelName string, fromDateOverride time.Time, logger *zap.Logger) ([]commontypes.Update, error) {
	// Ensure DB is available if needed for fetching time or updating
	if db == nil {
		// If no DB, we must have a fromDateOverride, otherwise we don't know where to start
		if fromDateOverride.IsZero() {
			return nil, fmt.Errorf("database connection is nil and no --from-date override provided for channel '%s'", channelName)
		}
		logger.Warn("Database connection is nil, cannot track last fetch time", zap.String("channel", channelName))
	}

	// 1. Get Channel Slack ID and DB ID
	channelSlackID, channelDBID, err := getChannelID(api, db, channelName, logger)
	if err != nil {
		// If DB was nil, getChannelID would fail if channel not found in Slack either.
		return nil, fmt.Errorf("failed to get channel info for '%s': %w", channelName, err)
	}

	// 2. Determine 'since' time
	var since time.Time
	if !fromDateOverride.IsZero() {
		since = fromDateOverride
		logger.Info("Using --from-date override for fetch start time",
			zap.String("channel", channelName),
			zap.Time("since", since))
	} else if db != nil { // Only use DB if it's available
		lastFetch, fetchErr := getLastFetchTime(db, channelDBID, logger)
		if fetchErr != nil {
			logger.Error("Failed to get last fetch time from DB, using default (7 days ago)",
				zap.String("channel", channelName),
				zap.Error(fetchErr))
			// Provide a sensible default if DB fetch fails
			since = time.Now().AddDate(0, 0, -7)
		} else {
			since = lastFetch
			logger.Info("Using last fetch time from database",
				zap.String("channel", channelName),
				zap.Time("since", since))
		}
	} else {
		// Should not happen due to initial check, but safeguard
		return nil, fmt.Errorf("internal error: DB is nil and fromDateOverride is zero for channel '%s'", channelName)
	}

	// 3. Fetch Messages from Slack
	var updates []commontypes.Update
	cursor := ""
	logger.Info("Fetching messages from Slack",
		zap.String("channel", channelName),
		zap.String("channel_id", channelSlackID),
		zap.Time("since", since),
	)
	for {
		params := &slack.GetConversationHistoryParameters{
			ChannelID: channelSlackID,
			Oldest:    fmt.Sprintf("%.6f", float64(since.UnixNano())/1e9), // Use float for precision
			Limit:     200, // Max allowed by Slack API
			Cursor:    cursor,
		}
		history, err := api.GetConversationHistory(params)
		if err != nil {
			// Check for specific errors like 'channel_not_found'
			if strings.Contains(err.Error(), "channel_not_found") {
				logger.Error("Slack API Error: Channel not found", zap.String("channel_id", channelSlackID), zap.Error(err))
				return nil, fmt.Errorf("slack channel '%s' (ID: %s) not found or bot lacks permission", channelName, channelSlackID)
			}
			// Handle rate limiting specifically if possible (check error message)
			if strings.Contains(err.Error(), "ratelimited") {
				logger.Warn("Rate limited by Slack API, pausing...")
				time.Sleep(30 * time.Second) // Wait longer if rate limited
				continue // Retry the same request
			}
			// Generic error
			return nil, fmt.Errorf("failed to get Slack conversation history for '%s': %w", channelName, err)
		}

		logger.Debug("Received message batch",
			zap.String("channel", channelName),
			zap.Int("count", len(history.Messages)),
			zap.Bool("has_more", history.HasMore),
		)

		for _, msg := range history.Messages {
			// Skip bot messages, channel join/leave messages, etc.
			if msg.BotID != "" || msg.SubType != "" {
				continue
			}
			// Skip thread replies that aren't also broadcast to the channel
			if msg.ThreadTimestamp != "" && msg.SubType != "thread_broadcast" {
				continue
			}

			// Get Permalink
			permalink, pErr := api.GetPermalink(&slack.PermalinkParameters{Channel: channelSlackID, Ts: msg.Timestamp})
			if pErr != nil {
				logger.Warn("Failed to get permalink, constructing fallback",
					zap.String("channel", channelName),
					zap.String("timestamp", msg.Timestamp),
					zap.Error(pErr))
				// Construct a fallback URL (may not work for private channels without auth)
				permalink = fmt.Sprintf("https://slack.com/archives/%s/p%s", channelSlackID, strings.Replace(msg.Timestamp, ".", "", 1))
			}

			// Categorize
			category, priority := categorizeMessage(channelName, msg.Text) // Pass channelName for context

			// Create Update struct
			update := commontypes.Update{
				Text:      msg.Text,
				Timestamp: msg.Timestamp,
				Link:      permalink,
				Channel:   channelName,
				Category:  category,
				Priority:  priority,
			}
			updates = append(updates, update)

			// Optionally save message to DB if needed for other purposes (currently not used)
			// if db != nil {
			// 	if err := saveMessage(db, channelDBID, update, logger); err != nil {
			// 		logger.Error("Failed to save message to DB", zap.Error(err))
			// 		// Decide if this should be fatal or just a warning
			// 	}
			// }
		}

		// Check if finished pagination
		if !history.HasMore || history.ResponseMetaData.NextCursor == "" {
			logger.Debug("Finished fetching pages for channel", zap.String("channel", channelName))
			break
		}

		// Prepare for next page
		cursor = history.ResponseMetaData.NextCursor
		logger.Debug("Moving to next page", zap.String("channel", channelName), zap.String("cursor", cursor))
		// Basic rate limiting: Pause between pages
		time.Sleep(1200 * time.Millisecond) // Slack Tier 2 allows ~50 requests/min, Tier 3 ~100/min. Be conservative.
	}

	// 4. Update Last Fetch Time in DB (if DB is available)
	if db != nil {
		if err := updateLastFetchTime(db, channelDBID, logger); err != nil {
			// Log error but don't fail the whole process, as messages were fetched
			logger.Error("Failed to update last fetch time in DB",
				zap.String("channel", channelName),
				zap.Int("db_id", channelDBID),
				zap.Error(err))
		} else {
			logger.Info("Successfully updated last fetch time in DB", zap.String("channel", channelName))
		}
	}

	logger.Info("Finished processing channel",
		zap.String("channel", channelName),
		zap.Int("updates_found", len(updates)),
	)
	return updates, nil
}

// --- Internal Helper Functions ---

// getChannelID finds Slack and DB IDs for a channel name.
// It first checks the DB, then Slack, and upserts the channel info into the DB.
// IMPORTANT: Requires db connection to be non-nil if you expect DB lookup/upsert.
func getChannelID(api *slack.Client, db *sql.DB, channelName string, logger *zap.Logger) (slackID string, dbID int, err error) {
	// 1. Try fetching from DB first (if available)
	if db != nil {
		query := `SELECT id, slack_id FROM channels WHERE name = $1`
		err = db.QueryRow(query, channelName).Scan(&dbID, &slackID)
		if err == nil {
			logger.Debug("Found channel in database cache",
				zap.String("channel_name", channelName),
				zap.String("slack_id", slackID),
				zap.Int("db_id", dbID))
			return slackID, dbID, nil // Found in DB
		}
		if err != sql.ErrNoRows {
			// DB query failed for a reason other than not found
			return "", 0, fmt.Errorf("error querying channel '%s' from database: %w", channelName, err)
		}
		// Not found in DB (err == sql.ErrNoRows), proceed to check Slack
		logger.Debug("Channel not found in DB cache, checking Slack API", zap.String("channel_name", channelName))
	}

	// 2. Fetch from Slack API (if not found in DB or DB is nil)
	logger.Info("Fetching channel list from Slack to find ID", zap.String("target_channel", channelName))
	params := &slack.GetConversationsParameters{
		ExcludeArchived: true,
		Limit:           1000, // Fetch more channels at once
		Types:           []string{"public_channel", "private_channel"}, // Include private if bot is invited
	}
	cursor := ""
	found := false
	for {
		params.Cursor = cursor
		channels, nextCursor, listErr := api.GetConversations(params)
		if listErr != nil {
			return "", 0, fmt.Errorf("error getting conversations from Slack: %w", listErr)
		}

		for _, channel := range channels {
			if channel.Name == channelName {
				slackID = channel.ID
				found = true
				logger.Info("Found channel via Slack API",
					zap.String("channel_name", channelName),
					zap.String("slack_id", slackID))
				break // Exit inner loop once found
			}
		}

		if found || nextCursor == "" {
			break // Exit outer loop if found or no more pages
		}
		cursor = nextCursor
		time.Sleep(500 * time.Millisecond) // Be nice to the API
	}

	if !found {
		return "", 0, fmt.Errorf("channel '%s' not found via Slack API", channelName)
	}

	// 3. Upsert channel info into DB (if DB is available)
	if db != nil {
		dbID, upsertErr := upsertChannel(db, slackID, channelName, logger)
		if upsertErr != nil {
			// Log error but return the found slackID, as the main goal was finding it
			logger.Error("Failed to upsert channel into database, proceeding without DB ID",
				zap.String("channel_name", channelName),
				zap.String("slack_id", slackID),
				zap.Error(upsertErr))
			return slackID, 0, nil // Return slackID, but 0 for dbID and nil error (best effort)
		}
		// Successfully upserted
		return slackID, dbID, nil
	}

	// DB was nil, but we found the channel in Slack
	return slackID, 0, nil
}

// upsertChannel inserts or updates a channel in the database.
func upsertChannel(db *sql.DB, slackID, name string, logger *zap.Logger) (int, error) {
	var id int
	// Ensure db is not nil before proceeding
	if db == nil {
		return 0, fmt.Errorf("cannot upsert channel: database connection is nil")
	}

	query := `
		INSERT INTO channels (slack_id, name, created_at, updated_at)
		VALUES ($1, $2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (slack_id)
		DO UPDATE SET name = EXCLUDED.name, updated_at = CURRENT_TIMESTAMP
		RETURNING id`

	logger.Debug("Upserting channel into database",
		zap.String("slack_id", slackID),
		zap.String("name", name))

	err := db.QueryRow(query, slackID, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("error upserting channel '%s' (ID: %s): %w", name, slackID, err)
	}
	logger.Debug("Channel upsert successful", zap.Int("db_id", id))
	return id, nil
}

// getLastFetchTime retrieves the last fetch timestamp for a channel from the database.
func getLastFetchTime(db *sql.DB, channelDBID int, logger *zap.Logger) (time.Time, error) {
	var lastFetchStr sql.NullTime
	// Ensure db is not nil
	if db == nil {
		return time.Time{}, fmt.Errorf("cannot get last fetch time: database connection is nil")
	}
	if channelDBID == 0 {
		return time.Time{}, fmt.Errorf("cannot get last fetch time: invalid channel DB ID 0")
	}

	query := `SELECT last_fetched FROM channels WHERE id = $1`
	logger.Debug("Querying last fetch time from DB", zap.Int("db_id", channelDBID))
	err := db.QueryRow(query, channelDBID).Scan(&lastFetchStr)
	if err != nil {
		if err == sql.ErrNoRows {
			// This shouldn't happen if upsertChannel worked, but handle defensively
			logger.Warn("No DB entry found for channel ID to get last fetch time", zap.Int("db_id", channelDBID))
			return time.Time{}, fmt.Errorf("no database record found for channel id %d", channelDBID)
		}
		return time.Time{}, fmt.Errorf("error querying last fetch time for DB ID %d: %w", channelDBID, err)
	}

	if !lastFetchStr.Valid || lastFetchStr.Time.IsZero() {
		logger.Info("No valid last fetch timestamp found in DB, using default (7 days ago)", zap.Int("db_id", channelDBID))
		return time.Now().AddDate(0, 0, -7), nil
	}

	logger.Debug("Found last fetch timestamp in DB", zap.Int("db_id", channelDBID), zap.Time("timestamp", lastFetchStr.Time))
	return lastFetchStr.Time, nil
}

// updateLastFetchTime updates the last fetch timestamp for a channel in the database.
func updateLastFetchTime(db *sql.DB, channelDBID int, logger *zap.Logger) error {
	// Ensure db is not nil and ID is valid
	if db == nil {
		return fmt.Errorf("cannot update last fetch time: database connection is nil")
	}
	if channelDBID == 0 {
		return fmt.Errorf("cannot update last fetch time: invalid channel DB ID 0")
	}

	query := `UPDATE channels SET last_fetched = CURRENT_TIMESTAMP WHERE id = $1`
	logger.Debug("Updating last fetch time in DB", zap.Int("db_id", channelDBID))
	result, err := db.Exec(query, channelDBID)
	if err != nil {
		return fmt.Errorf("error executing last fetch time update for DB ID %d: %w", channelDBID, err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		logger.Warn("Update last fetch time affected 0 rows", zap.Int("db_id", channelDBID))
		// This might indicate the channel was deleted between get and update, or another issue.
		// Return nil error as the command executed, but log the warning.
	}
	return nil
}

// categorizeMessage analyzes message text to assign a category and priority.
func categorizeMessage(channelName, text string) (string, int) {
	lowerText := strings.ToLower(text)
	lowerChannelName := strings.ToLower(channelName)

	// Define keywords for categorization
	alertKeywords := []string{"alert", "incident", "down", " outage", "critical", "p0", "sev0", "sev1"}
	supportKeywords := []string{"support request", "customer issue", "ticket", "bug report", "need help", "assistance"}
	highPriorityKeywords := []string{"urgent", "asap", "blocker", "immediately"}

	// Priorities: 3=High, 2=Medium, 1=Low
	priority := 1 // Default low

	// Check for high priority keywords first
	for _, keyword := range highPriorityKeywords {
		if strings.Contains(lowerText, keyword) {
			priority = 3
			break
		}
	}

	// Determine category
	for _, keyword := range alertKeywords {
		if strings.Contains(lowerText, keyword) {
			// If it wasn't already marked high priority, make it high
			if priority < 3 { priority = 3 }
			return "alert", priority
		}
	}

	// Check channel name context for support
	if strings.Contains(lowerChannelName, "support") || strings.Contains(lowerChannelName, "customer") || strings.Contains(lowerChannelName, "help") {
		// Support keywords in support channels are more likely relevant
		for _, keyword := range supportKeywords {
			if strings.Contains(lowerText, keyword) {
				// Make support requests medium priority unless marked urgent
				if priority < 2 { priority = 2 }
				return "support", priority
			}
		}
		// Default category in a support channel is 'support' unless it matches alert keywords
		if priority < 2 { priority = 2 } // Bump default priority in support channels
		return "support", priority
	}

	// General check for support keywords outside support channels
	for _, keyword := range supportKeywords {
		if strings.Contains(lowerText, keyword) {
			if priority < 2 { priority = 2 }
			return "support", priority
		}
	}

	return "general", priority // Default
}

// ListChannels fetches and prints available channels.
func ListChannels(api *slack.Client, logger *zap.Logger) error {
	params := &slack.GetConversationsParameters{ExcludeArchived: true, Types: []string{"public_channel", "private_channel"}, Limit: 1000}
	var chans []slack.Channel
	cursor := ""
	for {
		params.Cursor = cursor
		pageChans, nextCursor, err := api.GetConversations(params)
		if err != nil { return fmt.Errorf("list err: %v", err) }
		chans = append(chans, pageChans...)
		if nextCursor == "" { break }
		cursor = nextCursor
		time.Sleep(500 * time.Millisecond)
	}
	sort.Slice(chans, func(i, j int) bool { return chans[i].Name < chans[j].Name })
	fmt.Println("Available Channels:")
	for _, ch := range chans {
		typeStr := "Public"; if ch.IsPrivate { typeStr = "Private" }
		fmt.Printf("- %s (ID: %s, Type: %s)\n", ch.Name, ch.ID, typeStr)
	}
	return nil
}

func min(a, b int) int { if a < b { return a }; return b }
