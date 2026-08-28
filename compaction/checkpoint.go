package compaction

import "dshgo/llm"

// The checkpoint marker every compaction checkpoint carries: a plugin-source
// message from the `compact` plugin.
const checkpointPlugin = "compact"

// CompactionCheckpointSource is the message provenance carried by a concrete
// compaction checkpoint: the frozen plugin marker plus the correlated
// compaction identity. Build it with CompactCheckpointSource.
type CompactionCheckpointSource struct {
	// Kind is always "plugin" (the marker).
	Kind string `json:"kind"`
	// Plugin is always "compact" (the marker).
	Plugin string `json:"plugin"`
	// CompactionID is the owning compaction identity.
	CompactionID CompactionID `json:"compactionId"`
	// SourceCommandID is the initiating manual command, when present.
	SourceCommandID CommandID `json:"sourceCommandId,omitempty"`
}

// CompactCheckpointSource creates checkpoint provenance correlated with one
// compaction transaction.
func CompactCheckpointSource(compactionID CompactionID, sourceCommandID CommandID) CompactionCheckpointSource {
	return CompactionCheckpointSource{
		Kind:            "plugin",
		Plugin:          checkpointPlugin,
		CompactionID:    compactionID,
		SourceCommandID: sourceCommandID,
	}
}

// IsCompactCheckpointSource tests whether a persisted message source
// identifies a compaction checkpoint.
func IsCompactCheckpointSource(source llm.MessageSource) bool {
	return source.Kind == "plugin" && source.Plugin == checkpointPlugin
}
