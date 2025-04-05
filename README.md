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

You can configure the channels to monitor either through command-line flags or using a configuration file at `config.yaml`.

Copy `.env.example` to `.env` and fill in the required values:

```env
# Required Configuration
SLACK_TOKEN=xoxb-your-token
OPENAI_TOKEN=your-openai-token
SOURCE_CHANNELS=channel1,channel2

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

```bash
./shinbun --channels general,team-updates,announcements
```

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
