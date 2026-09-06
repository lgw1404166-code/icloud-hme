# iCloud Hide My Email 本地管理工具

[English](#english) | 中文

通过 Apple HME Web 接口和 MoeMail 公开 API，实现 iCloud 隐藏邮箱别名的创建、列出和邮件收取功能。

## 功能特性

- ✅ **后台别名池** — 服务按固定速率预创建 iCloud 隐藏邮箱地址，调用方只领取本地池中的邮箱
- ✅ **列出所有别名** — 查看账号下的所有 HME 别名
- ✅ **调用方去重** — `chatgpt`、`ChatGPT` 等大小写视为同一身份，同一调用方不会重复拿到已领取过的别名
- ✅ **收取邮件** — 通过账号级 MoeMail 转发收件箱 API 精确读取发到 HME 别名的邮件和正文
- ✅ **账号级收件配置** — 每个 Apple 账号可绑定独立的转发收件箱，不依赖 iCloud Mail 或 App Password
- ✅ **多账号管理** — 支持多个 iCloud 账号并行管理
- ✅ **职责解耦** — Apple Cookie 只用于 HME 别名管理；邮件由转发服务 API 读取

## 快速开始

### 1. 安装

#### 方式一：下载二进制发布版（推荐）

从 [GitHub Releases](https://github.com/xiaozhou26/icloud-hme/releases) 下载对应平台的二进制文件：

| 平台 | 文件 |
|---|---|
| Linux x86_64 | `icloud-hme_linux_amd64` |
| Linux ARM64 | `icloud-hme_linux_arm64` |
| macOS Intel | `icloud-hme_darwin_amd64` |
| macOS Apple Silicon | `icloud-hme_darwin_arm64` |
| Windows x86_64 | `icloud-hme_windows_amd64.exe` |

```bash
# 示例：Linux 下直接运行
chmod +x icloud-hme_linux_amd64
./icloud-hme_linux_amd64
```

#### 方式二：Docker

```bash
# 拉取镜像
docker pull ghcr.io/xiaozhou26/icloud-hme:latest

# 运行（将本机 data 目录挂载进去）
docker run -d \
  --name icloud-hme \
  -p 8081:8081 \
  -v /path/to/data:/app/data \
  ghcr.io/xiaozhou26/icloud-hme:latest
```

镜像支持 `linux/amd64` 和 `linux/arm64` 双架构，自动适配。

#### 方式三：源码编译

```bash
# 前置要求: Go 1.26+
git clone https://github.com/xiaozhou26/icloud-hme.git
cd icloud-hme

# 编译
go build -o icloud-hme.exe .

# 调试模式（启用 Gin 请求日志）
./icloud-hme.exe -debug
```

### 2. 配置账号

在程序 `data/` 目录下创建 `accounts.json`:

```json
{
  "accounts": [
    {
      "id": "acc_1",
      "name": "主号",
      "host": "icloud.com",
      "cookies": {
        "X-APPLE-WEBAUTH-TOKEN": "token_value",
        "X-APPLE-WEBAUTH-USER": "v=1:s=1:d=22789132008",
        "X-APPLE-WEBAUTH-HSA-TRUST": "trust_value",
        "X-APPLE-DS-WEB-SESSION-TOKEN": "session_token"
      },
      "mail_receiver": {
        "address": "relay@example.com",
        "api_key": "your-moemail-api-key",
        "base_url": "https://moemail.app"
      },
      "proxy": "http://user:pass@host:port"
    }
  ]
}
```

> **提示:** 也可以通过管理页面或 API 动态添加账号。创建账号时必须提供浏览器导出的 Cookie；收件配置在账号行的“配置转发收件箱”操作中单独保存。

### 手动导入 Cookie

1. 在 Chrome 或 Edge 中登录 iCloud，并打开 `https://account.apple.com/account/manage/section/privacy` 完成 Apple 账户登录。
2. 使用浏览器的 Cookie 导出工具同时导出 `icloud.com` / `www.icloud.com` 与 `account.apple.com` / `appleid.apple.com` 的 Cookie；优先使用带 `domain` 的浏览器导出数组。
3. 在管理页面点击“添加账号”，粘贴 Cookie JSON 或 `name=value; name2=value2` 格式，提交后服务会立即校验。

Cookie 是 iCloud 的临时会话凭据，不要发送给第三方；失效后可在账号列表使用“更新 Cookie”重新导入。

Apple 账户管理接口会在 `/account/manage/gs/ws/token` 响应中下发滚动会话 Cookie（当前观察到 `caw-at`、`awat` 的 `Max-Age=900`）。服务会同步响应中的 `scnt` 与 CookieJar 新值，因此在持续调用期间会自动滚动；长时间空闲超过 Apple 的账户管理会话窗口后，仍需在账户页重新登录并更新 Cookie。

服务默认每 10 分钟主动刷新一次 Apple 账户管理会话，避免没有业务请求时短 Cookie 到期。通过 `ICLOUD_HME_ACCOUNT_REFRESH_INTERVAL` 调整间隔（例如 `8m`）；设为 `0` 或 `off` 可关闭。保活要求服务持续运行，已经过期或被 Apple 注销的根会话仍需重新登录。

### 后台别名池

外部调用方不再直接触发 Apple 创建请求。服务启动后会按后台节奏预创建别名并保存到本地池，`POST /api/create` 只负责按调用方身份领取一个“该调用方尚未使用过”的邮箱。领取成功会立刻永久保留该调用方标签；业务若在完成注册前明确失败，可调用 `POST /api/aliases/release` 释放自己的标签。

若业务发现某个别名已经完成注册但调用方标签缺失，可调用 `POST /api/aliases/:id/callers` 幂等补写标签；该操作不会释放或删除别名。

可用环境变量：

- `ICLOUD_HME_AUTO_CREATE`：默认开启；设为 `0` / `off` / `false` 关闭后台创建。
- `ICLOUD_HME_AUTO_CREATE_PER_HOUR`：默认 `10`，表示每小时每个账号最多创建 10 个（默认约 6 分钟 1 个）。
- `ICLOUD_HME_AUTO_CREATE_INTERVAL`：直接指定创建间隔，例如 `6m`；设置后优先于 `PER_HOUR`。
- `ICLOUD_HME_AUTO_CREATE_MAX_TOTAL`：单账号本地最多保留多少别名，`0` 表示不设本地上限。
- `ICLOUD_HME_AUTO_CREATE_LABEL` / `ICLOUD_HME_AUTO_CREATE_NOTE`：后台创建时写入 Apple 侧的标签/备注。

当 Apple 根登录态过期时，管理页面会弹窗提示重新登录；点击确定会打开 `https://account.apple.com/account/manage/section/privacy`，登录完成后回到管理页面更新 Cookie。

### 纯协议自动重新登录 + OTP

这组配置合并在一起，默认关闭：

- 直接运行 `build/icloud-hme.exe` 时会自动读取项目根目录 `.env`；系统环境变量优先级更高，不会被 `.env` 覆盖。
- `ICLOUD_HME_RELOGIN_ENABLED`：总开关，关闭时继续使用手动登录弹窗。
- `ICLOUD_HME_RELOGIN_MODE`：默认 `apple_protocol_sms`。
- `ICLOUD_HME_RELOGIN_APPLE_ID` / `ICLOUD_HME_RELOGIN_APPLE_PASSWORD`：纯协议自动登录所需账号信息。
- `ICLOUD_HME_RELOGIN_OTP_PROVIDER`：默认 `smsgate_webhook`。
- `ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN`：SMSGate webhook token。
- `ICLOUD_HME_RELOGIN_OTP_TTL`：验证码缓存时间，默认 5 分钟。
- `ICLOUD_HME_RELOGIN_MANUAL_URL`：手动登录页地址，默认 Apple 隐私邮箱页。
- `ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT`：纯协议自动流程整体超时，默认 3 分钟。

`.env` 示例：

```dotenv
ICLOUD_HME_RELOGIN_ENABLED=false
ICLOUD_HME_RELOGIN_MODE=apple_protocol_sms
ICLOUD_HME_RELOGIN_APPLE_ID=
ICLOUD_HME_RELOGIN_APPLE_PASSWORD=
ICLOUD_HME_RELOGIN_OTP_PROVIDER=smsgate_webhook
ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN=
ICLOUD_HME_RELOGIN_OTP_TTL=5m
ICLOUD_HME_RELOGIN_MANUAL_URL=https://account.apple.com/account/manage/section/privacy
ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT=3m
```

对应接口：

- `POST /api/otp/inbound?token=...`
- `GET /api/otp/latest?token=...&provider=apple`
- `GET /api/relogin/config`

关闭总开关时，页面仍然会弹窗提示你手动重新登录。

管理页左侧新增 **配置** 菜单，会集中展示纯协议自动重新登录和 Android/SMSGate OTP 的当前状态；可直接在页面修改总开关、模式、Apple ID/密码、OTP provider/token/TTL、手动登录页，保存后写入 `.env` 并立即应用到当前服务进程。

### Android SMSGate APK

源码放在 `tools/android-sms-gateway/`，当前已自编译 debug APK：

```text
build/smsgateway-debug.apk
```

SHA256:

```text
AA3C7010191E64C12969D1F87A5C50CA59CA654E2236B88206F1061CBF4971AE
```

重新构建：

```powershell
powershell -ExecutionPolicy Bypass -File tools/build-smsgateway-apk.ps1
```

SMSGate 本地模式注册 webhook 示例：

```bash
curl -X POST "http://<安卓手机局域网IP>:8080/webhooks" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "icloud-hme-apple-otp",
    "url": "http://<本机IP>:18081/api/otp/inbound?token=<ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN>",
    "event": "sms:received"
  }'
```

服务端会二次过滤，只接受 Apple 相关短信中的 6 位验证码，并缓存验证码，不保存完整短信正文。

### 3. 启动服务

```bash
# 二进制方式（默认 data 目录）
./icloud-hme_linux_amd64

# 指定端口和数据目录
./icloud-hme_linux_amd64 -addr :9090 -data ./my_data

# 调试模式（启用请求日志）
./icloud-hme_linux_amd64 -debug

# 查看完整参数
./icloud-hme_linux_amd64 -h
```

服务默认监听 `:8081`。

## API 接口

### 核心接口

#### 获取 HME 别名（从本地池领取）

```bash
POST /api/create

# 请求体
{
  "account_id": "acc_1",      # 条件必填: 账号 ID
  "caller": "chatgpt"         # 必填: 调用方身份，大小写视为同一身份
}

# 响应
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

#### 读取邮件

```bash
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&folder=all&limit=20&days=7

# 参数说明:
#   account_id - 必填: 账号 ID
#   alias      - 必填: 要精确读取的 HME 别名
#   folder     - 可选: all/inbox (默认 all；提供商 API 无垃圾邮件文件夹)
#   limit      - 可选: 返回邮件数量 (默认 20)
#   days       - 可选: 查找最近几天的邮件 (默认 0 表示不限)

# 响应
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "count": 2,
    "method": "moemail_api",
    "messages": [
      {
        "id": "1042",
        "from": "noreply@example.com",
        "to": "xyz123@icloud.com",
        "subject": "欢迎注册",
        "date": "2026-07-09T14:32:10+08:00"
      }
    ]
  }
}

# 收件方式:
#   method: "moemail_api"
```

邮件列表默认只返回信封摘要以加快加载速度。需要正文时再调用：

```bash
GET /api/inbox/message?account_id=acc_1&alias=xyz123@icloud.com&id=1042
```

HTML 邮件会返回 `html` 字段，管理页面通过 sandbox iframe 直接展示网页邮件内容。

### 账号管理接口

#### 列出可用账号

```bash
GET /api/accounts

# 响应
{
  "success": true,
  "data": [
    {"id": "acc_1", "name": "主号"},
    {"id": "acc_2", "name": "副号"}
  ]
}
```

默认只返回已启用账号，停用账号不会作为可用 `account_id` 返回。管理场景可使用 `GET /api/accounts?include_disabled=true` 查看完整账号列表并重新启用账号。

#### 添加账号

```bash
POST /api/accounts

# 请求体
{
  "name": "新账号",
  "cookies": "{\"x-apple-session-token\":\"token_value\"}",  # 必填，JSON 或 Header 格式
  "host": "icloud.com",           # 可选
  "proxy": "http://..."           # 可选
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_3",
    "name": "新账号",
    "status": "active"
  }
}
```

#### 删除账号

```bash
DELETE /api/accounts/:id

# 响应
{
  "success": true,
  "data": {"id": "acc_3"}
}
```

#### 配置转发收件箱

```bash
PUT /api/accounts/:id/mail-receiver

# 请求体
{
  "address": "relay@example.com",
  "api_key": "your-moemail-api-key",
  "base_url": "https://moemail.app"
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_1",
    "mail_receiver": {
      "address": "relay@example.com"
    }
  }
}
```

### 别名管理接口

#### 列出所有别名

```bash
GET /api/aliases?account_id=acc_1

# 响应
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
        "used_by_count": 2
      }
    ]
  }
}
```

#### 停用别名

```bash
POST /api/aliases/:id/deactivate

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

#### 激活别名

```bash
POST /api/aliases/:id/reactivate

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

#### 删除别名

```bash
DELETE /api/aliases/:id

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123"
  }
}
```

## 认证方式

### 方式一: Cookie 认证 (推荐,功能最完整)

Cookie 认证用于后台创建别名和管理别名。

**适用范围:**
- 后台创建/停用/激活/删除 HME 别名 ✅

**获取 Cookie:**

1. 使用浏览器登录 [icloud.com](https://www.icloud.com) 或 [icloud.com.cn](https://www.icloud.com.cn) (国区)，并登录 [Apple 账户](https://account.apple.com/account/manage/section/privacy)
2. 使用可信的 Cookie 导出工具同时导出 `icloud.com` / `www.icloud.com` 与 `account.apple.com` / `appleid.apple.com` 的全部 Cookie，优先使用带 `domain` 的数组格式
3. 导出为 `{"key":"value"}` 对象、`[{"name":"key","value":"value","domain":".apple.com"}]` 数组，或 `name=value; name2=value2` Header 格式

**关键 Cookie (必需):**
- `X-APPLE-WEBAUTH-TOKEN` — 认证 token
- `X-APPLE-WEBAUTH-USER` — 含 dsid (`v=1:s=1:d=22789132008`)
- `X-APPLE-WEBAUTH-HSA-TRUST` — 设备信任 token
- `X-APPLE-DS-WEB-SESSION-TOKEN` — 会话 token

要使用后台别名池，导入的 Cookie 还应包含 Apple 账户会话中的 `myacinfo`、`caw`/`caw-at` 或 `awat`。检测到这些 Cookie 后，后台任务会调用 `appleid.apple.com/account/manage/email/private/add` 与 `/add/complete`；外部获取邮箱接口只从本地池领取。

账户 Cookie 的有效期由 Apple 控制。`caw-at`/`awat` 是短期滚动值，不应单独当作长期凭据；建议保留已登录的账户页浏览器会话，空闲过期后从该页面重新导出 Cookie。

**注意:** 导出的 Cookie 值不要包含多余的引号或转义字符。

### 转发收件箱：MoeMail

先在 Apple 隐私邮箱页面将“转发至”改为对应账号配置的固定 MoeMail 邮箱。随后在账号页面填写该固定邮箱地址、MoeMail 实例地址和 API Key。邮箱 ID 可选；未填时服务会用地址从 `/api/emails` 自动定位。

邮件接口只接受 `alias` 参数，并只返回能在 MoeMail 返回的 `to_address` 或原始收件头中精确匹配该 HME 别名的邮件。不能证明归属的邮件不会返回。

**必须保留原始收件人。** MoeMail 官方 Worker 当前只保存转发目标地址、发件人、主题与正文，不会持久化 Apple 转发前的 HME 别名或原始邮件头。直接使用该默认实现无法从一个固定转发邮箱准确区分多个 HME 别名。本服务检测到这一情况会报错而非误匹配。请使用保留原始 HME 收件人的 MoeMail 实例：在其收件 Worker 写入消息时，把原始 `To` / `X-Original-To` / `Delivered-To` 中的 HME 别名保存到 `message.toAddress`（并由 `/api/emails` 与详情接口返回）。

## 项目架构

```
icloud-hme/
├── main.go                 # 入口: 加载配置、初始化管理器、启动服务
├── accounts.json           # 账号配置文件 (自动生成)
├── go.mod
└── internal/
    ├── account/
    │   └── manager.go      # 多账号管理器 (持久化、客户端工厂)
    ├── hme/
    │   └── client.go       # iCloud HME Web 客户端 (Cookie 认证)
    └── server/
        ├── receiver_api.go # MoeMail 收件 API 客户端
        └── server.go       # HTTP API (Gin 路由 + 请求处理)
```

### 核心模块

- **account.Manager**: 管理多个 iCloud 账号,负责配置持久化和客户端创建
- **hme.Client**: 封装 iCloud HME Web API,支持 Cookie 认证
- **server receiver client**: 通过 MoeMail 公共 API 精确读取转发邮件和正文
- **server.Server**: HTTP API 服务,提供 RESTful 接口

## 技术栈

- **Go 1.26+**
- **Gin** — HTTP 框架
- **tls-client** — TLS 指纹模拟 (绕过 iCloud 反爬)

## 常见问题

### Q: 后台别名池提示需要重新登录?

**A:** Apple 根登录态已过期。管理页面会提示重新登录并打开官方隐私邮箱页；登录完成后重新导出并更新 `icloud.com` 与 `account.apple.com` / `appleid.apple.com` Cookie。

### Q: 调用方怎么避免拿到重复邮箱?

**A:** 调用 `POST /api/create` 时传入 `caller`，例如 `chatgpt`。本项目会立刻记录该调用方已领取过哪些别名；下次同一调用方再领取时会跳过这些邮箱。若业务在完成注册前明确失败，可调用 `POST /api/aliases/release` 并传入 `account_id`、`anonymous_id`、`caller` 来释放自己的标签。

### Q: 读取邮件返回超时?

**A:** 检查账号页面中配置的 MoeMail API 地址、API Key、转发邮箱地址是否一致，并确认 Apple 隐私邮箱页已转发到该地址。

### Q: 如何查看某个别名收到了哪些邮件?

**A:** 调用 `GET /api/inbox?account_id=acc_1&alias=your_alias@icloud.com&folder=all`。该账号必须已在账号页面配置 MoeMail 转发收件箱。

### Q: 支持同时管理多个 iCloud 账号吗?

**A:** 支持，在 `accounts.json` 中配置多个账号即可，每个账号有独立的 `id`。

## 开发指南

### 本地开发

```bash
# 安装依赖
go mod download

# 运行 (开发模式，默认 :8081，带 Gin 请求日志)
go run main.go -debug

# 编译
go build -o icloud-hme.exe .

# 交叉编译
GOOS=linux GOARCH=amd64 go build -o icloud-hme .
GOOS=windows GOARCH=amd64 go build -o icloud-hme.exe .
```

### 发布

推送 `v*` tag 到 GitHub 自动触发 CI：

```bash
git tag v0.2.0 && git push origin --tags
```

Actions 会自动构建多平台二进制、Docker 镜像（`ghcr.io/xiaozhou26/icloud-hme`）并创建 Release。

### 代码规范

- 代码注释使用中文
- 错误信息返回给用户时使用中文
- API 响应格式统一: `{success: bool, data: any, message: string}`

## 许可证

MIT License

---
## 社区

友情链接：[LINUX DO](https://linux.do)

## English

A local management tool for Apple iCloud Hide My Email (HME) aliases. Apple Cookies manage aliases; the MoeMail API reads forwarded messages.

### Features

- Create HME aliases automatically
- List all aliases for an account
- Read forwarded HME messages and bodies through the MoeMail API
- Manage multiple iCloud accounts
- Per-account Cookie and forwarding mailbox provider configuration

### Quick Start

#### Option 1: Binary (GitHub Releases)

Download the latest binary from [GitHub Releases](https://github.com/xiaozhou26/icloud-hme/releases):

| Platform | File |
|---|---|
| Linux x86_64 | `icloud-hme_linux_amd64` |
| Linux ARM64 | `icloud-hme_linux_arm64` |
| macOS Intel | `icloud-hme_darwin_amd64` |
| macOS Apple Silicon | `icloud-hme_darwin_arm64` |
| Windows x86_64 | `icloud-hme_windows_amd64.exe` |

```bash
# Linux example
chmod +x icloud-hme_linux_amd64
./icloud-hme_linux_amd64
```

#### Option 2: Docker

```bash
docker pull ghcr.io/xiaozhou26/icloud-hme:latest

docker run -d \
  --name icloud-hme \
  -p 8081:8081 \
  -v /path/to/data:/app/data \
  ghcr.io/xiaozhou26/icloud-hme:latest
```

#### Option 3: Build from source

```bash
git clone https://github.com/xiaozhou26/icloud-hme.git
cd icloud-hme
go build -o icloud-hme .
./icloud-hme -debug     # enable request logging
```

Create `data/accounts.json` and start the server (default port `:8081`).
