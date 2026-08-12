// Package mail 实现 iCloud 邮件 IMAP 读取客户端。
//
// 通过 Apple 应用专用密码连接 imap.mail.me.com:993,
// 拉取隐私邮箱别名收到的邮件。对应原 Python 项目 icloud_mail.py。
package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/charset"
	"golang.org/x/net/html"
)

const (
	IMAPServer = "imap.mail.me.com"
	IMAPPort   = 993

	FolderAll   = "all"
	FolderInbox = "inbox"
	FolderJunk  = "junk"

	inboxMailbox = "INBOX"
	junkMailbox  = "Junk"
)

// Message 是一封邮件的摘要信息。
type Message struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Preview string `json:"preview"`
	Folder  string `json:"folder,omitempty"`
}

// NormalizeFolder 规范化 API 邮件夹值。空值默认查询全部邮件。
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

// FullMessage 是一封邮件的完整内容(含正文)。
type FullMessage struct {
	Message
	Body        string `json:"body"`
	HTML        string `json:"html,omitempty"`
	ContentType string `json:"content_type"`
	IsHTML      bool   `json:"is_html"`
}

// Client 是 iCloud 邮件 IMAP 客户端。
type Client struct {
	appleID     string
	appPassword string
	cli         *client.Client
}

// NewClient 创建 IMAP 客户端。需在调用其它方法前先 Connect。
func NewClient(appleID, appPassword string) *Client {
	return &Client{appleID: appleID, appPassword: appPassword}
}

// Connect 连接并登录 IMAP 服务器。
func (c *Client) Connect() error {
	addr := fmt.Sprintf("%s:%d", IMAPServer, IMAPPort)
	cli, err := client.DialTLS(addr, nil)
	if err != nil {
		return fmt.Errorf("IMAP 连接失败: %w", err)
	}
	if err := cli.Login(c.appleID, c.appPassword); err != nil {
		return fmt.Errorf("IMAP 登录失败 — 请检查: 1) 应用专用密码是否正确 2) Apple ID: %s — %w", c.appleID, err)
	}
	c.cli = cli
	return nil
}

// Disconnect 登出并关闭连接。
func (c *Client) Disconnect() {
	if c.cli != nil {
		_ = c.cli.Logout()
		c.cli = nil
	}
}

// Noop 检查当前 IMAP 会话是否仍然可用。
func (c *Client) Noop() error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	return c.cli.Noop()
}

// InboxCount 返回收件箱邮件总数。
func (c *Client) InboxCount() (int, error) {
	if c.cli == nil {
		return 0, fmt.Errorf("未连接")
	}
	mbox, err := c.cli.Select("INBOX", false)
	if err != nil {
		return 0, err
	}
	return int(mbox.Messages), nil
}

// ListInbox 拉取 INBOX 最近邮件，保留原有调用语义。
func (c *Client) ListInbox(limit int, days int) ([]Message, error) {
	return c.ListMessages(FolderInbox, limit, days)
}

// ListMessages 拉取指定邮件夹最近邮件。folder 为空时默认查询 INBOX 和 Junk。
func (c *Client) ListMessages(folder string, limit int, days int) ([]Message, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 50
	}
	folder, err := NormalizeFolder(folder)
	if err != nil {
		return nil, err
	}
	if folder == FolderAll {
		inbox, err := c.listMailbox(inboxMailbox, FolderInbox, limit, days)
		if err != nil {
			return nil, fmt.Errorf("读取收件箱失败: %w", err)
		}
		junk, err := c.listMailbox(junkMailbox, FolderJunk, limit, days)
		if err != nil {
			return nil, fmt.Errorf("读取垃圾邮件失败: %w", err)
		}
		return mergeMessages(limit, inbox, junk), nil
	}
	mailbox, _ := mailboxForFolder(folder)
	return c.listMailbox(mailbox, folder, limit, days)
}

func (c *Client) listMailbox(mailbox, folder string, limit, days int) ([]Message, error) {
	mbox, err := c.cli.Select(mailbox, true)
	if err != nil {
		return nil, err
	}
	total := int(mbox.Messages)
	if total == 0 {
		return []Message{}, nil
	}

	// 计算起始序号(只取最近 limit 封)
	from := uint32(1)
	if uint32(limit) < mbox.Messages {
		from = mbox.Messages - uint32(limit) + 1
	}

	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)

	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
	}

	messages := make(chan *imap.Message, limit)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.Fetch(seqset, items, messages)
	}()

	out := make([]Message, 0, limit)
	for msg := range messages {
		m := toMessage(msg)
		setMessageFolder(&m, folder)
		if !messageWithinDays(m, days) {
			continue
		}
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	sortMessages(out)
	return out, nil
}

// FindByRecipient 查找发给指定隐私邮箱别名的邮件。
//
// 保留原有调用语义，仅查询 INBOX。
func (c *Client) FindByRecipient(recipient string, limit int, days int) ([]Message, error) {
	return c.FindByRecipientInFolder(recipient, FolderInbox, limit, days)
}

// FindByRecipientInFolder 在指定邮件夹查找收件人。folder 为空时查询 INBOX 和 Junk。
func (c *Client) FindByRecipientInFolder(recipient, folder string, limit int, days int) ([]Message, error) {
	messages, _, err := c.FindByRecipientPage(recipient, folder, limit, 0, days)
	return messages, err
}

// FindByRecipientPage 按收件人分页查询邮件，并返回符合条件的邮件总数。
func (c *Client) FindByRecipientPage(recipient, folder string, limit, offset, days int) ([]Message, int, error) {
	if c.cli == nil {
		return nil, 0, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	folder, err := NormalizeFolder(folder)
	if err != nil {
		return nil, 0, err
	}
	fetchLimit := offset + limit
	if folder == FolderAll {
		inbox, inboxTotal, err := c.findByRecipientMailbox(recipient, inboxMailbox, FolderInbox, fetchLimit, days)
		if err != nil {
			return nil, 0, fmt.Errorf("搜索收件箱失败: %w", err)
		}
		junk, junkTotal, err := c.findByRecipientMailbox(recipient, junkMailbox, FolderJunk, fetchLimit, days)
		if err != nil {
			return nil, 0, fmt.Errorf("搜索垃圾邮件失败: %w", err)
		}
		return paginateMessages(mergeMessages(0, inbox, junk), offset, limit), inboxTotal + junkTotal, nil
	}
	mailbox, _ := mailboxForFolder(folder)
	messages, total, err := c.findByRecipientMailbox(recipient, mailbox, folder, fetchLimit, days)
	if err != nil {
		return nil, 0, err
	}
	return paginateMessages(messages, offset, limit), total, nil
}

// findByRecipientMailbox 先尝试 IMAP TO 搜索，失败则拉取邮件后本地过滤。
func (c *Client) findByRecipientMailbox(recipient, mailbox, folder string, limit int, days int) ([]Message, int, error) {
	// 先尝试服务端 TO 搜索
	_, err := c.cli.Select(mailbox, true)
	if err != nil {
		return nil, 0, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.Header.Add("To", recipient)
	if days > 0 {
		since := time.Now().AddDate(0, 0, -days)
		criteria.Since = since
	}
	uids, err := c.cli.UidSearch(criteria)
	if err == nil {
		messages, fetchErr := c.fetchByUIDs(uids, limit, folder)
		return messages, len(uids), fetchErr
	}

	// fallback: 拉取当前邮件夹后本地过滤
	all, err := c.listMailbox(mailbox, folder, limit*3, days)
	if err != nil {
		return nil, 0, err
	}
	recipient = strings.ToLower(recipient)
	out := make([]Message, 0, limit)
	for _, m := range all {
		if strings.Contains(strings.ToLower(m.To), recipient) {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, len(out), nil
}

func (c *Client) fetchByUIDs(uids []uint32, limit int, folder string) ([]Message, error) {
	if len(uids) == 0 {
		return []Message{}, nil
	}
	// 取最近 limit 条(UID 倒序)
	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}
	seqset := new(imap.SeqSet)
	for _, uid := range uids {
		seqset.AddNum(uid)
	}

	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	out := make([]Message, 0, len(uids))
	for msg := range messages {
		m := toMessage(msg)
		setMessageFolder(&m, folder)
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	sortMessages(out)
	return out, nil
}

func mailboxForFolder(folder string) (string, error) {
	switch folder {
	case FolderInbox:
		return inboxMailbox, nil
	case FolderJunk:
		return junkMailbox, nil
	default:
		return "", fmt.Errorf("邮件夹 %s 没有单一 IMAP 邮箱", folder)
	}
}

func setMessageFolder(message *Message, folder string) {
	message.Folder = folder
	if message.ID != "" && !strings.Contains(message.ID, ":") {
		message.ID = folder + ":" + message.ID
	}
}

func mergeMessages(limit int, groups ...[]Message) []Message {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	merged := make([]Message, 0, total)
	for _, group := range groups {
		merged = append(merged, group...)
	}
	sortMessages(merged)
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// MergeMessagePages 合并多个邮件夹的结果，按时间倒序后截取指定页。
func MergeMessagePages(offset, limit int, groups ...[]Message) []Message {
	return paginateMessages(mergeMessages(0, groups...), offset, limit)
}

func paginateMessages(messages []Message, offset, limit int) []Message {
	if offset >= len(messages) || limit <= 0 {
		return []Message{}
	}
	end := offset + limit
	if end > len(messages) {
		end = len(messages)
	}
	return messages[offset:end]
}

func sortMessages(messages []Message) {
	sort.SliceStable(messages, func(i, j int) bool {
		left, leftOK := messageTime(messages[i])
		right, rightOK := messageTime(messages[j])
		if leftOK && rightOK && !left.Equal(right) {
			return left.After(right)
		}
		if leftOK != rightOK {
			return leftOK
		}
		return messages[i].ID > messages[j].ID
	})
}

func messageWithinDays(message Message, days int) bool {
	if days <= 0 {
		return true
	}
	timestamp, ok := messageTime(message)
	if !ok {
		return true
	}
	return !timestamp.Before(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
}

func messageTime(message Message) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, time.RFC1123Z, time.RFC1123} {
		if parsed, err := time.Parse(layout, message.Date); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// GetFull 获取单封邮件的完整内容(含正文)。
func (c *Client) GetFull(uid uint32) (*FullMessage, error) {
	return c.GetFullByID(fmt.Sprintf("%s:%d", FolderInbox, uid))
}

// GetFullByID 按 list 接口返回的 id（folder:uid）读取单封邮件完整内容。
func (c *Client) GetFullByID(messageID string) (*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	folder, uid, err := parseMessageID(messageID)
	if err != nil {
		return nil, err
	}
	mailbox, err := mailboxForFolder(folder)
	if err != nil {
		return nil, err
	}
	if _, err := c.cli.Select(mailbox, true); err != nil {
		return nil, err
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, section.FetchItem()}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var msg *imap.Message
	for item := range messages {
		msg = item
		break
	}
	if err := <-done; err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, fmt.Errorf("邮件不存在 (uid=%d)", uid)
	}

	full := &FullMessage{Message: toMessage(msg)}
	setMessageFolder(&full.Message, folder)
	// 解析正文
	if r := msg.GetBody(section); r != nil {
		if em, err := mail.ReadMessage(r); err == nil {
			body, htmlBody, contentType, inlineImages, _ := readBodyPartsWithInline(em)
			full.Body = body
			full.HTML = rewriteCIDImages(htmlBody, inlineImages)
			full.IsHTML = htmlBody != ""
			full.ContentType = contentType
			if full.Body == "" && full.HTML != "" {
				full.Body = stripHTML(full.HTML)
			}
			if full.Preview == "" {
				full.Preview = firstLine(full.Body)
			}
		}
	}
	return full, nil
}

// ---- 解析工具 ----

func toMessage(msg *imap.Message) Message {
	m := Message{}
	if msg.Uid > 0 {
		m.ID = fmt.Sprintf("%d", msg.Uid)
	}
	if msg.Envelope != nil {
		if len(msg.Envelope.From) > 0 {
			m.From = msg.Envelope.From[0].Address()
		}
		if len(msg.Envelope.To) > 0 {
			addrs := make([]string, 0, len(msg.Envelope.To))
			for _, a := range msg.Envelope.To {
				addrs = append(addrs, a.Address())
			}
			m.To = strings.Join(addrs, ", ")
		}
		m.Subject = decodeHeader(msg.Envelope.Subject)
		if !msg.Envelope.Date.IsZero() {
			m.Date = msg.Envelope.Date.Format(time.RFC3339)
		}
	}
	if m.From != "" {
		m.From = decodeHeader(m.From)
	}
	if m.To != "" {
		m.To = decodeHeader(m.To)
	}
	return m
}

// toMessageWithBody 在 toMessage 基础上解析正文填充 Preview(供 OTP 提取)。
func toMessageWithBody(msg *imap.Message) Message {
	m := toMessage(msg)
	if r := msg.GetBody(&imap.BodySectionName{}); r != nil {
		if em, err := mail.ReadMessage(r); err == nil {
			if body, err := readBody(em); err == nil {
				m.Preview = strings.TrimSpace(body)
			}
		}
	}
	return m
}

// decodeHeader 解码 RFC 2047 编码的邮件头(如 =?UTF-8?B?xxx?=)。
func decodeHeader(s string) string {
	if s == "" {
		return ""
	}
	dec := mime.WordDecoder{CharsetReader: charset.Reader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

func parseMessageID(messageID string) (string, uint32, error) {
	messageID = strings.TrimSpace(messageID)
	folder := FolderInbox
	uidText := messageID
	if index := strings.IndexByte(messageID, ':'); index > 0 {
		folder = strings.ToLower(strings.TrimSpace(messageID[:index]))
		uidText = messageID[index+1:]
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(uidText), 10, 32)
	if err != nil || uid == 0 {
		return "", 0, fmt.Errorf("邮件 id 无效: %s", messageID)
	}
	if _, err := mailboxForFolder(folder); err != nil {
		return "", 0, err
	}
	return folder, uint32(uid), nil
}

// readBody 读取邮件正文,优先 text/plain,其次从 HTML 提取纯文本。
func readBody(msg *mail.Message) (string, error) {
	plain, htmlBody, _, err := readBodyParts(msg)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(plain) != "" {
		return plain, nil
	}
	return stripHTML(htmlBody), nil
}

// readBodyParts 解析 text/plain、text/html 以及 multipart/alternative 邮件。
func readBodyParts(msg *mail.Message) (plain, htmlBody, contentType string, err error) {
	plain, htmlBody, contentType, _, err = readBodyPartsWithInline(msg)
	return
}

func readBodyPartsWithInline(msg *mail.Message) (plain, htmlBody, contentType string, inlineImages map[string]string, err error) {
	inlineImages = make(map[string]string)
	plain, htmlBody, contentType, err = readBodyPartsInto(msg, inlineImages)
	return
}

func readBodyPartsInto(msg *mail.Message, inlineImages map[string]string) (plain, htmlBody, contentType string, err error) {
	if msg == nil {
		return "", "", "", fmt.Errorf("邮件正文为空")
	}
	contentType = msg.Header.Get("Content-Type")
	mediaType, params, parseErr := mime.ParseMediaType(contentType)
	if parseErr != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
		params = nil
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			raw, readErr := readTransferDecoded(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
			return strings.TrimSpace(string(raw)), "", contentType, readErr
		}
		reader := multipart.NewReader(msg.Body, boundary)
		for {
			part, nextErr := reader.NextPart()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				return plain, htmlBody, contentType, nextErr
			}
			nested := &mail.Message{Header: mail.Header(part.Header), Body: part}
			partPlain, partHTML, _, partErr := readBodyPartsInto(nested, inlineImages)
			if partErr != nil {
				continue
			}
			if strings.TrimSpace(plain) == "" && strings.TrimSpace(partPlain) != "" {
				plain = strings.TrimSpace(partPlain)
			}
			if strings.TrimSpace(htmlBody) == "" && strings.TrimSpace(partHTML) != "" {
				htmlBody = strings.TrimSpace(partHTML)
			}
		}
		return plain, htmlBody, contentType, nil
	}

	raw, err := readTransferDecoded(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return "", "", contentType, err
	}
	if charsetName := strings.TrimSpace(params["charset"]); charsetName != "" {
		if decodedReader, decodeErr := charset.Reader(charsetName, bytes.NewReader(raw)); decodeErr == nil {
			if decoded, readErr := io.ReadAll(decodedReader); readErr == nil {
				raw = decoded
			}
		}
	}
	text := string(raw)
	switch strings.ToLower(mediaType) {
	case "text/plain":
		return strings.TrimSpace(text), "", contentType, nil
	case "text/html", "application/xhtml+xml":
		return "", text, contentType, nil
	default:
		if mediaType == "" || strings.HasPrefix(strings.ToLower(mediaType), "text/") {
			return strings.TrimSpace(text), "", contentType, nil
		}
		contentID := strings.Trim(strings.TrimSpace(msg.Header.Get("Content-ID")), "<>")
		if strings.HasPrefix(strings.ToLower(mediaType), "image/") && contentID != "" && len(raw) <= 15*1024*1024 {
			inlineImages[strings.ToLower(contentID)] = "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)
		}
		return "", "", contentType, nil
	}
}

var cidReferencePattern = regexp.MustCompile(`(?i)cid:[^\s"'()<>]+`)

func rewriteCIDImages(rawHTML string, inlineImages map[string]string) string {
	if rawHTML == "" || len(inlineImages) == 0 {
		return rawHTML
	}
	return cidReferencePattern.ReplaceAllStringFunc(rawHTML, func(reference string) string {
		contentID := strings.ToLower(strings.TrimPrefix(strings.ToLower(reference), "cid:"))
		if dataURL, ok := inlineImages[contentID]; ok {
			return dataURL
		}
		return reference
	})
}

func readTransferDecoded(body io.Reader, encoding string) ([]byte, error) {
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	switch encoding {
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(body))
	case "base64":
		return io.ReadAll(base64.NewDecoder(base64.StdEncoding, body))
	default:
		return io.ReadAll(body)
	}
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if index := strings.IndexAny(value, "\r\n"); index >= 0 {
		return value[:index]
	}
	if len(value) > 240 {
		return value[:240]
	}
	return value
}

// stripHTML 提取邮件 HTML 中的可读正文，忽略 head、样式和脚本内容。
func stripHTML(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	var body *html.Node
	var findBody func(*html.Node)
	findBody = func(node *html.Node) {
		if body != nil {
			return
		}
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "body") {
			body = node
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			findBody(child)
		}
	}
	findBody(doc)
	if body == nil {
		body = doc
	}

	var text strings.Builder
	block := map[string]bool{"p": true, "div": true, "tr": true, "table": true, "section": true, "article": true, "header": true, "footer": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			tag := strings.ToLower(node.Data)
			switch tag {
			case "head", "style", "script", "noscript", "svg", "template":
				return
			case "br":
				text.WriteByte('\n')
			case "li":
				text.WriteString("\n- ")
			}
		}
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode && block[strings.ToLower(node.Data)] {
			text.WriteByte('\n')
		}
	}
	walk(body)

	lines := strings.Split(strings.ReplaceAll(text.String(), "\u00a0", " "), "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" && (len(clean) == 0 || clean[len(clean)-1] == "") {
			continue
		}
		clean = append(clean, line)
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}
