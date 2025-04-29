package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	goopenai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"shinbun/internal/commontypes"
)

const maxPromptLength = 3800 // Reduced slightly to be safer

// GenerateSummary sends updates to OpenAI and returns a markdown summary.
func GenerateSummary(client *goopenai.Client, updates []commontypes.Update, focus string, logger *zap.Logger) (string, error) {
	if len(updates) == 0 {
		return "No new updates found.", nil
	}

	// Sort updates: Priority, then Timestamp
	sort.SliceStable(updates, func(i, j int) bool {
		if updates[i].Priority != updates[j].Priority {
			return updates[i].Priority < updates[j].Priority // Lower priority number first
		}
		tsI, errI := FormatTimestamp(updates[i].Timestamp)
		tsJ, errJ := FormatTimestamp(updates[j].Timestamp)
		if errI != nil || errJ != nil {
			return updates[i].Timestamp < updates[j].Timestamp // Fallback sort
		}
		return tsI.Before(tsJ) // Older first
	})

	// --- Prepare Prompt --- //
	var sb strings.Builder
	currentTokenCount := 0
	includedMessages := 0

	// Build message list string, respecting token limits
	for i := len(updates) - 1; i >= 0; i-- { // Process newest first for prompt
		u := updates[i]
		formattedTime, timeErr := formatTimestamp(u.Timestamp)
		if timeErr != nil {
			logger.Warn("Failed to format timestamp, skipping", zap.String("timestamp", u.Timestamp), zap.Error(timeErr))
			continue
		}
		// Use Channel instead of non-existent Username
		messageLine := fmt.Sprintf("[%s] #%s: %s Link: <%s|View Message>\n",
			formattedTime,
			u.Channel, // Changed from u.Username
			formatMessage(u.Text),
			u.Link,
		)

		// Simple token estimation (words)
		lineTokens := len(strings.Fields(messageLine))

		if currentTokenCount+lineTokens > maxPromptLength {
			logger.Info("Reached token limit for prompt, stopping message inclusion",
				zap.Int("included_messages", includedMessages),
				zap.Int("total_messages", len(updates)),
				zap.Int("current_tokens", currentTokenCount),
				zap.Int("next_line_tokens", lineTokens),
			)
			break // Stop adding messages if limit exceeded
		}

		sb.WriteString(messageLine)
		currentTokenCount += lineTokens
		includedMessages++
	}

	if sb.Len() == 0 {
		return "No processable messages found within token limits.", nil // Handle case where even the first message is too long
	}

	// --- Select Prompt Template based on Focus --- //
	var promptTemplate string
	switch focus {
	case "support":
		promptTemplate = `Summarize the following support-related messages. Structure the summary into these sections:

1.  **Critical/Urgent Issues:** Bullet points for any urgent matters needing immediate attention.
2.  **New Support Requests:** Briefly list new issues raised.
3.  **Updates & Resolutions:** Summarize progress on ongoing issues or confirmed resolutions.
4.  **Statistics:** Provide a brief statistical overview including: the total number of requests/messages summarized, a breakdown of request types (if possible), components frequently mentioned, and teams involved/mentioned.

IMPORTANT: Each message below includes a "Link:" field containing the exact Slack message URL. When referencing messages, MUST use these exact URLs in markdown links: [Description](exact-slack-url).

Use a professional and direct tone. Focus on actionable information.

Current time for context: {{.CurrentTime}}.

Messages:
{{.Messages}}`
	default: // Default focus prompt (Newspaper style)
		promptTemplate = `You are an assistant that is providing me with important updates and information. You are going to give me key information for the week prior. I like my information presented
like a newspaper, with key information at the top, important highlights, and any urgent topics clearly called out. The remaining information should
be presented as a short summary with key highlights or takeaways that I should be aware of.

Each message includes a timestamp in JST (Japan Standard Time). Use these timestamps to provide accurate timing information in your summary.
For example, if a message is from "2025-02-01 14:30:00 JST", say "yesterday at 2:30 PM" or "on February 1st" as appropriate.
The current time is {{.CurrentTime}}.

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
{{.Messages}}

Please summarize these messages, making sure to use the exact Slack message URLs provided in the Link: fields above.`
	}

	// --- Populate Template --- //
	tmpl, err := template.New("prompt").Parse(promptTemplate)
	if err != nil {
		logger.Error("Failed to parse prompt template", zap.Error(err), zap.String("focus", focus))
		return "", fmt.Errorf("internal error: failed to parse prompt template: %w", err)
	}

	jst := time.FixedZone("JST", 9*60*60)
	data := struct {
		CurrentTime string
		Messages    string
	}{
		CurrentTime: time.Now().In(jst).Format("2006-01-02 15:04 JST"),
		Messages:    sb.String(),
	}

	var promptBuf bytes.Buffer
	if err := tmpl.Execute(&promptBuf, data); err != nil {
		logger.Error("Failed to execute prompt template", zap.Error(err), zap.String("focus", focus))
		return "", fmt.Errorf("internal error: failed to execute prompt template: %w", err)
	}
	prompt := promptBuf.String()

	logger.Debug("Generated OpenAI Prompt", zap.String("focus", focus), zap.Int("message_count", includedMessages), zap.Int("prompt_length_chars", len(prompt)))

	// --- Call OpenAI API --- //
	logger.Info("Sending request to OpenAI", zap.String("focus", focus), zap.Int("included_messages", includedMessages))
	resp, err := client.CreateChatCompletion(
		context.Background(),
		goopenai.ChatCompletionRequest{
			Model: goopenai.GPT4TurboPreview,
			Messages: []goopenai.ChatCompletionMessage{
				{Role: goopenai.ChatMessageRoleSystem, Content: "You summarize Slack messages into markdown digests."},
				{Role: goopenai.ChatMessageRoleUser, Content: prompt},
			},
			MaxTokens:   1000,
			Temperature: 0.3,
		},
	)

	if err != nil {
		return "", fmt.Errorf("openai error: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		return "", errors.New("openai returned empty summary")
	}
	logger.Info("Summary generated successfully")
	return resp.Choices[0].Message.Content, nil
}

// FormatTimestamp parses Slack timestamp string.
func FormatTimestamp(timestamp string) (time.Time, error) {
	parts := strings.Split(timestamp, ".")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid ts: %s", timestamp)
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid sec: %v", err)
	}
	nsecStr := parts[1]
	nsec := int64(0)
	if len(nsecStr) > 0 {
		if len(nsecStr) > 6 {
			nsecStr = nsecStr[:6]
		}
		for len(nsecStr) < 6 {
			nsecStr += "0"
		}
		nsec, err = strconv.ParseInt(nsecStr, 10, 64)
		if err != nil {
			nsec = 0 // Fallback
		}
		nsec *= 1000 // Micro to nano
	}
	return time.Unix(sec, nsec), nil
}

// formatMessage formats a single message string for the prompt.
func formatMessage(text string) string {
	// Simple formatting for now, could expand later (e.g., handle code blocks)
	return strings.TrimSpace(text)
}

// formatTimestamp formats a Slack timestamp string (e.g., "1618377073.000100") into a readable format.
func formatTimestamp(slackTs string) (string, error) {
	parts := strings.Split(slackTs, ".")
	if len(parts) < 1 {
		return "", errors.New("invalid timestamp format")
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid seconds in timestamp: %w", err)
	}

	t := time.Unix(sec, 0) // Use seconds part for Time object
	// Format as YYYY-MM-DD HH:MM:SS JST
	jst := time.FixedZone("JST", 9*60*60)
	return t.In(jst).Format("2006-01-02 15:04:05 JST"), nil
}

// min returns the smaller of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
