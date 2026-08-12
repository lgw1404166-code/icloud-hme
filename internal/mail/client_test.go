package mail

import (
	"net/mail"
	"strings"
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

func TestPaginateMessages(t *testing.T) {
	messages := []Message{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}}
	page := paginateMessages(messages, 2, 2)
	if len(page) != 2 || page[0].ID != "3" || page[1].ID != "4" {
		t.Fatalf("unexpected page: %#v", page)
	}
	if page := paginateMessages(messages, 8, 2); len(page) != 0 {
		t.Fatalf("out-of-range page = %#v, want empty", page)
	}
}

func TestMergeMessagePagesAcrossFolders(t *testing.T) {
	inbox := []Message{
		{ID: "inbox:3", Date: "2026-08-03T10:00:00Z"},
		{ID: "inbox:1", Date: "2026-08-01T10:00:00Z"},
	}
	junk := []Message{
		{ID: "junk:4", Date: "2026-08-04T10:00:00Z"},
		{ID: "junk:2", Date: "2026-08-02T10:00:00Z"},
	}
	page := MergeMessagePages(1, 2, inbox, junk)
	if len(page) != 2 || page[0].ID != "inbox:3" || page[1].ID != "junk:2" {
		t.Fatalf("unexpected merged page: %#v", page)
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

func TestReadBodyPartsKeepsHTMLBody(t *testing.T) {
	raw := "Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<html><body><p>Hello&nbsp;<b>World</b></p></body></html>"
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, htmlBody, _, err := readBodyParts(msg)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "" || !strings.Contains(htmlBody, "<b>World</b>") {
		t.Fatalf("plain=%q html=%q", plain, htmlBody)
	}
	if got := stripHTML(htmlBody); !strings.Contains(got, "Hello World") {
		t.Fatalf("stripped html = %q", got)
	}
}

func TestReadBodyPartsRewritesInlineCIDImage(t *testing.T) {
	raw := strings.Join([]string{
		"Content-Type: multipart/related; boundary=mail-boundary",
		"",
		"--mail-boundary",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<html><body><img src="cid:hero@example"></body></html>`,
		"--mail-boundary",
		"Content-Type: image/png",
		"Content-Transfer-Encoding: base64",
		"Content-ID: <hero@example>",
		"Content-Disposition: inline",
		"",
		"aGVsbG8=",
		"--mail-boundary--",
		"",
	}, "\r\n")
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, htmlBody, _, inlineImages, err := readBodyPartsWithInline(msg)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := rewriteCIDImages(htmlBody, inlineImages)
	if !strings.Contains(rewritten, "data:image/png;base64,aGVsbG8=") || strings.Contains(rewritten, "cid:hero@example") {
		t.Fatalf("inline image was not rewritten: %q", rewritten)
	}
}

func TestStripHTMLIgnoresStylesAndScripts(t *testing.T) {
	raw := `<html><head><style>.code { color: red; }</style><script>alert(1)</script></head><body><h1>Hello</h1><p>Your code is <strong>123456</strong>.</p></body></html>`
	got := stripHTML(raw)
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "Your code is 123456.") {
		t.Fatalf("stripHTML() = %q", got)
	}
	for _, unwanted := range []string{"color: red", "alert(1)"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("stripHTML() retained %q: %q", unwanted, got)
		}
	}
}
