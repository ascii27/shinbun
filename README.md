# Shinbun

Shinbun (新聞) is a Slack channel digest tool that helps you stay on top of your Slack conversations by providing summaries and action items from configured channels.

## Features

- Connect to Slack workspace and fetch messages from configured channels
- Generate summaries of channel activities
- Extract and compile action items and follow-ups
- Configurable channel selection
- Easy to use command-line interface
- Optionally sends summaries via email
- Stores message history in PostgreSQL database

## Setup

1. Create a Slack App and obtain the necessary credentials
   - Go to https://api.slack.com/apps
   - Create a new app
   - Add the following OAuth scopes:
     - channels:history
     - channels:read
     - groups:history
     - groups:read

2. Copy the `.env.example` to `.env` and fill in your Slack credentials:
   ```
   SLACK_BOT_TOKEN=xoxb-your-token
   SLACK_APP_TOKEN=xapp-your-token
   ```

3. Build the application:
   ```bash
   go build -o shinbun
   ```

## Configuration

You configure Shinbun primarily through the `.env` file.

Copy `.env.example` to `.env` and fill in the required values:

```env
# Required Configuration
SLACK_BOT_TOKEN=xoxb-your-token
OPENAI_API_KEY=sk-your-openai-key

# Channel Focus Categories
# Define comma-separated lists of channel names for different focus areas.
# The application will use DEFAULT_FOCUS_CHANNELS unless a --focus is provided.
DEFAULT_FOCUS_CHANNELS=general,random,announcements
# Example 'support' focus category (used with --focus support)
SUPPORT_FOCUS_CHANNELS=support-tier1,helpdesk

# Database Configuration
DB_HOST=localhost
DB_PORT=5432
DB_NAME=shinbun
DB_USER=postgres
DB_PASSWORD=password

# Email Configuration (Optional)
SMTP_HOST=smtp.gmail.com
SMTP_PORT=587
SMTP_USER=your-email@gmail.com
SMTP_PASSWORD=your-app-specific-password
EMAIL_FROM=your-email@gmail.com
EMAIL_TO=recipient1@example.com,recipient2@example.com
```

## Usage

Run the application from your terminal:

```bash
# Run with default focus, fetching messages since last run
go run main.go 

# Run with 'support' focus
go run main.go --focus support

# Run with default focus, fetching messages from the last 7 days
go run main.go --from-date 7d

# Run with default focus, fetching messages since a specific date
go run main.go --from-date 2025-04-01

# List available channels and exit
go run main.go --list-channels

# Run in dry-run mode (prints summary/email to console instead of sending)
go run main.go --dry-run
```

**Command-line Flags:**

*   `--focus <category>`: Specify the channel focus category to use (e.g., `default`, `support`). Corresponds to `*_FOCUS_CHANNELS` variables in `.env`. Defaults to `default`.
*   `--from-date <date|duration>`: Fetch messages starting from a specific date (`YYYY-MM-DD`) or a relative duration (e.g., `24h`, `7d`). If omitted, fetches messages since the last successful run for each channel.
*   `--list-channels`: List accessible Slack channels (public and private the bot is in) and exit.
*   `--dry-run`: Execute the process but print the summary and email content to the console instead of sending an email.

## Email Setup

To enable email functionality:

1. Configure your SMTP settings in the `.env` file
2. For Gmail:
   - Use `smtp.gmail.com` as SMTP_HOST
   - Use port 587
   - Create an [App Password](https://support.google.com/accounts/answer/185833?hl=en) for SMTP_PASSWORD
3. Multiple recipients can be specified by separating email addresses with commas in EMAIL_TO

## License

MIT License
