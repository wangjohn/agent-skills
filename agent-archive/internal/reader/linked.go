package reader

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// LinkedAvailability is a read-time observation, separate from the historical
// link in a source bundle. Metadata availability does not verify child source
// bytes; show CHILD --normalized performs that check on explicit selection.
type LinkedAvailability struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

// ResolveLinkedSessions reads only direct child metadata. It does not follow
// grandchildren, download transcripts, or pin children against retention.
// One missing or malformed child must not make its parent's metadata unreadable.
func ResolveLinkedSessions(ctx context.Context, store storage.ObjectStore, parent archive.Metadata) []LinkedAvailability {
	var result []LinkedAvailability
	for _, link := range parent.LinkedSessions {
		item := LinkedAvailability{SessionID: link.SessionID, State: "unavailable"}
		if link.Relationship != "subagent" || link.Status == archive.LinkedSessionUnavailable {
			result = append(result, item)
			continue
		}
		key, err := archive.MetadataObjectKey(parent.Harness.Name, link.SessionID)
		if err != nil || link.SessionID == parent.SessionID {
			item.State = "identity_mismatch"
			result = append(result, item)
			continue
		}
		child, err := ReadMetadata(ctx, store, key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			if link.Status == archive.LinkedSessionPending {
				item.State = "pending"
			} else {
				item.State = "unavailable_or_expired"
			}
		case err != nil:
			item.State = "lookup_failed"
		case child.SessionID != link.SessionID || child.ParentSessionID != parent.SessionID || child.ProjectID != parent.ProjectID || child.MachineID != parent.MachineID || child.Harness.Name != parent.Harness.Name:
			item.State = "identity_mismatch"
		default:
			item.State = "metadata_available"
		}
		result = append(result, item)
	}
	return result
}
