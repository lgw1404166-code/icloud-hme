package server

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/mail"
)

const inboxCacheTTL = 30 * time.Second

type inboxCacheEntry struct {
	messages  []mail.Message
	total     int
	expiresAt time.Time
}

type inboxCacheStore struct {
	mu      sync.Mutex
	entries map[string]inboxCacheEntry
}

func inboxCacheKey(accountID, alias, folder string, limit, page, days int) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d", accountID, strings.ToLower(alias), folder, limit, page, days)
}

func (s *Server) getInboxCache(key string) ([]mail.Message, int, bool) {
	s.inboxCache.mu.Lock()
	defer s.inboxCache.mu.Unlock()
	entry, exists := s.inboxCache.entries[key]
	if !exists || time.Now().After(entry.expiresAt) {
		delete(s.inboxCache.entries, key)
		return nil, 0, false
	}
	return append([]mail.Message(nil), entry.messages...), entry.total, true
}

func (s *Server) setInboxCache(key string, messages []mail.Message, total int) {
	s.inboxCache.mu.Lock()
	defer s.inboxCache.mu.Unlock()
	s.inboxCache.entries[key] = inboxCacheEntry{
		messages:  append([]mail.Message(nil), messages...),
		total:     total,
		expiresAt: time.Now().Add(inboxCacheTTL),
	}
}

func (s *Server) clearInboxCache(accountID string) {
	s.inboxCache.mu.Lock()
	defer s.inboxCache.mu.Unlock()
	prefix := accountID + "\x00"
	for key := range s.inboxCache.entries {
		if accountID == "" || strings.HasPrefix(key, prefix) {
			delete(s.inboxCache.entries, key)
		}
	}
}
