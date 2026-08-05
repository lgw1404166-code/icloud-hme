// Package mail 实现 iCloud 邮件 IMAP 读取客户端。
//
// 通过 Apple 应用专用密码连接 imap.mail.me.com:993,
// 拉取隐私邮箱别名收到的邮件。对应原 Python 项目 icloud_mail.py。
package mail

import (
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/charset"
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
	ContentType string `json:"content_type"`
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

	// 拉取完整正文,以便填充 Preview(OTP 验证码在正文中)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		section.FetchItem(),
	}

	messages := make(chan *imap.Message, limit)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.Fetch(seqset, items, messages)
	}()

	out := make([]Message, 0, limit)
	for msg := range messages {
		m := toMessageWithBody(msg)
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
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 20
	}
	folder, err := NormalizeFolder(folder)
	if err != nil {
		return nil, err
	}
	if folder == FolderAll {
		inbox, err := c.findByRecipientMailbox(recipient, inboxMailbox, FolderInbox, limit, days)
		if err != nil {
			return nil, fmt.Errorf("搜索收件箱失败: %w", err)
		}
		junk, err := c.findByRecipientMailbox(recipient, junkMailbox, FolderJunk, limit, days)
		if err != nil {
			return nil, fmt.Errorf("搜索垃圾邮件失败: %w", err)
		}
		return mergeMessages(limit, inbox, junk), nil
	}
	mailbox, _ := mailboxForFolder(folder)
	return c.findByRecipientMailbox(recipient, mailbox, folder, limit, days)
}

// findByRecipientMailbox 先尝试 IMAP TO 搜索，失败则拉取邮件后本地过滤。
func (c *Client) findByRecipientMailbox(recipient, mailbox, folder string, limit int, days int) ([]Message, error) {
	// 先尝试服务端 TO 搜索
	_, err := c.cli.Select(mailbox, true)
	if err != nil {
		return nil, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.Header.Add("To", recipient)
	if days > 0 {
		since := time.Now().AddDate(0, 0, -days)
		criteria.Since = since
	}
	uids, err := c.cli.UidSearch(criteria)
	if err == nil && len(uids) > 0 {
		return c.fetchByUIDs(uids, limit, folder)
	}

	// fallback: 拉取当前邮件夹后本地过滤
	all, err := c.listMailbox(mailbox, folder, limit*3, days)
	if err != nil {
		return nil, err
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
	return out, nil
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

	// 拉取完整正文,以便填充 Preview(OTP 验证码在正文中)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, section.FetchItem()}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	out := make([]Message, 0, len(uids))
	for msg := range messages {
		m := toMessageWithBody(msg)
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
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if _, err := c.cli.Select("INBOX", true); err != nil {
		return nil, err
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchRFC822}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	msg := <-messages
	if err := <-done; err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, fmt.Errorf("邮件不存在 (uid=%d)", uid)
	}

	full := &FullMessage{Message: toMessage(msg)}
	// 解析正文
	if r := msg.GetBody(&imap.BodySectionName{}); r != nil {
		if em, err := mail.ReadMessage(r); err == nil {
			body, _ := readBody(em)
			full.Body = body
			full.ContentType = em.Header.Get("Content-Type")
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

var htmlTag = regexp.MustCompile(`<[^>]+>`)

// readBody 读取邮件正文,优先 text/plain,其次从 HTML 提取纯文本。
func readBody(msg *mail.Message) (string, error) {
	ct := msg.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		raw, _ := io.ReadAll(msg.Body)
		// quoted-printable 解码
		if strings.Contains(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			r := quotedprintable.NewReader(strings.NewReader(string(raw)))
			raw, _ = io.ReadAll(r)
		}
		return stripHTML(string(raw)), nil
	}
	// 默认当 text/plain
	raw, err := io.ReadAll(msg.Body)
	if err != nil {
		return "", err
	}
	if strings.Contains(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
		r := quotedprintable.NewReader(strings.NewReader(string(raw)))
		raw, _ = io.ReadAll(r)
	}
	return string(raw), nil
}

// stripHTML 粗略剥离 HTML 标签,保留可读文本。
func stripHTML(html string) string {
	// 换行标签转换行
	html = strings.ReplaceAll(html, "<br>", "\n")
	html = strings.ReplaceAll(html, "<br/>", "\n")
	html = strings.ReplaceAll(html, "<br />", "\n")
	html = strings.ReplaceAll(html, "</p>", "\n")
	html = strings.ReplaceAll(html, "</div>", "\n")
	html = strings.ReplaceAll(html, "</tr>", "\n")
	html = strings.ReplaceAll(html, "<li>", "\n- ")
	// 去掉所有标签
	html = htmlTag.ReplaceAllString(html, "")
	// 反转义常见实体
	html = strings.ReplaceAll(html, "&nbsp;", " ")
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	// 压缩多余空白
	lines := strings.Split(html, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
