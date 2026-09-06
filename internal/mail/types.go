// Package mail contains the API mail value objects shared by receiver clients.
package mail

import (
	"fmt"
	"strings"
)

const (
	FolderAll   = "all"
	FolderInbox = "inbox"
	FolderJunk  = "junk"
)

// Message is a summary returned by a receiver provider.
type Message struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Preview string `json:"preview"`
	Folder  string `json:"folder,omitempty"`
}

// FullMessage is a message with its fetched body.
type FullMessage struct {
	Message
	Body        string `json:"body"`
	HTML        string `json:"html,omitempty"`
	ContentType string `json:"content_type"`
	IsHTML      bool   `json:"is_html"`
}

// NormalizeFolder preserves the public API vocabulary. Receiver providers
// expose a single inbox, so the server handles all/inbox equivalently.
func NormalizeFolder(folder string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case "", FolderAll:
		return FolderAll, nil
	case FolderInbox:
		return FolderInbox, nil
	case FolderJunk:
		return FolderJunk, nil
	default:
		return "", fmt.Errorf("不支持的邮件夹: %s (可选 all、inbox、junk)", folder)
	}
}
