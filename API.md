# iCloud Hide My Email API 文档

## 概述

HTTP JSON API，所有接口返回统一格式：

- Base URL: `https://icloud.ezaiclub.com`
- 外部请求认证: `X-API-Key: <API_KEY>`
- `Content-Type`: 带 JSON 请求体时使用 `application/json`

示例请求头：

```http
X-API-Key: <API_KEY>
Content-Type: application/json
```

```json
{
  "success": true,
  "data": {},
  "message": ""
}
```

**错误响应:**
- `401 Unauthorized` — API Key 无效，或 iCloud Cookie 会话失效（以响应消息区分）
- `400 Bad Request` — 参数错误
- `404 Not Found` — 账号不存在
- `424 Failed Dependency` — Apple 创建接口、邮件 IMAP 或 iCloud Web 邮件依赖不可用
- `429 Too Many Requests` — 同账号已有创建请求、仍在本地冷却期，或 Apple 返回限流

创建接口的 `429` 和带冷却的 `424` 响应会包含 `Retry-After` 响应头（单位为秒）。

---

## 核心接口

### 1. 创建 HME 别名

```http
POST /api/create
Content-Type: application/json

{
  "account_id": "acc_1",
  "label": "注册某网站"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "email": "xyz123@icloud.com",
    "label": "注册某网站",
    "created_at": "2024-01-15T10:30:00Z",
    "account_id": "acc_1"
  }
}
```

**参数说明:**
- `account_id` (条件必填) — 账号 ID；系统中只有一个账号时可以省略，存在多个账号时必须提供
- `label` (可选) — 别名标签，默认为 "Created YYYY-MM-DD HH:mm"

账号 ID 通过 `GET /api/accounts` 获取。删除账号再重新添加会生成新的 ID，外部调用方应重新查询，不能继续使用已删除账号的 ID。

**错误情况:**
- `401` — Cookie 过期，需更新
- `429` — 创建请求过于频繁；按 `Retry-After` 等待后再请求
- `424` — Apple 创建接口返回了其它错误；响应中的 `error.upstream_status` 和 `error.upstream_body` 提供上游摘要

为避免触发 Apple 风控，同一账号的创建操作具有以下约束：

- 同一时间只允许一个创建请求；并发请求立即返回 `429`
- 两次创建尝试至少间隔 30 秒
- Apple 限流或创建失败后默认冷却 10 分钟；Apple 返回更长的 `Retry-After` 时以其为准
- 每次 API 调用只执行一次 `generate` 和一次 `reserve`，不会自动重试创建

---

### 2. 读取邮件

```http
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&folder=all&limit=20&days=7
```

**响应 (走 IMAP,App Password):**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "folder": "all",
    "count": 2,
    "method": "imap",
    "messages": [
      {
        "id": "junk:1042",
        "from": "GitHub <noreply@github.com>",
        "to": "xyz123@icloud.com",
        "subject": "[GitHub] Please verify your email address",
        "date": "2026-07-09T14:32:10+08:00",
        "preview": "Almost done! To finish setting up your account, we just need to verify..",
        "folder": "junk"
      }
    ]
  }
}
```

**响应 (回退到 Web API,Cookie):** `method` 变为 `web_api`
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "folder": "inbox",
    "count": 1,
    "method": "web_api",
    "messages": [
      {
        "id": "AQMkAD...",
        "from": "GitHub <noreply@github.com>",
        "to": "xyz123@icloud.com",
        "subject": "[GitHub] Please verify your email address",
        "date": "Wed, 09 Jul 2026 06:32:10 GMT",
        "preview": "Almost done! To finish setting up your account..",
        "folder": "inbox"
      }
    ]
  }
}
```

**参数说明:**
- `account_id` (必填) — 账号 ID
- `alias` (可选) — 只返回发到该别名的邮件
- `folder` (可选) — `all`、`inbox` 或 `junk`，默认 `all`;别名查询同样按此范围搜索
- `limit` (可选) — 返回邮件数量，默认 20
- `days` (可选) — 查找最近几天的邮件，默认 7 (仅 IMAP 模式)

**邮件读取双路径 (自动选择):**
1. **优先: IMAP (App Password)** — 设置了 App Password 时使用,支持服务端按收件人搜索
2. **回退: Web API (Cookie 认证)** — 仅 `folder=inbox` 时可回退;`all` 和 `junk` 必须使用 IMAP

响应中 `"method": "imap"` 或 `"method": "web_api"` 标识实际使用的读取方式。

**别名过滤逻辑:**
- **IMAP (`FindByRecipientInFolder`):** 按所选邮件夹使用原生 IMAP `TO` 头搜索;`all` 会合并 `INBOX` 和 `Junk`，按时间倒序后应用 `limit`
- **Web API (`FindByAlias`):** iCloud Web API 不支持按收件人搜索,拉取 `limit*2` (至少 50) 条后本地对 `Subject`/`From`/`To` 做包含匹配

**返回字段差异 (两条路径):**
- `id` — IMAP 是带邮件夹前缀的 UID (`inbox:1042` / `junk:1042`),Web API 是 iCloud GUID
- `folder` — 邮件所在文件夹: `inbox` 或 `junk`
- `date` — IMAP 走 RFC3339,Web API 是原始邮件头 RFC1123 串
- `preview` — 正文摘要,非完整正文

---

## 账号管理接口

### 3. 列出所有账号

```http
GET /api/accounts
```

**响应:**
```json
{
  "success": true,
  "data": [
    {
      "id": "acc_1",
      "name": "主号",
      "host": "imap.mail.me.com"
    }
  ]
}
```

**注意:** 响应中不包含敏感信息（cookies、app_passwords）

---

### 4. 添加账号

**请求（Cookie 必填）:**
```http
POST /api/accounts
Content-Type: application/json

{
  "name": "新账号",
  "cookies": "{\"x-apple-session-token\":\"token_value\"}",
  "host": "icloud.com",
  "proxy": "http://user:pass@host:port"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_3",
    "name": "新账号",
    "host": "icloud.com",
    "status": "active"
  }
}
```

**参数说明:**
- `name` (必填) — 账号名称
- `cookies` (必填) — 浏览器导出的 Cookie 字符串,支持三种格式:
  - JSON: `"{\"name\":\"value\"}"`
  - 浏览器导出数组: `[{"name":"name","value":"value"}]`
  - Header: `"name1=value1; name2=value2"`
- `host` (可选) — iCloud 域名,默认 `icloud.com`
- `proxy` (可选) — HTTP/SOCKS5 代理

**注意:** Cookie 创建时会立即校验；校验失败的账号仍会保存为 `error`，可通过更新 Cookie 修正。

---

### 5. 删除账号

```http
DELETE /api/accounts/:id
```


**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_3"
  }
}
```

**错误情况:**
- `404` — 账号不存在

---

### 6. 设置 App Password

```http
POST /api/accounts/:id/password
Content-Type: application/json

{
  "icloud_email": "your_email@icloud.com",
  "app_password": "xxxx-xxxx-xxxx-xxxx"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_1",
    "icloud_email": "your_email@icloud.com"
  }
}
```

**参数说明:**
- `icloud_email` (必填) — iCloud 邮箱地址
- `app_password` (必填) — App 专用密码

**用途:** App Password 用于 IMAP 邮件读取，生成方式见 [appleid.apple.com](https://appleid.apple.com)

---

## 别名管理接口

### 7. 列出所有别名

```http
GET /api/aliases?account_id=acc_1
```

**响应:**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 15,
    "aliases": [
      {
        "email": "xyz123@icloud.com",
        "anonymousId": "abc123",
        "label": "注册某网站",
        "active": true,
        "createdAt": "2024-01-15T10:30:00Z"
      }
    ]
  }
}
```

**参数说明:**
- `account_id` (必填) — 账号 ID

**别名字段:**
- `email` — HME 邮箱地址
- `anonymousId` — 别名唯一标识（用于停用/激活/删除）
- `label` — 用户定义的标签
- `active` — 是否激活
- `createdAt` — 创建时间

---

### 8. 停用别名

```http
POST /api/aliases/:id/deactivate
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `account_id` (必填) — 账号 ID

**说明:** 停用后别名不再接收邮件，但可随时激活恢复

---

### 9. 激活别名

```http
POST /api/aliases/:id/reactivate
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `account_id` (必填) — 账号 ID

**说明:** 激活已停用的别名，恢复邮件接收

---

### 10. 删除别名

```http
DELETE /api/aliases/:id
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "anonymous_id": "abc123"
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `account_id` (必填) — 账号 ID

**注意:** 删除不可恢复！如果直接删除失败，会先停用再删除

---

## 使用示例

### curl 示例

```bash
BASE_URL="https://icloud.ezaiclub.com"
API_KEY="<API_KEY>"

# 获取账号 ID
curl -fsS "$BASE_URL/api/accounts" \
  -H "X-API-Key: $API_KEY"

# 创建别名
curl -fsS -X POST "$BASE_URL/api/create" \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1", "label": "GitHub"}'

# 默认同时查询收件箱和垃圾邮件
curl -fsS --get "$BASE_URL/api/inbox" \
  -H "X-API-Key: $API_KEY" \
  --data-urlencode "account_id=acc_1" \
  --data-urlencode "alias=xyz123@icloud.com" \
  --data-urlencode "folder=all" \
  --data-urlencode "limit=20" \
  --data-urlencode "days=7"

# 列出别名
curl -fsS "$BASE_URL/api/aliases?account_id=acc_1" \
  -H "X-API-Key: $API_KEY"

# 停用别名
curl -fsS -X POST "$BASE_URL/api/aliases/abc123/deactivate" \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1"}'

# 删除别名
curl -fsS -X DELETE "$BASE_URL/api/aliases/abc123" \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1"}'
```

### Python 示例

```python
import requests

BASE_URL = "https://icloud.ezaiclub.com/api"
API_KEY = "<API_KEY>"
session = requests.Session()
session.headers.update({"X-API-Key": API_KEY})

# 创建别名
resp = session.post(f"{BASE_URL}/create", json={
    "account_id": "acc_1",
    "label": "Netflix"
})
print(resp.json())

# 读取邮件
resp = session.get(f"{BASE_URL}/inbox", params={
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "folder": "all",
    "limit": 20,
    "days": 7
})
print(resp.json())

# 列出别名
resp = session.get(f"{BASE_URL}/aliases", params={"account_id": "acc_1"})
for alias in resp.json()["data"]["aliases"]:
    print(f"{alias['email']} - {alias['label']} (active: {alias['active']})")
```

---

## iCloud 上游认证说明

### Cookie 认证 (推荐,功能最完整)

用于：创建别名、列出别名、停用/激活/删除别名、**读取邮件**

**获取方式:**
1. 浏览器登录 [icloud.com](https://www.icloud.com) 或 [https://www.icloud.com.cn](https://www.icloud.com.cn) (国区)
2. 使用可信的 Cookie 导出工具导出 `icloud.com` / `www.icloud.com` 的全部 Cookie，或从 iCloud 请求的 `Cookie` 请求头复制完整内容
3. 粘贴对象 JSON、浏览器导出数组 JSON 或 `name=value; name2=value2` Header 格式

**关键 Cookie:**
- `X-APPLE-WEBAUTH-TOKEN` — 认证 token
- `X-APPLE-WEBAUTH-USER` — 含 dsid (`v=1:s=1:d=22789132008`)
- `X-APPLE-WEBAUTH-HSA-TRUST` — 设备信任 token
- `X-APPLE-DS-WEB-SESSION-TOKEN` — 会话 token

**有效期:** 约 24 小时

### App Password 认证 (IMAP 回退)

仅用于 Web API 失败时的邮件读取回退

**获取方式:**
1. 登录 [appleid.apple.com](https://appleid.apple.com)
2. 登录和安全 → App 专用密码
3. 生成新密码

---

## 技术说明

### 邮件读取实现

**Web API 路径** (`internal/mail/web_client.go`):
1. 调用 `setup.icloud.com.cn/setup/ws/1/validate` 获取 `mccgateway` URL
2. 调用 `mccgateway/mailws2/v1/thread/search` 读取邮件

**⚠️ 已知坑:**
- `validate` 返回的 mccgateway URL 可能带 `:443` 端口 (如 `p217-mccgateway.icloud.com.cn:443`)
- tls-client 的 cookie jar 按不带端口的 host 存储 cookie
- 带端口请求时 cookie 无法附加,导致 403
- **解决:** 解析 URL 后剥离端口号

**clientBuildNumber:** 与浏览器一致,当前 `2624Build22`

**IMAP 路径** (`internal/mail/client.go`):
- 标准 IMAP 协议,连接 `imap.mail.me.com:993`
- 需要 App Password

---

## 错误处理

### 会话失效 (401)

```json
{
  "success": false,
  "message": "iCloud 会话失效，请更新 Cookie: HTTP 401"
}
```

**解决:** 更新 `accounts.json` 中的 Cookie

### Apple 限流 (429)

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 600
```

```json
{
  "success": false,
  "message": "Apple 暂时限制了自动创建，请在冷却结束后重试: 创建别名失败: generate 失败: HTTP 429",
  "error": {
    "upstream_status": 429,
    "upstream_body": "Apple 返回的错误摘要",
    "retry_after_seconds": 600
  }
}
```

客户端必须遵守 `Retry-After`，不要通过代理 IP 并发重试；调用方 IP 不会改变本服务访问 Apple 时使用的出口 IP 和账号会话。

### Apple 其它错误 (424)

Apple 返回非限流错误或连接异常时，本服务使用 `424 Failed Dependency` 返回 JSON，而不是 `502`。这样 Cloudflare 橙云不会把应用错误替换成通用 HTML 502 页面。失败后若进入冷却，响应同样带 `Retry-After`。

### 参数错误 (400)

```json
{
  "success": false,
  "message": "参数错误: account_id 必填"
}
```

---

## 限制

- **创建频率**: 同账号最短间隔 30 秒；上游失败后默认冷却 10 分钟
- **Cookie 有效期**: 约 24 小时，需定期更新
- **邮件读取**: 依赖 IMAP 连接，超时默认 30 秒
