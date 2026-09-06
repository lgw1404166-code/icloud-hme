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
- `424 Failed Dependency` — Apple 创建接口或账号配置的 MoeMail 收件 API 不可用
- `429 Too Many Requests` — 同账号已有创建请求、仍在本地冷却期，或 Apple 返回限流

创建接口的 `429` 和带冷却的 `424` 响应会包含 `Retry-After` 响应头（单位为秒）。

---

## 核心接口

### 1. 获取 HME 别名（从后台别名池领取）

```http
POST /api/create
Content-Type: application/json

{
  "account_id": "acc_1",
  "caller": "chatgpt"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "email": "xyz123@icloud.com",
    "anonymousId": "abc123",
    "caller": "chatgpt",
    "used_by": ["chatgpt"],
    "created_at": "2024-01-15T10:30:00Z",
    "protocol": "local_pool",
    "account_id": "acc_1"
  }
}
```

**参数说明:**
- `account_id` (条件必填) — 账号 ID；系统中只有一个账号时可以省略，存在多个账号时必须提供
- `caller` (必填) — 调用方身份字符串，例如 `chatgpt`、`moxt`；大小写会统一按同一身份处理
- 兼容字段: `client`、`identity` 也会作为调用方身份读取

账号 ID 通过 `GET /api/accounts` 获取。删除账号再重新添加会生成新的 ID，外部调用方应重新查询，不能继续使用已删除账号的 ID。
该接口默认只返回已启用账号，因此停用账号不会再被业务调用方选中。管理界面需要查看并重新启用停用账号时，使用 `GET /api/accounts?include_disabled=true`。

**错误情况:**
- `400` — 缺少 `caller`
- `401` — 同步别名列表时发现 Cookie 过期，需重新登录并更新 Cookie
- `409` — 当前本地别名池中没有可分配给该调用方的新邮箱

从此版本起，外部调用 `POST /api/create` 只做“领取/分配”，不会向 Apple 发起创建请求。领取成功即永久记录该 `caller` 的使用标签；如业务在注册前发生明确失败，可调用通用释放接口移除自己的标签。后台别名池任务会按固定节奏预先创建邮箱：

- 默认每小时 10 个（约 6 分钟 1 个），通过 `ICLOUD_HME_AUTO_CREATE_PER_HOUR` 调整
- `ICLOUD_HME_AUTO_CREATE=0|off|false` 可关闭后台创建
- `ICLOUD_HME_AUTO_CREATE_MAX_TOTAL` 可设置单账号最多保留多少别名，`0` 表示不设本地上限
- 后台创建使用 Apple 账户管理入口，创建失败或触发限流后进入本地冷却

---

### 2. 读取邮件

```http
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&folder=all&limit=20&days=7
```

**响应 (MoeMail):**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "folder": "all",
    "count": 2,
    "method": "moemail_api",
    "messages": [
      {
        "id": "junk:1042",
        "from": "GitHub <noreply@github.com>",
        "to": "xyz123@icloud.com",
        "subject": "[GitHub] Please verify your email address",
        "date": "2026-07-09T14:32:10+08:00",
        "folder": "junk"
      }
    ]
  }
}
```

**参数说明:**
- `account_id` (必填) — 账号 ID
- `alias` (必填) — 只返回能精确确认发到该别名的邮件
- `folder` (可选) — `all` 或 `inbox`，默认 `all`；MoeMail API 不提供垃圾邮件目录
- `limit` (可选) — 返回邮件数量，默认 20
- `days` (可选) — 查找最近几天的邮件，默认 `0` 表示不限

**邮件读取方式:**
1. 每个 Apple 账号配置一个固定的 MoeMail 转发收件箱。
2. `GET /api/inbox` 和 `GET /api/inbox/message` 只使用该提供商的公开 API；iCloud IMAP、iCloud Web Mail 和 App Password 均不参与读取。

响应中的 `method` 为 `moemail_api`。

**别名过滤逻辑:**
- **MoeMail:** 使用 `GET /api/emails` 定位固定收件箱，随后调用 `GET /api/emails/{mailboxId}` 与 `GET /api/emails/{mailboxId}/{messageId}`。
- **精确过滤:** 仅匹配 `to`、`to_address`、`X-Original-To`、`Delivered-To` 等收件人字段或原始头中完整等于 `alias` 的邮箱地址；不会以主题或发件人进行猜测匹配。
- **实例要求:** 固定转发场景下，MoeMail 必须保留 Apple 转发前的 HME 原始收件人并通过 `to_address` 或邮件头返回。官方 MoeMail Worker 默认只保存转发目标地址，无法准确区分多个 HME 别名；服务检测到该情况会返回 `424`，不会猜测归属。

**返回字段差异 (两条路径):**
- `id` — MoeMail 提供商的邮件 ID，取正文时原样传回
- `folder` — 固定为 `inbox`
- `date` — 提供商时间戳转换为 RFC3339
- `preview` — 正文摘要,非完整正文

列表接口现在只拉取信封摘要（UID、发件人、收件人、主题、日期），正文按需读取，页面点击邮件后才加载完整内容。

### 2.1 读取单封邮件正文

```http
GET /api/inbox/message?account_id=acc_1&alias=xyz123@icloud.com&id=1042
```

**响应:**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "method": "moemail_api",
    "message": {
      "id": "1042",
      "from": "GitHub <noreply@github.com>",
      "to": "xyz123@icloud.com",
      "subject": "Verify",
      "date": "2026-07-09T14:32:10+08:00",
      "folder": "inbox",
      "body": "纯文本正文",
      "html": "<html>...</html>",
      "content_type": "multipart/alternative; boundary=...",
      "is_html": true
    }
  }
}
```

HTML 邮件会同时返回 `html` 与从 HTML 提取的 `body`；管理页面使用 sandbox iframe 展示 HTML 正文。

### 2.2 配置账号转发收件箱

```http
PUT /api/accounts/:id/mail-receiver
Content-Type: application/json

{
  "address": "relay@example.com",
  "api_key": "your-moemail-api-key",
  "base_url": "https://moemail.app"
}
```

`base_url` 填 MoeMail 实例地址，`mailbox_id` 可选。接口响应和 `GET /api/accounts` 只返回地址、实例和邮箱 ID，不会返回 API Key。先在 Apple 隐私邮箱页面将“转发至”改为同一个固定邮箱。

```http
DELETE /api/accounts/:id/mail-receiver
```

清除该账号的转发收件箱凭据，不会删除 Apple Cookie、HME 别名或本地别名池记录。

---

### 2.3 OTP webhook 与重新登录配置

#### 接收 Android/SMSGate 验证码

```http
POST /api/otp/inbound?token=<ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN>
Content-Type: application/json

{
  "event": "sms:received",
  "payload": {
    "sender": "Apple",
    "message": "Your Apple ID Code is: 123456.",
    "receivedAt": "2026-08-10T12:00:00+08:00"
  }
}
```

服务端只接受 Apple 相关短信中的 6 位验证码。非 Apple 短信会返回 `accepted:false`，完整短信正文不会进入验证码缓存。

**响应:**
```json
{
  "success": true,
  "data": {
    "accepted": true,
    "provider": "apple",
    "received_at": "2026-08-10T12:00:00+08:00",
    "expires_at": "2026-08-10T12:05:00+08:00"
  }
}
```

#### 获取最新验证码

```http
GET /api/otp/latest?token=<ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN>&provider=apple
```

#### 获取重新登录配置状态

```http
GET /api/relogin/config
```

**响应:**
```json
{
  "success": true,
  "data": {
    "enabled": false,
    "mode": "apple_protocol_sms",
    "manual_login_url": "https://account.apple.com/account/manage/section/privacy",
    "protocol_timeout_seconds": 180,
    "apple_id_configured": false,
    "apple_password_configured": false,
    "otp": {
      "provider": "smsgate_webhook",
      "webhook_enabled": true,
      "webhook_path": "/api/otp/inbound",
      "latest_path": "/api/otp/latest",
      "ttl_seconds": 300
    }
  }
}
```

环境变量统一放在 `ICLOUD_HME_RELOGIN_*` 下：

- `ICLOUD_HME_RELOGIN_ENABLED`
- `ICLOUD_HME_RELOGIN_MODE`
- `ICLOUD_HME_RELOGIN_APPLE_ID`
- `ICLOUD_HME_RELOGIN_APPLE_PASSWORD`
- `ICLOUD_HME_RELOGIN_OTP_PROVIDER`
- `ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN`
- `ICLOUD_HME_RELOGIN_OTP_TTL`
- `ICLOUD_HME_RELOGIN_MANUAL_URL`
- `ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT`

直接运行程序时会读取项目根目录 `.env`，系统环境变量优先级更高；Docker Compose 会从 `.env` 透传同一组变量。未启用 `ICLOUD_HME_RELOGIN_ENABLED` 时，管理页面仍会弹窗并打开手动登录页。

管理页面的 **配置** 菜单会读取同一个接口，集中展示自动重新登录开关、账号密码配置状态、OTP webhook 状态、TTL、手动登录 URL；也可以通过页面保存，后端会写入 `.env` 并立即应用到当前进程。

#### 更新重新登录配置

```http
PUT /api/relogin/config
Content-Type: application/json

{
  "enabled": true,
  "mode": "apple_protocol_sms",
  "apple_id": "name@example.com",
  "apple_password": "apple-password",
  "manual_login_url": "https://account.apple.com/account/manage/section/privacy",
  "protocol_timeout": "3m",
  "otp": {
    "provider": "smsgate_webhook",
    "webhook_token": "random-token",
    "ttl": "5m"
  }
}
```

密码和 token 不会在响应里回显；页面里留空表示保持现有值。

---

## 账号管理接口

### 3. 列出可用账号

```http
GET /api/accounts
```

默认只返回 `enabled=true` 的账号。需要进行账号管理时，可通过 `include_disabled=true` 查询包括停用账号在内的完整列表：

```http
GET /api/accounts?include_disabled=true
```

**响应:**
```json
{
  "success": true,
  "data": [
    {
      "id": "acc_1",
      "name": "主号",
      "host": "icloud.com",
      "enabled": true,
      "auto_create_enabled": true
    }
  ]
}
```

**注意:** 响应中不包含敏感信息（cookies、mail_receiver.api_key）

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

要启用后台别名池创建，请同时导入 `icloud.com` 与 `account.apple.com` / `appleid.apple.com` Cookie，并保留 `myacinfo`、`caw`/`caw-at` 或 `awat` 等账户会话 Cookie。后台任务检测到账户会话后，会优先使用 Apple 账户管理接口创建新别名并写入本地池；外部 `POST /api/create` 只从池中领取，响应为 `protocol: "local_pool"`。

Apple 账户管理会话不是永久令牌：当前 `/account/manage/gs/ws/token` 响应中的 `caw-at`、`awat` 为 `Max-Age=900` 的滚动 Cookie。服务会保存每次响应更新的 `scnt` 和 CookieJar Cookie；持续调用时可自动滚动，长时间空闲后仍需从已登录的 Apple 账户页重新导入会话 Cookie。

服务默认通过后台任务每 10 分钟刷新一次账户管理会话。可设置 `ICLOUD_HME_ACCOUNT_REFRESH_INTERVAL=8m` 调整间隔，或设置为 `0` / `off` 关闭。别名池创建由 `ICLOUD_HME_AUTO_CREATE*` 系列环境变量控制。

**注意:** Cookie 创建时会立即校验；校验失败的账号仍会保存为 `error`，可通过更新 Cookie 修正。

---

### 5. 设置账号启用开关

此开关控制账号是否参与别名领取、别名同步与管理、邮件读取以及后台自动创建。停用账号仍会保留在账号列表中，Cookie、转发收件箱配置和重新启用操作不受影响。

```http
PUT /api/accounts/:id/enabled
Content-Type: application/json

{
  "enabled": false
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_1",
    "enabled": false
  }
}
```

停用后，`POST /api/create` 不会再选择该账号；显式指定停用账号会返回 `409 Conflict`。

### 6. 设置账号级自动创建开关

此开关只控制后台别名池是否为指定账号向 Apple 创建新别名，不影响收件箱、已有别名或手动领取。

```http
PUT /api/accounts/:id/auto-create
Content-Type: application/json

{
  "enabled": false
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_1",
    "enabled": false
  }
}
```

### 7. 删除账号

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

## 别名管理接口

### 9. 列出所有别名

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
        "createdAt": "2024-01-15T10:30:00Z",
        "used_by": ["chatgpt", "moxt"],
        "used_by_count": 2,
        "last_used_at": "2026-08-10T12:30:00+08:00"
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
- `used_by` — 本项目记录中领取过该邮箱的调用方身份列表
- `used_by_count` — 调用方数量
- `last_used_at` — 最近一次被领取的时间

---

### 9.1 释放调用方领取记录

业务调用方在确认自己的注册流程尚未完成时，可释放已领取的别名，让该 `caller` 后续能够再次领取它。释放不会停用或删除 Apple 侧别名，也不会影响其他调用方的标签。

```http
POST /api/aliases/release
Content-Type: application/json

{
  "account_id": "acc_1",
  "anonymous_id": "abc123",
  "caller": "lovart"
}
```

### 9.2 修复已使用别名的调用方标签

当外部业务发现某个别名已经完成注册，但本地调用方标签尚未记录时，使用此幂等接口补写标签。该接口不会释放、停用或删除 Apple 侧别名。

```http
POST /api/aliases/:id/callers
Content-Type: application/json

{
  "account_id": "acc_1",
  "caller": "lovart"
}
```

`id` 是别名的 `anonymousId`。成功后该 `caller` 会永久计入本地 `used_by`，直到显式调用释放接口。

**参数说明:**
- `account_id`（必填）— 别名所属账号 ID
- `anonymous_id`（必填）— `POST /api/create` 返回的 `anonymousId`
- `caller`（必填）— 要释放的调用方身份，大小写不敏感

下列旧接口保留以兼容已有客户端：

```http
DELETE /api/aliases/:id/callers/:caller
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
    "account_id": "acc_1",
    "anonymousId": "abc123",
    "email": "xyz123@icloud.com",
    "caller": "moxt",
    "used_by": ["chatgpt"]
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `:caller` (路径参数) — 要删除的调用方标识，大小写不敏感
- `account_id` (必填) — 别名所属账号 ID

该操作只删除本地保存的调用方领取记录，不会停用或删除 Apple 侧别名。记录删除后，该调用方可以再次领取这个别名。

---

### 10. 停用别名

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

### 11. 激活别名

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

### 12. 删除别名

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

# 领取别名
curl -fsS -X POST "$BASE_URL/api/create" \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1", "caller": "chatgpt"}'

# 默认同时查询收件箱和垃圾邮件
curl -fsS --get "$BASE_URL/api/inbox" \
  -H "X-API-Key: $API_KEY" \
  --data-urlencode "account_id=acc_1" \
  --data-urlencode "alias=xyz123@icloud.com" \
  --data-urlencode "folder=all" \
  --data-urlencode "limit=20" \
  --data-urlencode "days=7"

# 点击列表项后按 id 读取正文
curl -fsS --get "$BASE_URL/api/inbox/message" \
  -H "X-API-Key: $API_KEY" \
  --data-urlencode "account_id=acc_1" \
  --data-urlencode "id=junk:1042"

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

# 领取别名
resp = session.post(f"{BASE_URL}/create", json={
    "account_id": "acc_1",
    "caller": "chatgpt"
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
    print(f"{alias['email']} - used_by={alias.get('used_by', [])} (active: {alias['active']})")
```

---

## iCloud 上游认证说明

### Cookie 认证 (推荐,功能最完整)

用于：后台定时创建别名、列出别名、停用/激活/删除别名

**获取方式:**
1. 浏览器登录 [icloud.com](https://www.icloud.com) 或 [https://www.icloud.com.cn](https://www.icloud.com.cn) (国区)，并登录 `https://account.apple.com/account/manage/section/privacy`
2. 使用可信的 Cookie 导出工具导出 `icloud.com` / `www.icloud.com` 与 `account.apple.com` / `appleid.apple.com` 的全部 Cookie，或从请求的 `Cookie` 请求头复制完整内容
3. 粘贴对象 JSON、浏览器导出数组 JSON 或 `name=value; name2=value2` Header 格式

**关键 Cookie:**
- `X-APPLE-WEBAUTH-TOKEN` — 认证 token
- `X-APPLE-WEBAUTH-USER` — 含 dsid (`v=1:s=1:d=22789132008`)
- `X-APPLE-WEBAUTH-HSA-TRUST` — 设备信任 token
- `X-APPLE-DS-WEB-SESSION-TOKEN` — 会话 token

账户页短 Cookie 会滚动刷新；服务默认每 10 分钟保活。根会话失效时，管理页面会提示重新登录并打开 Apple 隐私邮箱页面，登录后更新 Cookie 即可继续后台创建。

## 技术说明

### 邮件读取实现

- `internal/server/receiver_api.go` 实现 MoeMail 收件 API。
- MoeMail 基址默认 `https://moemail.app`，使用实例的 `X-API-Key`。
- 账号级凭据持久化在 `accounts.json`，所有账号查询与配置响应均做脱敏。

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
- **邮件读取**: 依赖账号配置的 MoeMail HTTPS API，超时默认 30 秒
