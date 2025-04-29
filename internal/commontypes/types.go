package commontypes

// Update represents a single message update
type Update struct {
	Text      string
	Timestamp string
	Link      string
	Channel   string // Added channel name for context
	Category  string
	Priority  int
}
