CREATE TABLE IF NOT EXISTS channels (
    id SERIAL PRIMARY KEY,
    slack_id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    last_fetched TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,
    slack_id TEXT NOT NULL,
    channel_id INTEGER REFERENCES channels(id),
    text TEXT NOT NULL,
    timestamp TIMESTAMP WITH TIME ZONE NOT NULL,
    permalink TEXT,
    category TEXT,
    priority INTEGER,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(channel_id, timestamp),
    UNIQUE(slack_id)
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_timestamp ON messages(channel_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_slack_id ON messages(slack_id);
