package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	cleanupTickInterval = time.Minute
	cleanupPageLimit    = 25
)

type moEmailCleanupResult struct {
	Deleted    int
	Scanned    int
	NextCursor string
}

// Cleanup scans one oldest-progressing page at a time. It deliberately avoids
// a full mailbox sweep because MoeMail list responses may contain large HTML.
func cleanupMoEmailReceiver(ctx context.Context, receiver receiverClient, retention time.Duration, cursor string, maxDelete int) (moEmailCleanupResult, error) {
	client, ok := receiver.(*apiMailReceiver)
	if !ok {
		return moEmailCleanupResult{}, fmt.Errorf("不支持的 MoeMail 清理客户端")
	}
	return client.cleanupOldMessagesPage(ctx, retention, cursor, maxDelete)
}

func (c *apiMailReceiver) cleanupOldMessagesPage(ctx context.Context, retention time.Duration, cursor string, maxDelete int) (moEmailCleanupResult, error) {
	mailboxID, err := c.moEmailMailboxID(ctx)
	if err != nil {
		return moEmailCleanupResult{}, err
	}
	query := url.Values{"summary": {"1"}}
	if strings.TrimSpace(cursor) != "" {
		query.Set("cursor", cursor)
	}
	body, err := c.getJSON(ctx, c.config.BaseURL+"/api/emails/"+url.PathEscape(mailboxID)+"?"+query.Encode(), c.moEmailHeaders())
	if err != nil {
		return moEmailCleanupResult{}, err
	}
	items := extractMessageList(body)
	result := moEmailCleanupResult{Scanned: len(items), NextCursor: stringValue(body["nextCursor"])}
	cutoff := time.Now().Add(-retention)
	for _, item := range items {
		if result.Deleted >= maxDelete {
			break
		}
		message := receiverMessageFromMap(item, false)
		when, ok := receiverMessageTime(message.Date)
		if message.ID == "" || !ok || !when.Before(cutoff) {
			continue
		}
		if err := c.deleteMoEmailMessage(ctx, mailboxID, message.ID); err != nil {
			return result, fmt.Errorf("删除邮件 %s: %w", message.ID, err)
		}
		result.Deleted++
	}
	if result.NextCursor == "" || len(items) == 0 {
		result.NextCursor = ""
	}
	clearMoEmailSnapshot(c.config.BaseURL, mailboxID)
	return result, nil
}

func (c *apiMailReceiver) deleteMoEmailMessage(ctx context.Context, mailboxID, messageID string) error {
	endpoint := c.config.BaseURL + "/api/emails/" + url.PathEscape(mailboxID) + "/" + url.PathEscape(messageID)
	var lastErr error
	for attempt := 0; attempt < moEmailMaxAttempts; attempt++ {
		retryable, err := c.deleteJSONOnce(ctx, endpoint, c.moEmailHeaders())
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt+1 >= moEmailMaxAttempts {
			break
		}
		if err := waitForMoEmailRetry(ctx, time.Duration(attempt+1)*300*time.Millisecond); err != nil {
			return err
		}
	}
	return lastErr
}

func (c *apiMailReceiver) deleteJSONOnce(ctx context.Context, endpoint string, headers http.Header) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return false, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return false, nil
	}
	return resp.StatusCode >= http.StatusInternalServerError, fmt.Errorf("MoeMail 删除接口返回 HTTP %d", resp.StatusCode)
}

func receiverMessageTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	return parsed, err == nil
}

func (s *Server) cleanupCursor(accountID string) string {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	return s.cleanupCursors[accountID]
}

func (s *Server) setCleanupCursor(accountID, cursor string) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if cursor == "" {
		delete(s.cleanupCursors, accountID)
		return
	}
	s.cleanupCursors[accountID] = cursor
}

func (s *Server) startMailReceiverCleanupWorker() {
	go func() {
		for {
			s.runMailReceiverCleanupTick()
			time.Sleep(cleanupTickInterval)
		}
	}()
}

func (s *Server) runMailReceiverCleanupTick() {
	for _, publicAccount := range s.mgr.ListAccounts() {
		if publicAccount == nil || !publicAccount.Enabled || publicAccount.MailReceiver == nil {
			continue
		}
		if !publicAccount.MailReceiver.CleanupEnabled || publicAccount.MailReceiver.CleanupRetentionSeconds <= 0 {
			continue
		}
		receiverConfig, err := s.mgr.MailReceiver(publicAccount.ID)
		if err != nil {
			continue
		}
		receiver, err := s.receiverClientFactory(normalizeMailReceiver(receiverConfig))
		if err != nil {
			continue
		}
		retention := time.Duration(receiverConfig.CleanupRetentionSeconds) * time.Second
		result, err := cleanupMoEmailReceiver(context.Background(), receiver, retention, s.cleanupCursor(publicAccount.ID), cleanupPageLimit)
		if err != nil {
			continue
		}
		s.setCleanupCursor(publicAccount.ID, result.NextCursor)
		if result.Deleted > 0 {
			s.clearInboxCache(publicAccount.ID)
		}
	}
}
