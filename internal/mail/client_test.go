package mail

import (
	"testing"
	"time"
)

func TestNormalizeFolderDefaultsToAll(t *testing.T) {
	for _, input := range []string{"", "all", " ALL "} {
		folder, err := NormalizeFolder(input)
		if err != nil {
			t.Fatalf("NormalizeFolder(%q): %v", input, err)
		}
		if folder != FolderAll {
			t.Fatalf("NormalizeFolder(%q) = %q, want %q", input, folder, FolderAll)
		}
	}
	if _, err := NormalizeFolder("archive"); err == nil {
		t.Fatal("NormalizeFolder accepted an unsupported folder")
	}
}

func TestMergeMessagesSortsAndLimitsAcrossFolders(t *testing.T) {
	inbox := []Message{{ID: "inbox:1", Folder: FolderInbox, Date: "2026-08-01T10:00:00Z"}}
	junk := []Message{
		{ID: "junk:2", Folder: FolderJunk, Date: "2026-08-03T10:00:00Z"},
		{ID: "junk:1", Folder: FolderJunk, Date: "2026-08-02T10:00:00Z"},
	}

	merged := mergeMessages(2, inbox, junk)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	if merged[0].ID != "junk:2" || merged[1].ID != "junk:1" {
		t.Fatalf("unexpected order: %#v", merged)
	}
}

func TestSetMessageFolderMakesUIDUnique(t *testing.T) {
	message := Message{ID: "2"}
	setMessageFolder(&message, FolderJunk)
	if message.ID != "junk:2" || message.Folder != FolderJunk {
		t.Fatalf("unexpected message identity: %#v", message)
	}
}

func TestMessageWithinDaysParsesRFC3339(t *testing.T) {
	recent := Message{Date: time.Now().Add(-time.Hour).Format(time.RFC3339)}
	old := Message{Date: time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}
	if !messageWithinDays(recent, 1) {
		t.Fatal("recent RFC3339 message was filtered out")
	}
	if messageWithinDays(old, 1) {
		t.Fatal("old RFC3339 message was not filtered out")
	}
}
