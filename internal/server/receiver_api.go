package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
)

const (
	defaultMoEmailURL = "https://moemail.app"
	maxReceiverPages  = 1000

	// A shared inbox must not be scanned once per HME alias. A short snapshot
	// gives OTP polling fresh mail while protecting the Cloudflare Worker.
	moEmailSnapshotTTL      = 20 * time.Second
	moEmailSnapshotMaxStale = 2 * time.Minute
	moEmailFailureBackoff   = 20 * time.Second
	moEmailMaxAttempts      = 3
)

type receiverClient interface {
	Validate(context.Context) error
	List(context.Context, string, int, int, int) ([]mail.Message, int, error)
	Get(context.Context, string, string) (*mail.FullMessage, error)
}

type apiMailReceiver struct {
	config *account.MailReceiverConfig
	client *http.Client
}

type moEmailSnapshot struct {
	items      []map[string]any
	expiresAt  time.Time
	staleUntil time.Time
	retryAfter time.Time
	loading    bool
	done       chan struct{}
}

var moEmailSnapshots = struct {
	mu      sync.Mutex
	entries map[string]*moEmailSnapshot
}{entries: make(map[string]*moEmailSnapshot)}

func newReceiverClient(config *account.MailReceiverConfig) (receiverClient, error) {
	if err := validateMailReceiver(config); err != nil {
		return nil, err
	}
	return &apiMailReceiver{
		config: config,
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func validateMailReceiver(config *account.MailReceiverConfig) error {
	if config == nil {
		return fmt.Errorf("账号未配置转发收件箱提供商")
	}
	if strings.TrimSpace(config.Address) == "" || !strings.Contains(config.Address, "@") {
		return fmt.Errorf("转发收件箱地址必填")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return fmt.Errorf("转发收件箱 API Key 必填")
	}
	if raw := strings.TrimSpace(config.BaseURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return fmt.Errorf("转发收件箱 API 地址必须是 HTTPS URL")
		}
	}
	return nil
}

func normalizeMailReceiver(config *account.MailReceiverConfig) *account.MailReceiverConfig {
	if config == nil {
		return nil
	}
	copy := *config
	copy.Address = strings.ToLower(strings.TrimSpace(copy.Address))
	copy.BaseURL = strings.TrimRight(strings.TrimSpace(copy.BaseURL), "/")
	copy.MailboxID = strings.TrimSpace(copy.MailboxID)
	copy.APIKey = strings.TrimSpace(copy.APIKey)
	if copy.BaseURL == "" {
		copy.BaseURL = defaultMoEmailURL
	}
	return &copy
}

func (c *apiMailReceiver) List(ctx context.Context, alias string, limit, offset, days int) ([]mail.Message, int, error) {
	return c.listMoEmail(ctx, alias, limit, offset, days)
}

// Validate proves that the configured credentials can access the specific
// fixed forwarding mailbox before those credentials are persisted.
func (c *apiMailReceiver) Validate(ctx context.Context) error {
	_, err := c.moEmailMailboxID(ctx)
	return err
}

func (c *apiMailReceiver) Get(ctx context.Context, alias, messageID string) (*mail.FullMessage, error) {
	raw, err := c.getMoEmail(ctx, messageID)
	if err != nil {
		return nil, err
	}
	message := receiverMessageFromMap(raw, true)
	if message.ID == "" {
		return nil, fmt.Errorf("收件箱提供商返回的邮件缺少 id")
	}
	if !receiverMessageMatchesAlias(raw, alias) {
		return nil, fmt.Errorf("邮件不属于指定 iCloud 别名")
	}
	return &message, nil
}

func (c *apiMailReceiver) listMoEmail(ctx context.Context, alias string, limit, offset, days int) ([]mail.Message, int, error) {
	mailboxID, err := c.moEmailMailboxID(ctx)
	if err != nil {
		return nil, 0, err
	}
	items, err := c.moEmailLatestMessages(ctx, mailboxID, alias)
	if err != nil {
		return nil, 0, err
	}
	all := make([]mail.Message, 0, limit)
	seen := make(map[string]struct{})
	receivedMessages := false
	hasOriginalRecipient := false
	for _, item := range items {
		receivedMessages = true
		message := receiverMessageFromMap(item, false)
		if message.ID == "" {
			continue
		}
		candidate := item
		if !receiverMessageMatchesAlias(candidate, alias) {
			// Updated MoeMail list responses expose the original recipient in
			// to_address. A record with any explicit recipient already has a
			// definitive alias (or is historical forwarding-only mail), so it
			// cannot match this request. Only legacy records missing a recipient
			// need a detail lookup for a possible original-recipient header.
			if receiverHasExplicitRecipient(candidate) {
				if receiverHasOriginalRecipient(candidate, c.config.Address) {
					hasOriginalRecipient = true
				}
				continue
			}
			detail, detailErr := c.getMoEmailByMailbox(ctx, mailboxID, message.ID)
			if detailErr != nil {
				return nil, 0, fmt.Errorf("读取 MoeMail 邮件 %s 详情失败: %w", message.ID, detailErr)
			}
			candidate = detail
			message = receiverMessageFromMap(candidate, false)
		}
		if receiverHasOriginalRecipient(candidate, c.config.Address) {
			hasOriginalRecipient = true
		}
		if !receiverMessageMatchesAlias(candidate, alias) || !receiverMessageWithinDays(message, days) {
			continue
		}
		if _, exists := seen[message.ID]; exists {
			continue
		}
		seen[message.ID] = struct{}{}
		all = append(all, message.Message)
	}
	if receivedMessages && !hasOriginalRecipient {
		return nil, 0, fmt.Errorf("MoeMail 实例未保留原始收件人，无法准确匹配 iCloud 别名；实例必须在 to_address 或邮件头中返回原 HME 别名")
	}
	sortReceiverMessages(all)
	return receiverPage(all, limit, offset), len(all), nil
}

func (c *apiMailReceiver) moEmailLatestMessages(ctx context.Context, mailboxID, alias string) ([]map[string]any, error) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	key := c.config.BaseURL + "\x00" + mailboxID + "\x00" + alias
	for {
		now := time.Now()
		moEmailSnapshots.mu.Lock()
		entry := moEmailSnapshots.entries[key]
		if entry != nil && !entry.loading && now.Before(entry.expiresAt) {
			items := append([]map[string]any(nil), entry.items...)
			moEmailSnapshots.mu.Unlock()
			return items, nil
		}
		if entry != nil && !entry.loading && len(entry.items) > 0 && now.Before(entry.retryAfter) {
			items := append([]map[string]any(nil), entry.items...)
			moEmailSnapshots.mu.Unlock()
			return items, nil
		}
		if entry != nil && entry.loading {
			done := entry.done
			moEmailSnapshots.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		if entry == nil {
			entry = &moEmailSnapshot{}
			moEmailSnapshots.entries[key] = entry
		}
		// Retain a verified snapshot while an expired refresh is in flight. If
		// Cloudflare aborts the new response we can serve that short-lived
		// snapshot instead of cascading retries across every OTP poller.
		entry.loading = true
		entry.done = make(chan struct{})
		moEmailSnapshots.mu.Unlock()

		query := url.Values{
			"summary":    {"1"},
			"limit":      {strconv.Itoa(100)},
			"to_address": {alias},
		}
		body, err := c.getJSON(ctx, c.config.BaseURL+"/api/emails/"+url.PathEscape(mailboxID)+"?"+query.Encode(), c.moEmailHeaders())
		items := extractMessageList(body)

		moEmailSnapshots.mu.Lock()
		entry.loading = false
		if err == nil {
			entry.items = items
			entry.expiresAt = time.Now().Add(moEmailSnapshotTTL)
			entry.staleUntil = time.Now().Add(moEmailSnapshotMaxStale)
			entry.retryAfter = time.Time{}
		} else if len(entry.items) > 0 && time.Now().Before(entry.staleUntil) {
			// Cloudflare can terminate an overloaded Worker mid-response. Keep
			// the last verified snapshot briefly rather than making every alias
			// poll repeat the same expensive list request and cascade failures.
			entry.retryAfter = time.Now().Add(moEmailFailureBackoff)
			items = append([]map[string]any(nil), entry.items...)
			err = nil
		} else {
			delete(moEmailSnapshots.entries, key)
		}
		close(entry.done)
		moEmailSnapshots.mu.Unlock()
		return items, err
	}
}

func clearMoEmailSnapshot(baseURL, mailboxID string) {
	prefix := strings.TrimRight(baseURL, "/") + "\x00" + strings.TrimSpace(mailboxID)
	moEmailSnapshots.mu.Lock()
	for key := range moEmailSnapshots.entries {
		if key == prefix || strings.HasPrefix(key, prefix+"\x00") {
			delete(moEmailSnapshots.entries, key)
		}
	}
	moEmailSnapshots.mu.Unlock()
}

func (c *apiMailReceiver) getMoEmail(ctx context.Context, messageID string) (map[string]any, error) {
	mailboxID, err := c.moEmailMailboxID(ctx)
	if err != nil {
		return nil, err
	}
	return c.getMoEmailByMailbox(ctx, mailboxID, messageID)
}

func (c *apiMailReceiver) getMoEmailByMailbox(ctx context.Context, mailboxID, messageID string) (map[string]any, error) {
	body, err := c.getJSON(ctx, c.config.BaseURL+"/api/emails/"+url.PathEscape(mailboxID)+"/"+url.PathEscape(messageID), c.moEmailHeaders())
	if err != nil {
		return nil, err
	}
	return extractMessage(body), nil
}

func (c *apiMailReceiver) moEmailMailboxID(ctx context.Context) (string, error) {
	if c.config.MailboxID != "" {
		return c.config.MailboxID, nil
	}
	cursor := ""
	for page := 0; page < maxReceiverPages; page++ {
		endpoint := c.config.BaseURL + "/api/emails"
		if cursor != "" {
			endpoint += "?" + url.Values{"cursor": {cursor}}.Encode()
		}
		body, err := c.getJSON(ctx, endpoint, c.moEmailHeaders())
		if err != nil {
			return "", err
		}
		for _, raw := range mapList(body["emails"]) {
			if strings.EqualFold(stringValue(raw["address"]), c.config.Address) {
				if id := stringValue(raw["id"]); id != "" {
					return id, nil
				}
			}
		}
		next := stringValue(body["nextCursor"])
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return "", fmt.Errorf("MoeMail 中找不到转发收件箱: %s", c.config.Address)
}

func (c *apiMailReceiver) moEmailHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("X-API-Key", c.config.APIKey)
	return headers
}

func (c *apiMailReceiver) getJSON(ctx context.Context, endpoint string, headers http.Header) (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt < moEmailMaxAttempts; attempt++ {
		parsed, retryable, err := c.getJSONOnce(ctx, endpoint, headers)
		if err == nil {
			return parsed, nil
		}
		lastErr = err
		if !retryable || attempt+1 >= moEmailMaxAttempts {
			break
		}
		delay := 300 * time.Millisecond
		if attempt > 0 {
			delay = 900 * time.Millisecond
		}
		if err := waitForMoEmailRetry(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *apiMailReceiver) getJSONOnce(ctx context.Context, endpoint string, headers http.Header) (map[string]any, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if readErr != nil {
		return nil, true, readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode >= http.StatusInternalServerError, fmt.Errorf("MoeMail 邮件 API 返回 HTTP %d: %s", resp.StatusCode, truncateReceiverError(string(body), 400))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, true, fmt.Errorf("解析 MoeMail 邮件 API 响应失败: %w", err)
	}
	if success, found := parsed["success"].(bool); found && !success {
		return nil, false, fmt.Errorf("MoeMail 邮件 API 返回失败: %s", truncateReceiverError(string(body), 400))
	}
	return parsed, false, nil
}

func waitForMoEmailRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func extractMessageList(body map[string]any) []map[string]any {
	value := any(body)
	if data, ok := body["data"]; ok {
		value = data
	}
	if list := mapList(value); len(list) > 0 {
		return list
	}
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"messages", "results", "items", "list"} {
			if list := mapList(object[key]); len(list) > 0 {
				return list
			}
		}
	}
	for _, key := range []string{"messages", "results", "items", "list"} {
		if list := mapList(body[key]); len(list) > 0 {
			return list
		}
	}
	return nil
}

func extractMessage(body map[string]any) map[string]any {
	value := any(body)
	if data, ok := body["data"]; ok {
		value = data
	}
	if object, ok := value.(map[string]any); ok {
		if message, ok := object["message"].(map[string]any); ok {
			return message
		}
		return object
	}
	if message, ok := body["message"].(map[string]any); ok {
		return message
	}
	return nil
}

func mapList(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func receiverMessageFromMap(raw map[string]any, full bool) mail.FullMessage {
	message := mail.FullMessage{Message: mail.Message{
		ID:      firstMapString(raw, "id", "messageId", "message_id"),
		From:    addressValue(firstMapValue(raw, "from", "from_address", "sender", "source")),
		To:      addressValue(firstMapValue(raw, "to", "to_address", "address", "recipient")),
		Subject: firstMapString(raw, "subject", "title"),
		Date:    receiverDate(firstMapValue(raw, "received_at", "receivedAt", "sent_at", "sentAt", "createdAt", "created_at", "date")),
		Folder:  "inbox",
	}}
	if full {
		message.Body = firstMapString(raw, "text", "content", "body")
		message.HTML = firstMapString(raw, "html", "html_content", "bodyHtml")
		message.ContentType = firstMapString(raw, "content_type", "contentType")
		message.IsHTML = message.HTML != ""
		if message.Body == "" && message.HTML != "" {
			message.Body = receiverStripHTML(message.HTML)
		}
	} else {
		message.Preview = firstMapString(raw, "preview", "snippet", "text", "content")
	}
	if message.Preview == "" {
		message.Preview = receiverPreview(message.Body)
	}
	return message
}

func receiverMessageMatchesAlias(raw map[string]any, alias string) bool {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		return false
	}
	for _, candidate := range recipientCandidates(raw) {
		for _, found := range receiverEmailPattern.FindAllString(strings.ToLower(candidate), -1) {
			if found == alias {
				return true
			}
		}
	}
	return false
}

func receiverHasOriginalRecipient(raw map[string]any, forwardingAddress string) bool {
	forwardingAddress = strings.ToLower(strings.TrimSpace(forwardingAddress))
	for _, candidate := range recipientCandidates(raw) {
		for _, found := range receiverEmailPattern.FindAllString(strings.ToLower(candidate), -1) {
			if found != forwardingAddress {
				return true
			}
		}
	}
	return false
}

func receiverHasExplicitRecipient(raw map[string]any) bool {
	for _, candidate := range recipientCandidates(raw) {
		if len(receiverEmailPattern.FindAllString(strings.ToLower(candidate), -1)) > 0 {
			return true
		}
	}
	return false
}

var receiverEmailPattern = regexp.MustCompile(`[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`)
var receiverHTMLTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)

func recipientCandidates(value any) []string {
	var out []string
	var walk func(any, string)
	walk = func(node any, key string) {
		switch item := node.(type) {
		case map[string]any:
			for name, child := range item {
				lower := strings.ToLower(name)
				if isRecipientField(lower) {
					walk(child, lower)
					continue
				}
				if lower == "headers" || lower == "header" || lower == "envelope" || lower == "metadata" {
					walk(child, lower)
				}
			}
		case []any:
			for _, child := range item {
				walk(child, key)
			}
		case string:
			if isRecipientField(key) {
				out = append(out, item)
			}
		}
	}
	walk(value, "")
	return out
}

func isRecipientField(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	return key == "to" || key == "address" || key == "recipient" || key == "recipients" ||
		key == "toaddress" || key == "deliveredto" || key == "originalto" || key == "envelopeto" ||
		key == "resentto" || key == "xoriginalto" || key == "xenvelopeto"
}

func receiverStripHTML(value string) string {
	value = receiverHTMLTagPattern.ReplaceAllString(value, " ")
	value = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"").Replace(value)
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))
}

func receiverMessageWithinDays(message mail.FullMessage, days int) bool {
	if days <= 0 || message.Date == "" {
		return true
	}
	parsed, err := time.Parse(time.RFC3339, message.Date)
	return err != nil || !parsed.Before(time.Now().Add(-time.Duration(days)*24*time.Hour))
}

func sortReceiverMessages(messages []mail.Message) {
	sort.SliceStable(messages, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339, messages[i].Date)
		right, rightErr := time.Parse(time.RFC3339, messages[j].Date)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.After(right)
		}
		return messages[i].ID > messages[j].ID
	})
}

func receiverPage(messages []mail.Message, limit, offset int) []mail.Message {
	if limit <= 0 || offset >= len(messages) {
		return []mail.Message{}
	}
	end := offset + limit
	if end > len(messages) {
		end = len(messages)
	}
	return messages[offset:end]
}

func firstMapValue(raw map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := raw[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func firstMapString(raw map[string]any, keys ...string) string {
	return stringValue(firstMapValue(raw, keys...))
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return fmt.Sprintf("%.0f", typed)
	case json.Number:
		return typed.String()
	case map[string]any:
		return firstMapString(typed, "address", "email", "value", "name")
	default:
		return ""
	}
}

func addressValue(value any) string {
	if object, ok := value.(map[string]any); ok {
		address := firstMapString(object, "address", "email")
		name := firstMapString(object, "name", "displayName")
		if name != "" && address != "" {
			return name + " <" + address + ">"
		}
		return address
	}
	return stringValue(value)
}

func receiverDate(value any) string {
	switch typed := value.(type) {
	case float64:
		if typed > 1_000_000_000_000 {
			return time.UnixMilli(int64(typed)).Format(time.RFC3339)
		}
		return time.Unix(int64(typed), 0).Format(time.RFC3339)
	case string:
		if parsed, err := time.Parse(time.RFC3339, typed); err == nil {
			return parsed.Format(time.RFC3339)
		}
		return typed
	default:
		return ""
	}
}

func receiverPreview(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		return value[:240]
	}
	return value
}

func truncateReceiverError(value string, size int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > size {
		return value[:size] + "..."
	}
	return value
}
