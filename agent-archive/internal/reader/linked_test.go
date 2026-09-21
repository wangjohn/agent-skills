package reader

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
)

func TestLinkedSessionsResolveWithoutDownloadingOrPinningSources(t *testing.T) {
	parent, _, store := fixture(t)
	parent.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "child", Relationship: "subagent", Status: archive.LinkedSessionPending}}
	ctx := context.Background()
	check := func(want string) {
		t.Helper()
		got := ResolveLinkedSessions(ctx, store, parent)
		if len(got) != 1 || got[0].State != want {
			t.Fatalf("want %s got %+v", want, got)
		}
	}
	check("pending")
	child := parent
	child.SessionID = "child"
	child.ParentSessionID = parent.SessionID
	child.LinkedSessions = nil
	child.SourceBundle.Key = "sessions/codex/child/source." + strings.Repeat("a", 64) + ".json.gz"
	child.SourceBundle.SHA256 = strings.Repeat("a", 64)
	key, _ := archive.MetadataObjectKey("codex", "child")
	save := func() {
		t.Helper()
		data, err := json.Marshal(child)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	save()
	// No child source was uploaded: resolution must be metadata-only.
	check("metadata_available")
	parent.LinkedSessions[0].Status = archive.LinkedSessionPublished
	child.ParentSessionID = "other-parent"
	save()
	check("identity_mismatch")
	child.ParentSessionID = parent.SessionID
	child.MachineID = "other-machine"
	save()
	check("identity_mismatch")
	if err := store.Put(ctx, key, []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	check("lookup_failed")
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	check("unavailable_or_expired")
	parent.LinkedSessions[0].Status = archive.LinkedSessionUnavailable
	check("unavailable")
}

func TestSourceReadRejectsMismatchedParent(t *testing.T) {
	metadata, _, store := fixture(t)
	metadata.ParentSessionID = "unrelated-parent"
	if _, err := LoadSource(context.Background(), store, metadata, Limits{}); err == nil {
		t.Fatal("source ownership mismatch accepted")
	}
}
