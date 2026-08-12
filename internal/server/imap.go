package server

import (
	"fmt"
	"strings"
	"sync"

	"icloud-hme/internal/mail"
)

type imapSession struct {
	mu     sync.Mutex
	client *mail.Client
}

func (s *Server) warmIMAPSessions() {
	for _, account := range s.mgr.ListAccounts() {
		accountID := account.ID
		for _, slot := range []string{mail.FolderInbox, mail.FolderJunk} {
			slot := slot
			go func() {
				_ = s.withMailClientSlot(accountID, slot, func(*mail.Client) error { return nil })
			}()
		}
	}
}

// withMailClient serializes commands per account and reuses the authenticated
// IMAP connection. A dropped connection is rebuilt once before surfacing the error.
func (s *Server) withMailClient(accountID string, operation func(*mail.Client) error) error {
	return s.withMailClientSlot(accountID, "default", operation)
}

func (s *Server) withMailClientSlot(accountID, slot string, operation func(*mail.Client) error) error {
	sessionKey := accountID + "\x00" + slot
	s.imapMu.Lock()
	session := s.imapSessions[sessionKey]
	if session == nil {
		session = &imapSession{}
		s.imapSessions[sessionKey] = session
	}
	s.imapMu.Unlock()

	session.mu.Lock()
	defer session.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if session.client == nil {
			client, err := s.mgr.MailClient(accountID)
			if err != nil {
				return err
			}
			if err := client.Connect(); err != nil {
				return err
			}
			session.client = client
		}
		if err := operation(session.client); err == nil {
			return nil
		} else {
			lastErr = err
		}
		session.client.Disconnect()
		session.client = nil
	}
	return lastErr
}

func (s *Server) closeIMAPSession(accountID string) {
	s.clearInboxCache(accountID)
	s.imapMu.Lock()
	sessions := make([]*imapSession, 0, 3)
	prefix := accountID + "\x00"
	for sessionKey, session := range s.imapSessions {
		if strings.HasPrefix(sessionKey, prefix) {
			sessions = append(sessions, session)
			delete(s.imapSessions, sessionKey)
		}
	}
	s.imapMu.Unlock()
	for _, session := range sessions {
		session.mu.Lock()
		if session.client != nil {
			session.client.Disconnect()
			session.client = nil
		}
		session.mu.Unlock()
	}
}

type folderQueryResult struct {
	messages []mail.Message
	total    int
	err      error
}

func (s *Server) findAliasInAllFolders(accountID, alias string, limit, offset, days int) ([]mail.Message, int, error) {
	fetchLimit := offset + limit
	results := make(chan folderQueryResult, 2)
	for _, folder := range []string{mail.FolderInbox, mail.FolderJunk} {
		folder := folder
		go func() {
			var messages []mail.Message
			var total int
			err := s.withMailClientSlot(accountID, folder, func(client *mail.Client) error {
				var queryErr error
				messages, total, queryErr = client.FindByRecipientPage(alias, folder, fetchLimit, 0, days)
				return queryErr
			})
			results <- folderQueryResult{messages: messages, total: total, err: err}
		}()
	}

	groups := make([][]mail.Message, 0, 2)
	total := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			return nil, 0, fmt.Errorf("并行查询邮件夹失败: %w", result.err)
		}
		groups = append(groups, result.messages)
		total += result.total
	}
	return mail.MergeMessagePages(offset, limit, groups...), total, nil
}

func (s *Server) closeAllIMAPSessions() {
	s.clearInboxCache("")
	s.imapMu.Lock()
	sessions := s.imapSessions
	s.imapSessions = make(map[string]*imapSession)
	s.imapMu.Unlock()
	for _, session := range sessions {
		session.mu.Lock()
		if session.client != nil {
			session.client.Disconnect()
			session.client = nil
		}
		session.mu.Unlock()
	}
}
