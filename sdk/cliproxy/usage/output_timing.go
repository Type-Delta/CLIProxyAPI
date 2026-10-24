package usage

// OutputTiming describes matching output increments and their local receipt window.
// The first source-read batch is excluded from WindowTokens. Counts from a local
// tokenizer remain estimated even when the source timestamps were observed.
type OutputTiming struct {
	Version        int    `json:"version"`
	Protocol       string `json:"protocol"`
	Source         string `json:"source"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	FirstArrivalNS int64  `json:"first_arrival_ns"`
	LastArrivalNS  int64  `json:"last_arrival_ns"`
	ArrivalCount   int64  `json:"arrival_count"`
	// Byte counts cover extracted substantive content, including the first batch.
	FirstBatchBytes   int64 `json:"first_batch_bytes"`
	TotalBytes        int64 `json:"total_bytes"`
	LargestBatchBytes int64 `json:"largest_batch_bytes"`
	// MaxInterarrivalNS is the longest gap between substantive read batches.
	MaxInterarrivalNS int64        `json:"max_interarrival_ns"`
	WindowTokens      int64        `json:"window_tokens"`
	TokenQuality      TokenQuality `json:"token_quality"`
	Coverage          string       `json:"coverage"`
}
