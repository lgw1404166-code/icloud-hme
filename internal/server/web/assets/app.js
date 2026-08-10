(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

  const state = {
    view: "accounts",
    accounts: [],
    aliases: [],
    messages: [],
    messageDetails: {},
    reloginConfig: { enabled: false, manual_login_url: "https://account.apple.com/account/manage/section/privacy", otp: {} },
    inboxError: "",
    selectedAccountId: localStorage.getItem("icloud-hme.selected-account") || "",
    selectedMessageId: "",
    inboxRequestSeq: 0,
    inboxFolder: "all",
    aliasFilter: "all",
    aliasSearch: "",
    aliasesLoadedFor: "",
    modalSubmit: null,
    confirmSubmit: null,
    loginPrompted: {},
  };

  const viewMeta = {
    accounts: { title: "账号" },
    aliases: { title: "别名" },
    inbox: { title: "收件箱" },
    settings: { title: "配置" },
  };

  function renderIcons(root = document) {
    if (window.lucide) {
      window.lucide.createIcons({
        root,
        attrs: { "aria-hidden": "true" },
      });
    }
  }

  function escapeHTML(value) {
    return String(value ?? "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#039;");
  }

  function formatDate(value, fallback = "-") {
    if (!value) return fallback;
    let parsed;
    if (typeof value === "number" || /^\d+$/.test(String(value))) {
      const numeric = Number(value);
      parsed = new Date(numeric > 1_000_000_000_000 ? numeric : numeric * 1000);
    } else {
      parsed = new Date(value);
    }
    if (Number.isNaN(parsed.getTime())) return String(value);
    return new Intl.DateTimeFormat("zh-CN", {
      timeZone: "Asia/Shanghai",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    }).format(parsed).replaceAll("/", "-") + " 北京时间";
  }

  function folderLabel(folder) {
    if (folder === "junk") return "垃圾邮件";
    if (folder === "inbox") return "收件箱";
    return "全部邮件";
  }

  function accountInitial(account) {
    const source = account.name || account.icloud_email || account.real_email || "IC";
    return [...source.trim()].slice(0, 2).join("").toUpperCase();
  }

  function currentAccount() {
    return state.accounts.find((account) => account.id === state.selectedAccountId) || null;
  }

  async function request(path, options = {}) {
    const config = { ...options, headers: { ...(options.headers || {}) } };
    if (config.body && !(config.body instanceof FormData)) {
      config.headers["Content-Type"] = "application/json";
    }

    const response = await fetch(path, config);
    const raw = await response.text();
    let payload;
    try {
      payload = raw ? JSON.parse(raw) : {};
    } catch {
      payload = { success: false, message: raw || `HTTP ${response.status}` };
    }

    if (!response.ok || payload.success === false) {
      const error = new Error(payload.message || `请求失败 (HTTP ${response.status})`);
      error.status = response.status;
      throw error;
    }
    return payload.data;
  }

  function setServiceStatus(online) {
    const wrapper = $(".service-state");
    const label = $("#service-status");
    wrapper?.classList.toggle("is-offline", !online);
    if (label) label.textContent = online ? "服务在线" : "服务异常";
  }

  function toast(message, type = "success") {
    const item = document.createElement("div");
    item.className = `toast${type === "error" ? " is-error" : ""}`;
    item.innerHTML = `<i data-lucide="${type === "error" ? "circle-alert" : "circle-check"}"></i><span>${escapeHTML(message)}</span>`;
    $("#toast-region").append(item);
    renderIcons(item);
    window.setTimeout(() => item.remove(), 4200);
  }

  function setButtonLoading(button, loading) {
    if (!button) return;
    if (loading) {
      button.dataset.originalHtml = button.innerHTML;
      button.disabled = true;
      button.classList.add("is-loading");
      button.innerHTML = `<i data-lucide="loader-circle"></i><span>处理中</span>`;
      renderIcons(button);
    } else {
      button.disabled = false;
      button.classList.remove("is-loading");
      if (button.dataset.originalHtml) {
        button.innerHTML = button.dataset.originalHtml;
        delete button.dataset.originalHtml;
        renderIcons(button);
      }
    }
  }

  function setRefreshLoading(loading) {
    const button = $("#refresh-view");
    button.disabled = loading;
    button.classList.toggle("is-spinning", loading);
  }

  function renderAccountSelect() {
    const select = $("#global-account-select");
    const previous = state.selectedAccountId;
    if (!state.accounts.length) {
      state.selectedAccountId = "";
      select.innerHTML = `<option value="">暂无账号</option>`;
      select.disabled = true;
      localStorage.removeItem("icloud-hme.selected-account");
      return;
    }

    if (!state.accounts.some((account) => account.id === previous)) {
      state.selectedAccountId = state.accounts[0].id;
    }
    select.disabled = false;
    select.innerHTML = state.accounts
      .map((account) => `<option value="${escapeHTML(account.id)}">${escapeHTML(account.name || account.icloud_email || account.id)}</option>`)
      .join("");
    select.value = state.selectedAccountId;
    localStorage.setItem("icloud-hme.selected-account", state.selectedAccountId);
  }

  function accountStatus(account) {
    const status = String(account.status || "pending").toLowerCase();
    if (status === "active") return { label: "正常", className: "status-active" };
    if (status === "error") return { label: "异常", className: "status-error" };
    return { label: "待认证", className: "status-pending" };
  }

  function renderAccountMetrics() {
    const active = state.accounts.filter((account) => account.status === "active").length;
    const aliases = state.accounts.reduce((sum, account) => sum + Number(account.alias_active || 0), 0);
    $("#metric-accounts").textContent = state.accounts.length;
    $("#metric-active").textContent = active;
    $("#metric-aliases").textContent = aliases;
    $("#accounts-summary").textContent = `${state.accounts.length} 个账号`;
  }

  function renderAccounts() {
    renderAccountMetrics();
    const body = $("#accounts-body");
    const empty = $("#accounts-empty");
    const shell = $("#accounts-table-shell");
    const hasAccounts = state.accounts.length > 0;
    shell.hidden = !hasAccounts;
    empty.hidden = hasAccounts;

    body.innerHTML = state.accounts.map((account) => {
      const status = accountStatus(account);
      const email = account.icloud_email || account.real_email || account.id;
      const errorTitle = account.last_error ? ` title="${escapeHTML(account.last_error)}"` : "";
      return `
        <tr>
          <td data-label="账号">
            <div class="cell-primary">
              <span class="account-avatar">${escapeHTML(accountInitial(account))}</span>
              <span class="cell-copy">
                <strong>${escapeHTML(account.name || "未命名账号")}</strong>
                <small>${escapeHTML(email)}</small>
              </span>
            </div>
          </td>
          <td data-label="状态"><span class="status-badge ${status.className}"${errorTitle}>${status.label}</span></td>
          <td data-label="域"><span class="cell-secondary">${escapeHTML(account.host || "icloud.com")}</span></td>
          <td data-label="别名"><span>${Number(account.alias_active || 0)} / ${Number(account.alias_total || 0)}</span></td>
          <td data-label="最近验证"><span class="cell-secondary">${escapeHTML(formatDate(account.last_validated))}</span></td>
          <td data-label="操作" class="align-right">
            <div class="row-actions">
              <button class="icon-button" type="button" data-action="open-aliases" data-id="${escapeHTML(account.id)}" title="查看别名" aria-label="查看别名"><i data-lucide="at-sign"></i></button>
              <button class="icon-button" type="button" data-action="update-cookies" data-id="${escapeHTML(account.id)}" title="更新 Cookie" aria-label="更新 Cookie"><i data-lucide="cookie"></i></button>
              <button class="icon-button" type="button" data-action="set-password" data-id="${escapeHTML(account.id)}" title="设置 App Password" aria-label="设置 App Password"><i data-lucide="key-round"></i></button>
              <button class="icon-button is-danger" type="button" data-action="delete-account" data-id="${escapeHTML(account.id)}" title="删除账号" aria-label="删除账号"><i data-lucide="trash-2"></i></button>
            </div>
          </td>
        </tr>`;
    }).join("");
    renderIcons(body);
  }

  function renderAccountsLoading() {
    $("#accounts-empty").hidden = true;
    $("#accounts-table-shell").hidden = false;
    $("#accounts-body").innerHTML = `<tr><td colspan="6"><div class="loading-state"><i data-lucide="loader-circle"></i><span>加载中</span></div></td></tr>`;
    renderIcons($("#accounts-body"));
  }

  async function refreshAccounts({ silent = false } = {}) {
    if (!silent) renderAccountsLoading();
    try {
      state.accounts = (await request("/api/accounts")) || [];
      setServiceStatus(true);
      renderAccountSelect();
      renderAccounts();
      renderInboxAliasOptions();
      maybePromptRelogin();
    } catch (error) {
      setServiceStatus(false);
      if (!silent) {
        state.accounts = [];
        renderAccountSelect();
        renderAccounts();
      }
      toast(error.message, "error");
      throw error;
    }
  }

  async function refreshReloginConfig() {
    try {
      state.reloginConfig = await request("/api/relogin/config");
    } catch {
      state.reloginConfig = { enabled: false, manual_login_url: "https://account.apple.com/account/manage/section/privacy", otp: {} };
    }
    renderReloginConfig();
  }

  function formatDurationSeconds(seconds) {
    const value = Number(seconds || 0);
    if (!value) return "-";
    if (value % 60 === 0) return `${value / 60} 分钟`;
    return `${value} 秒`;
  }

  function formatDurationSecondsForInput(seconds, fallback) {
    const value = Number(seconds || 0);
    if (!value) return fallback;
    if (value % 3600 === 0) return `${value / 3600}h`;
    if (value % 60 === 0) return `${value / 60}m`;
    return `${value}s`;
  }

  function configStatusBadge(enabled, yesText = "已配置", noText = "未配置") {
    return `<span class="status-badge ${enabled ? "status-active" : "status-inactive"}">${enabled ? yesText : noText}</span>`;
  }

  function reloginEnvTemplate() {
    return [
      "# 纯协议自动重新登录 + Android/SMSGate 验证码接收",
      "# 总开关关闭时，登录态过期仍走页面弹窗手动重新登录",
      "ICLOUD_HME_RELOGIN_ENABLED=false",
      "ICLOUD_HME_RELOGIN_MODE=apple_protocol_sms",
      "ICLOUD_HME_RELOGIN_APPLE_ID=",
      "ICLOUD_HME_RELOGIN_APPLE_PASSWORD=",
      "ICLOUD_HME_RELOGIN_OTP_PROVIDER=smsgate_webhook",
      "ICLOUD_HME_RELOGIN_OTP_WEBHOOK_TOKEN=",
      "ICLOUD_HME_RELOGIN_OTP_TTL=5m",
      "ICLOUD_HME_RELOGIN_MANUAL_URL=https://account.apple.com/account/manage/section/privacy",
      "ICLOUD_HME_RELOGIN_PROTOCOL_TIMEOUT=3m",
    ].join("\n");
  }

  function configRow(label, value) {
    return `<div class="config-row"><dt>${escapeHTML(label)}</dt><dd>${value}</dd></div>`;
  }

  function renderReloginConfig() {
    const panel = $("#relogin-config-panel");
    if (!panel) return;
    const cfg = state.reloginConfig || {};
    const otp = cfg.otp || {};
    const ready = Boolean(cfg.enabled && cfg.apple_id_configured && cfg.apple_password_configured && otp.webhook_enabled);
    const manualURL = cfg.manual_login_url || "https://account.apple.com/account/manage/section/privacy";
    const webhookURL = `${window.location.origin}${otp.webhook_path || "/api/otp/inbound"}?token=<TOKEN>`;
    const latestURL = `${window.location.origin}${otp.latest_path || "/api/otp/latest"}?token=<TOKEN>&provider=apple`;
    const enabledChecked = cfg.enabled ? "checked" : "";
    const behavior = cfg.enabled
      ? (ready ? "纯协议自动流程已启用，OTP 由 webhook 接收。" : "纯协议自动流程已开启，但还有配置项待补齐。")
      : "总开关关闭，登录态过期时页面会弹窗并打开手动登录页。";

    const summary = $("#settings-summary");
    if (summary) {
      summary.textContent = cfg.enabled
        ? `自动重新登录：${ready ? "配置完整" : "待补齐"}`
        : "自动重新登录关闭，使用手动登录弹窗";
    }

    panel.innerHTML = `
      <article class="config-card">
        <header>
          <span class="metric-icon ${cfg.enabled ? "tone-green" : "tone-amber"}"><i data-lucide="${cfg.enabled ? "shield-check" : "shield-alert"}"></i></span>
          <div>
            <h3>纯协议自动重新登录</h3>
            <p>${escapeHTML(behavior)}</p>
          </div>
        </header>
        <dl class="config-list">
          ${configRow("总开关", configStatusBadge(Boolean(cfg.enabled), "已开启", "已关闭"))}
          ${configRow("模式", `<code>${escapeHTML(cfg.mode || "apple_protocol_sms")}</code>`)}
          ${configRow("Apple ID", configStatusBadge(Boolean(cfg.apple_id_configured)))}
          ${configRow("Apple 密码", configStatusBadge(Boolean(cfg.apple_password_configured)))}
          ${configRow("协议超时", `<span>${escapeHTML(formatDurationSeconds(cfg.protocol_timeout_seconds))}</span>`)}
          ${configRow("手动登录页", `<a href="${escapeHTML(manualURL)}" target="_blank" rel="noopener">${escapeHTML(manualURL)}</a>`)}
        </dl>
        <div class="config-actions">
          <button class="button button-secondary" type="button" data-action="open-manual-login">
            <i data-lucide="external-link"></i><span>打开手动登录页</span>
          </button>
        </div>
      </article>

      <article class="config-card">
        <header>
          <span class="metric-icon ${otp.webhook_enabled ? "tone-green" : "tone-amber"}"><i data-lucide="message-square-lock"></i></span>
          <div>
            <h3>Android/SMSGate OTP</h3>
            <p>只缓存 Apple 短信里的 6 位验证码，不保存完整短信正文。</p>
          </div>
        </header>
        <dl class="config-list">
          ${configRow("Provider", `<code>${escapeHTML(otp.provider || "smsgate_webhook")}</code>`)}
          ${configRow("Webhook Token", configStatusBadge(Boolean(otp.webhook_enabled)))}
          ${configRow("兼容旧 Token", configStatusBadge(Boolean(otp.legacy_token_configured), "已检测", "未使用"))}
          ${configRow("验证码 TTL", `<span>${escapeHTML(formatDurationSeconds(otp.ttl_seconds))}</span>`)}
          ${configRow("接收地址", `<code>${escapeHTML(webhookURL)}</code>`)}
          ${configRow("读取地址", `<code>${escapeHTML(latestURL)}</code>`)}
        </dl>
      </article>

      <article class="config-card config-card-wide">
        <header>
          <span class="metric-icon tone-teal"><i data-lucide="sliders-horizontal"></i></span>
          <div>
            <h3>页面配置</h3>
            <p>保存后会写入项目根目录 .env 并立即应用到当前进程；密码和 token 留空表示保持现有值。</p>
          </div>
        </header>
        <form class="config-form" id="relogin-config-form">
          <label class="field switch-field">
            <span>启用纯协议自动重新登录</span>
            <input name="enabled" type="checkbox" ${enabledChecked}>
          </label>
          <div class="field-row">
            <label class="field"><span>模式</span><input name="mode" value="${escapeHTML(cfg.mode || "apple_protocol_sms")}" autocomplete="off"></label>
            <label class="field"><span>协议超时</span><input name="protocol_timeout" value="${escapeHTML(formatDurationSecondsForInput(cfg.protocol_timeout_seconds, "3m"))}" autocomplete="off" placeholder="3m"></label>
          </div>
          <div class="field-row">
            <label class="field"><span>Apple ID</span><input name="apple_id" autocomplete="username" placeholder="${cfg.apple_id_configured ? "已配置，留空保持现有值" : "name@example.com"}"></label>
            <label class="field"><span>Apple 密码</span><input name="apple_password" type="password" autocomplete="new-password" placeholder="${cfg.apple_password_configured ? "已配置，留空保持现有值" : "输入后保存到 .env"}"></label>
          </div>
          <div class="field-row">
            <label class="field"><span>OTP Provider</span><input name="otp_provider" value="${escapeHTML(otp.provider || "smsgate_webhook")}" autocomplete="off"></label>
            <label class="field"><span>OTP TTL</span><input name="otp_ttl" value="${escapeHTML(formatDurationSecondsForInput(otp.ttl_seconds, "5m"))}" autocomplete="off" placeholder="5m"></label>
          </div>
          <label class="field"><span>OTP Webhook Token</span><input name="otp_webhook_token" type="password" autocomplete="new-password" placeholder="${otp.webhook_enabled ? "已配置，留空保持现有值" : "建议使用随机长 token"}"></label>
          <label class="field"><span>手动登录页</span><input name="manual_login_url" value="${escapeHTML(manualURL)}" autocomplete="off"></label>
          <div class="config-actions">
            <button class="button button-primary" type="submit" id="relogin-config-submit">
              <i data-lucide="save"></i><span>保存到 .env 并应用</span>
            </button>
          </div>
        </form>
      </article>

      <article class="config-card config-card-wide">
        <header>
          <span class="metric-icon tone-teal"><i data-lucide="file-code-2"></i></span>
          <div>
            <h3>.env 配置模板</h3>
            <p>也可以复制模板后手动编辑 .env；服务启动时会读取这一组配置。</p>
          </div>
        </header>
        <pre class="env-block">${escapeHTML(reloginEnvTemplate())}</pre>
      </article>
    `;
    renderIcons(panel);
  }

  function maybePromptRelogin() {
    state.accounts.forEach((item) => {
      if (!item.requires_login) delete state.loginPrompted[item.id];
    });
    const account = currentAccount() || state.accounts.find((item) => item.requires_login);
    if (!account || !account.requires_login || state.loginPrompted[account.id]) return;
    const dialog = $("#confirm-dialog");
    if (dialog?.open) return;
    state.loginPrompted[account.id] = true;
    if (state.reloginConfig?.enabled) {
      const otpReady = Boolean(state.reloginConfig?.otp?.webhook_enabled);
      const credentialsReady = Boolean(state.reloginConfig?.apple_id_configured && state.reloginConfig?.apple_password_configured);
      if (otpReady && credentialsReady) {
        toast(`${account.name || account.id} 登录态过期，已启用纯协议自动重新登录，等待 Apple 短信验证码。`);
      } else {
        toast(`已开启纯协议自动重新登录，但配置未完整：Apple 账号密码=${credentialsReady ? "已配置" : "未配置"}，OTP webhook=${otpReady ? "已配置" : "未配置"}`, "error");
      }
      return;
    }
    openConfirm({
      title: "需要重新登录 Apple",
      message: `${account.name || account.id} 的 Apple 账户登录态已过期。点击确定后会打开官方隐私邮箱页面，登录完成后请回到本页面更新 Cookie。`,
      submitLabel: "打开登录页",
      onSubmit: async () => {
        window.open(state.reloginConfig?.manual_login_url || "https://account.apple.com/account/manage/section/privacy", "icloud_hme_relogin", "popup,width=1120,height=820");
      },
    });
  }

  function filteredAliases() {
    const search = state.aliasSearch.trim().toLowerCase();
    return state.aliases.filter((alias) => {
      const statusMatch = state.aliasFilter === "all"
        || (state.aliasFilter === "active" && alias.active)
        || (state.aliasFilter === "inactive" && !alias.active);
      const searchMatch = !search || `${alias.email || ""} ${(alias.used_by || []).join(" ")}`.toLowerCase().includes(search);
      return statusMatch && searchMatch;
    });
  }

  function renderAliases() {
    const body = $("#aliases-body");
    const shell = $("#aliases-table-shell");
    const empty = $("#aliases-empty");
    const account = currentAccount();
    const aliases = filteredAliases();
    $("#create-alias-button").disabled = !account;
    const activeCount = state.aliases.filter((alias) => alias.active).length;
    $("#aliases-summary").textContent = account
      ? `${account.name || account.id} · ${activeCount} 个有效 / ${state.aliases.length} 个别名`
      : "选择账号后加载";

    shell.hidden = aliases.length === 0;
    empty.hidden = aliases.length > 0;
    $("#aliases-empty-title").textContent = state.aliases.length ? "没有匹配的别名" : "暂无别名";

    body.innerHTML = aliases.map((alias) => `
      <tr>
        <td data-label="邮箱地址">
          <div class="email-cell">
            <code>${escapeHTML(alias.email)}</code>
            <button class="copy-button" type="button" data-action="copy-email" data-email="${escapeHTML(alias.email)}" title="复制邮箱" aria-label="复制邮箱"><i data-lucide="copy"></i></button>
          </div>
        </td>
        <td data-label="状态"><span class="status-badge ${alias.active ? "status-active" : "status-inactive"}">${alias.active ? "使用中" : "已停用"}</span></td>
        <td data-label="调用方使用记录"><span class="cell-secondary">${escapeHTML((alias.used_by || []).join("、") || "尚未领取")}</span></td>
        <td data-label="创建时间"><span class="cell-secondary">${escapeHTML(formatDate(alias.createdAt))}</span></td>
        <td data-label="操作" class="align-right">
          <div class="row-actions">
            <button class="icon-button" type="button" data-action="toggle-alias" data-id="${escapeHTML(alias.anonymousId)}" data-active="${alias.active}" title="${alias.active ? "停用别名" : "激活别名"}" aria-label="${alias.active ? "停用别名" : "激活别名"}"><i data-lucide="${alias.active ? "power-off" : "power"}"></i></button>
            <button class="icon-button is-danger" type="button" data-action="delete-alias" data-id="${escapeHTML(alias.anonymousId)}" data-email="${escapeHTML(alias.email)}" title="删除别名" aria-label="删除别名"><i data-lucide="trash-2"></i></button>
          </div>
        </td>
      </tr>`).join("");
    renderIcons(body);
  }

  function renderAliasesLoading() {
    $("#aliases-empty").hidden = true;
    $("#aliases-table-shell").hidden = false;
    $("#aliases-body").innerHTML = `<tr><td colspan="5"><div class="loading-state"><i data-lucide="loader-circle"></i><span>同步 iCloud 别名</span></div></td></tr>`;
    renderIcons($("#aliases-body"));
  }

  function updateAccountAliasStats(accountId, aliases) {
    const account = findAccount(accountId);
    if (!account) return;
    account.alias_total = aliases.length;
    account.alias_active = aliases.filter((alias) => alias.active).length;
    renderAccountMetrics();
    renderAccounts();
  }

  async function refreshAliases({ force = false } = {}) {
    const account = currentAccount();
    if (!account) {
      state.aliases = [];
      state.aliasesLoadedFor = "";
      renderAliases();
      renderInboxAliasOptions();
      return;
    }
    if (!force && state.aliasesLoadedFor === account.id) {
      renderAliases();
      return;
    }

    renderAliasesLoading();
    try {
      const data = await request(`/api/aliases?account_id=${encodeURIComponent(account.id)}`);
      state.aliases = data?.aliases || [];
      state.aliasesLoadedFor = account.id;
      updateAccountAliasStats(account.id, state.aliases);
      renderAliases();
      renderInboxAliasOptions();
    } catch (error) {
      state.aliases = [];
      state.aliasesLoadedFor = account.id;
      renderAliases();
      renderInboxAliasOptions();
      toast(error.message, "error");
      if (error.status === 401) {
        await refreshAccounts({ silent: true });
      }
    }
  }

  function renderInboxAliasOptions() {
    const select = $("#inbox-alias");
    const previous = select.value;
    select.innerHTML = `<option value="">全部邮件</option>${state.aliases
      .map((alias) => `<option value="${escapeHTML(alias.email)}">${escapeHTML(alias.label ? `${alias.label} · ${alias.email}` : alias.email)}</option>`)
      .join("")}`;
    if ([...select.options].some((option) => option.value === previous)) select.value = previous;
    select.disabled = !currentAccount();
  }

  function renderMailDetail(message) {
    const detail = $("#mail-detail");
    if (!message) {
      detail.classList.remove("is-open");
      detail.innerHTML = `<div class="mail-detail-empty"><span class="empty-icon"><i data-lucide="mail-open"></i></span><h3>选择一封邮件</h3></div>`;
      renderIcons(detail);
      return;
    }

    const htmlBody = message.html ? `
          <iframe class="mail-html-frame" sandbox srcdoc="${escapeHTML(message.html)}"></iframe>` : "";
    const textBody = !message.html ? `<div class="mail-content">${escapeHTML(message.body || message.preview || "无正文内容")}</div>` : "";
    detail.innerHTML = `
      <div class="mail-detail-content">
        <button class="icon-button mail-detail-close" type="button" data-action="close-message" title="返回邮件列表" aria-label="返回邮件列表"><i data-lucide="arrow-left"></i></button>
        <header class="mail-detail-header">
          <h3>${escapeHTML(message.subject || "无主题")}</h3>
          <dl class="mail-meta">
            <dt>发件人</dt><dd>${escapeHTML(message.from || "-")}</dd>
            <dt>收件人</dt><dd>${escapeHTML(message.to || "-")}</dd>
            <dt>时间</dt><dd>${escapeHTML(formatDate(message.date, message.date || "-"))}</dd>
            <dt>邮件夹</dt><dd>${escapeHTML(folderLabel(message.folder || "inbox"))}</dd>
            <dt>ID</dt><dd>${escapeHTML(message.id || "-")}</dd>
          </dl>
        </header>
        ${htmlBody || textBody}
      </div>`;
    detail.classList.add("is-open");
    renderIcons(detail);
  }

  function renderMailDetailLoading() {
    const detail = $("#mail-detail");
    detail.classList.add("is-open");
    detail.innerHTML = `<div class="loading-state"><i data-lucide="loader-circle"></i><span>加载邮件正文</span></div>`;
    renderIcons(detail);
  }

  function renderMessages() {
    const list = $("#mail-list");
    const account = currentAccount();
    $("#inbox-summary").textContent = account
      ? `${account.name || account.id} · ${state.messages.length} 封邮件`
      : "选择账号后查询";

    if (!state.messages.length) {
      if (state.inboxError) {
        list.innerHTML = `<div class="mail-empty-list mail-error-state"><span class="empty-icon"><i data-lucide="circle-alert"></i></span><strong>暂时无法读取邮件</strong><span class="mail-error-copy">${escapeHTML(state.inboxError)}</span></div>`;
      } else {
        list.innerHTML = `<div class="mail-empty-list"><span class="empty-icon"><i data-lucide="mail"></i></span><strong>暂无邮件</strong></div>`;
      }
      renderMailDetail(null);
      renderIcons(list);
      return;
    }

    list.innerHTML = state.messages.map((message) => `
      <button class="mail-item${message.id === state.selectedMessageId ? " is-selected" : ""}" type="button" data-action="select-message" data-id="${escapeHTML(message.id)}">
        <span class="mail-item-head"><strong>${escapeHTML(message.from || "未知发件人")}</strong><span class="mail-item-meta"><span class="mail-folder-badge${message.folder === "junk" ? " is-junk" : ""}">${escapeHTML(folderLabel(message.folder || "inbox"))}</span><time>${escapeHTML(formatDate(message.date))}</time></span></span>
        <span class="mail-subject">${escapeHTML(message.subject || "无主题")}</span>
      </button>`).join("");
    if (!state.selectedMessageId) renderMailDetail(null);
  }

  function renderMessagesLoading() {
    $("#mail-list").innerHTML = `<div class="loading-state"><i data-lucide="loader-circle"></i><span>读取邮件</span></div>`;
    renderMailDetail(null);
    renderIcons($("#mail-list"));
  }

  async function refreshInbox() {
    const account = currentAccount();
    if (!account) {
      state.messages = [];
      state.inboxError = "";
      renderMessages();
      return;
    }
    state.inboxError = "";
    renderMessagesLoading();
    const requestSeq = ++state.inboxRequestSeq;
    const requestAccountId = account.id;
    const params = new URLSearchParams({
      account_id: account.id,
      folder: state.inboxFolder,
      limit: $("#inbox-limit").value,
      days: $("#inbox-days").value,
    });
    const alias = $("#inbox-alias").value;
    if (alias) params.set("alias", alias);
    try {
      const data = await request(`/api/inbox?${params}`);
      if (requestSeq !== state.inboxRequestSeq || state.selectedAccountId !== requestAccountId) return;
      state.messages = data?.messages || [];
      state.inboxError = "";
      state.selectedMessageId = "";
      state.messageDetails = {};
      renderMessages();
      $("#inbox-summary").textContent = `${account.name || account.id} · ${folderLabel(data?.folder || state.inboxFolder)} · ${state.messages.length} 封邮件 · ${data?.method === "imap" ? "IMAP" : "Web API"}`;
    } catch (error) {
      if (requestSeq !== state.inboxRequestSeq || state.selectedAccountId !== requestAccountId) return;
      state.messages = [];
      state.inboxError = error.message;
      renderMessages();
    }
  }

  async function loadMessageDetail(messageId) {
    const account = currentAccount();
    if (!account || !messageId) return;
    state.selectedMessageId = messageId;
    renderMessages();
    if (state.messageDetails[messageId]) {
      renderMailDetail(state.messageDetails[messageId]);
      return;
    }
    renderMailDetailLoading();
    const requestAccountId = account.id;
    const params = new URLSearchParams({ account_id: account.id, id: messageId });
    try {
      const data = await request(`/api/inbox/message?${params}`);
      if (state.selectedAccountId !== requestAccountId || state.selectedMessageId !== messageId) return;
      const message = data?.message || {};
      state.messageDetails[messageId] = message;
      renderMailDetail(message);
    } catch (error) {
      if (state.selectedAccountId !== requestAccountId || state.selectedMessageId !== messageId) return;
      renderMailDetail({
        id: messageId,
        subject: "正文加载失败",
        from: "-",
        to: "-",
        date: "",
        folder: messageId.split(":")[0] || "inbox",
        body: error.message,
      });
    }
  }

  async function refreshCurrentView() {
    setRefreshLoading(true);
    try {
      if (state.view === "accounts") await refreshAccounts({ silent: true });
      if (state.view === "aliases") await refreshAliases({ force: true });
      if (state.view === "inbox") await refreshInbox();
      if (state.view === "settings") await refreshReloginConfig();
    } finally {
      setRefreshLoading(false);
    }
  }

  async function setView(view) {
    if (!viewMeta[view]) return;
    state.view = view;
    $("#page-title").textContent = viewMeta[view].title;
    $$("[data-view-target]").forEach((button) => {
      const active = button.dataset.viewTarget === view;
      button.classList.toggle("is-active", active);
      if (active) button.setAttribute("aria-current", "page");
      else button.removeAttribute("aria-current");
    });
    $$("[data-view]").forEach((section) => {
      const active = section.dataset.view === view;
      section.hidden = !active;
      section.classList.toggle("is-active", active);
    });

    if (view === "aliases") await refreshAliases();
    if (view === "inbox") {
      void refreshAliases();
      await refreshInbox();
    }
    if (view === "settings") await refreshReloginConfig();
  }

  function editorError(message = "") {
    const existing = $(".field-error", $("#modal-body"));
    existing?.remove();
    if (!message) return;
    const error = document.createElement("div");
    error.className = "field-error";
    error.textContent = message;
    $("#modal-body").prepend(error);
  }

  function closeEditor() {
    const dialog = $("#editor-dialog");
    if (dialog.open) dialog.close();
    state.modalSubmit = null;
    editorError();
  }

  function openEditor({ eyebrow, title, submitLabel = "保存", body, onSubmit }) {
    const dialog = $("#editor-dialog");
    $("#modal-eyebrow").textContent = eyebrow;
    $("#modal-title").textContent = title;
    $("#modal-submit").innerHTML = `<span>${escapeHTML(submitLabel)}</span>`;
    $("#modal-body").innerHTML = body;
    state.modalSubmit = onSubmit;
    dialog.showModal();
    renderIcons(dialog);
    window.setTimeout(() => $("input, select, textarea", $("#modal-body"))?.focus(), 30);
  }

  function openConfirm({ title, message, submitLabel = "确认", onSubmit }) {
    const dialog = $("#confirm-dialog");
    $("#confirm-title").textContent = title;
    $("#confirm-message").textContent = message;
    $("#confirm-submit").textContent = submitLabel;
    state.confirmSubmit = onSubmit;
    dialog.showModal();
  }

  function findAccount(id) {
    return state.accounts.find((account) => account.id === id);
  }

  function openAddAccount() {
    openEditor({
      eyebrow: "账号",
      title: "添加账号",
      submitLabel: "创建账号",
      body: `
        <div class="field-row">
          <label class="field"><span>账号名称</span><input name="name" required maxlength="80" autocomplete="off"></label>
          <label class="field"><span>iCloud 区域</span><select name="host"><option value="icloud.com">icloud.com</option><option value="icloud.com.cn">icloud.com.cn</option></select></label>
        </div>
        <label class="field"><span>代理地址</span><input name="proxy" placeholder="http://user:pass@host:port" autocomplete="off"></label>
        <label class="field"><span>Cookie</span><textarea name="cookies" required placeholder="JSON、浏览器导出数组或 Cookie Header" spellcheck="false"></textarea><small>请导入 icloud.com 及 account.apple.com / appleid.apple.com 的登录 Cookie；包含 Apple 账户 Cookie 时，创建别名会使用与官网一致的账户管理接口。</small></label>`,
      onSubmit: async (form) => {
        const data = new FormData(form);
        const cookies = String(data.get("cookies") || "").trim();
        const account = await request("/api/accounts", {
          method: "POST",
          body: JSON.stringify({
            name: String(data.get("name") || "").trim(),
            host: String(data.get("host") || "icloud.com"),
            proxy: String(data.get("proxy") || "").trim(),
            cookies,
          }),
        });
        state.selectedAccountId = account.id;
        closeEditor();
        await refreshAccounts({ silent: true });
        toast(account.status === "active" ? "账号已添加并验证" : "账号已添加，但 Cookie 校验失败");
      },
    });
  }

  function parseCookieInput(raw) {
    const value = raw.trim();
    if (!value) throw new Error("Cookie 不能为空");
    if (value.startsWith("{") || value.startsWith("[")) {
      const parsed = JSON.parse(value);
      if (Array.isArray(parsed)) {
        const mapped = Object.fromEntries(parsed.filter((item) => item?.name).map((item) => [item.name, String(item.value ?? "")]));
        if (Object.keys(mapped).length) return mapped;
      }
      if (parsed && typeof parsed === "object") return parsed;
      throw new Error("Cookie JSON 格式无效");
    }
    const mapped = {};
    value.split(";").forEach((part) => {
      const index = part.indexOf("=");
      if (index > 0) mapped[part.slice(0, index).trim()] = part.slice(index + 1).trim();
    });
    if (!Object.keys(mapped).length) throw new Error("无法解析 Cookie");
    return mapped;
  }

  function openCookieEditor(account) {
    if (!account) return;
    openEditor({
      eyebrow: account.name || "账号",
      title: "更新 Cookie",
      submitLabel: "验证并保存",
      body: `<label class="field"><span>Cookie</span><textarea name="cookies" required placeholder="JSON、浏览器导出数组或 Cookie Header" spellcheck="false"></textarea><small>同时导入 icloud.com 与 account.apple.com / appleid.apple.com Cookie，可启用官网创建协议。</small></label>`,
      onSubmit: async (form) => {
        const cookies = parseCookieInput(String(new FormData(form).get("cookies") || ""));
        await request(`/api/accounts/${encodeURIComponent(account.id)}/cookies`, {
          method: "PUT",
          body: JSON.stringify({ cookies }),
        });
        closeEditor();
        state.aliasesLoadedFor = "";
        await refreshAccounts({ silent: true });
        toast("Cookie 已验证并保存");
      },
    });
  }

  function openPasswordEditor(account) {
    if (!account) return;
    openEditor({
      eyebrow: account.name || "账号",
      title: "设置 App Password",
      submitLabel: "验证并保存",
      body: `
        <label class="field"><span>iCloud 邮箱</span><input name="icloud_email" type="email" value="${escapeHTML(account.icloud_email || "")}" required autocomplete="username"></label>
        <label class="field"><span>App 专用密码</span><input name="app_password" type="password" required autocomplete="new-password" placeholder="xxxx-xxxx-xxxx-xxxx"></label>`,
      onSubmit: async (form) => {
        const data = new FormData(form);
        await request(`/api/accounts/${encodeURIComponent(account.id)}/password`, {
          method: "POST",
          body: JSON.stringify({
            icloud_email: String(data.get("icloud_email") || "").trim(),
            app_password: String(data.get("app_password") || "").trim(),
          }),
        });
        closeEditor();
        await refreshAccounts({ silent: true });
        toast("App Password 已验证并保存");
      },
    });
  }

  function openCreateAlias() {
    const account = currentAccount();
    if (!account) return toast("请先选择账号", "error");
    openEditor({
      eyebrow: account.name || "账号",
      title: "领取池中别名",
      submitLabel: "领取别名",
      body: `
        <label class="field"><span>调用方身份</span><input name="caller" maxlength="80" autocomplete="off" placeholder="例如 chatgpt、moxt" required></label>
        <p class="field-help">这里从后台定时创建的本地别名池领取邮箱，不触发 Apple 创建请求；同一调用方不会重复拿到已领取过的邮箱。</p>`,
      onSubmit: async (form) => {
        const data = new FormData(form);
        const caller = String(data.get("caller") || "").trim();
        const result = await request("/api/create", {
          method: "POST",
          body: JSON.stringify({ account_id: account.id, caller }),
        });
        closeEditor();
        state.aliasesLoadedFor = "";
        await refreshAliases({ force: true });
        await refreshAccounts({ silent: true });
        toast(`已领取 ${result.email}`);
      },
    });
  }

  async function copyText(value, message = "已复制") {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      const textarea = document.createElement("textarea");
      textarea.value = value;
      textarea.style.position = "fixed";
      textarea.style.opacity = "0";
      document.body.append(textarea);
      textarea.select();
      document.execCommand("copy");
      textarea.remove();
    }
    toast(message);
  }

  async function saveReloginConfig(form) {
    const data = new FormData(form);
    const otp = {
      provider: String(data.get("otp_provider") || "smsgate_webhook").trim(),
      ttl: String(data.get("otp_ttl") || "5m").trim(),
    };
    const otpToken = String(data.get("otp_webhook_token") || "").trim();
    if (otpToken) otp.webhook_token = otpToken;

    const body = {
      enabled: data.get("enabled") === "on",
      mode: String(data.get("mode") || "apple_protocol_sms").trim(),
      manual_login_url: String(data.get("manual_login_url") || "").trim(),
      protocol_timeout: String(data.get("protocol_timeout") || "3m").trim(),
      otp,
    };
    const appleID = String(data.get("apple_id") || "").trim();
    const applePassword = String(data.get("apple_password") || "").trim();
    if (appleID) body.apple_id = appleID;
    if (applePassword) body.apple_password = applePassword;

    const submit = $("#relogin-config-submit", form);
    setButtonLoading(submit, true);
    try {
      state.reloginConfig = await request("/api/relogin/config", {
        method: "PUT",
        body: JSON.stringify(body),
      });
      renderReloginConfig();
      toast("重新登录配置已保存并应用");
    } finally {
      setButtonLoading(submit, false);
    }
  }

  async function toggleAlias(button) {
    const account = currentAccount();
    if (!account) return;
    const active = button.dataset.active === "true";
    setButtonLoading(button, true);
    try {
      await request(`/api/aliases/${encodeURIComponent(button.dataset.id)}/${active ? "deactivate" : "reactivate"}`, {
        method: "POST",
        body: JSON.stringify({ account_id: account.id }),
      });
      state.aliasesLoadedFor = "";
      await refreshAliases({ force: true });
      toast(active ? "别名已停用" : "别名已激活");
    } catch (error) {
      toast(error.message, "error");
    } finally {
      setButtonLoading(button, false);
    }
  }

  function deleteAlias(button) {
    const account = currentAccount();
    if (!account) return;
    openConfirm({
      title: "删除邮箱别名",
      message: `删除 ${button.dataset.email || "该别名"} 后无法恢复。`,
      submitLabel: "删除别名",
      onSubmit: async () => {
        await request(`/api/aliases/${encodeURIComponent(button.dataset.id)}`, {
          method: "DELETE",
          body: JSON.stringify({ account_id: account.id }),
        });
        state.aliasesLoadedFor = "";
        await refreshAliases({ force: true });
        await refreshAccounts({ silent: true });
        toast("别名已删除");
      },
    });
  }

  function deleteAccount(account) {
    if (!account) return;
    openConfirm({
      title: "删除账号",
      message: `删除 ${account.name || account.id} 后，本机保存的该账号认证信息将被移除。`,
      submitLabel: "删除账号",
      onSubmit: async () => {
        await request(`/api/accounts/${encodeURIComponent(account.id)}`, { method: "DELETE" });
        state.aliases = [];
        state.messages = [];
        state.messageDetails = {};
        state.inboxError = "";
        state.aliasesLoadedFor = "";
        await refreshAccounts({ silent: true });
        renderAliases();
        renderMessages();
        toast("账号已删除");
      },
    });
  }

  function selectAccount(id) {
    if (id === state.selectedAccountId) return;
    state.selectedAccountId = id;
    localStorage.setItem("icloud-hme.selected-account", id);
    state.aliases = [];
    state.messages = [];
    state.messageDetails = {};
    state.inboxError = "";
    state.inboxRequestSeq++;
    state.aliasesLoadedFor = "";
    state.selectedMessageId = "";
    renderInboxAliasOptions();
  }

  function bindEvents() {
    document.addEventListener("click", async (event) => {
      const viewButton = event.target.closest("[data-view-target]");
      if (viewButton) return setView(viewButton.dataset.viewTarget);

      if (event.target.closest("[data-dialog-close]")) return closeEditor();
      if (event.target.closest("[data-confirm-cancel]")) {
        $("#confirm-dialog").close();
        state.confirmSubmit = null;
        return;
      }

      const inboxFolder = event.target.closest("[data-inbox-folder]");
      if (inboxFolder) {
        state.inboxFolder = inboxFolder.dataset.inboxFolder;
        $$('[data-inbox-folder]').forEach((button) => button.classList.toggle("is-selected", button === inboxFolder));
        if (state.view === "inbox") await refreshInbox();
        return;
      }

      const filter = event.target.closest("[data-alias-filter]");
      if (filter) {
        state.aliasFilter = filter.dataset.aliasFilter;
        $$("[data-alias-filter]").forEach((button) => button.classList.toggle("is-selected", button === filter));
        renderAliases();
        return;
      }

      const button = event.target.closest("[data-action]");
      if (!button) return;
      const action = button.dataset.action;
      const account = findAccount(button.dataset.id);
      if (action === "add-account") openAddAccount();
      if (action === "update-cookies") openCookieEditor(account);
      if (action === "set-password") openPasswordEditor(account);
      if (action === "delete-account") deleteAccount(account);
      if (action === "create-alias") openCreateAlias();
      if (action === "copy-email") copyText(button.dataset.email || "", "邮箱已复制");
      if (action === "toggle-alias") toggleAlias(button);
      if (action === "delete-alias") deleteAlias(button);
      if (action === "close-message") $("#mail-detail").classList.remove("is-open");
      if (action === "open-aliases") {
        selectAccount(button.dataset.id);
        $("#global-account-select").value = button.dataset.id;
        await setView("aliases");
      }
      if (action === "select-message") {
        await loadMessageDetail(button.dataset.id);
      }
      if (action === "open-manual-login") {
        window.open(state.reloginConfig?.manual_login_url || "https://account.apple.com/account/manage/section/privacy", "icloud_hme_relogin", "popup,width=1120,height=820");
      }
      if (action === "refresh-relogin-config") {
        setButtonLoading(button, true);
        try {
          await refreshReloginConfig();
          toast("重新登录配置已刷新");
        } catch (error) {
          toast(error.message, "error");
        } finally {
          setButtonLoading(button, false);
        }
      }
      if (action === "copy-relogin-env") {
        await copyText(reloginEnvTemplate(), ".env 模板已复制");
      }
      if (action === "reload-config") {
        setButtonLoading(button, true);
        try {
          await request("/api/reload", { method: "POST" });
          state.aliasesLoadedFor = "";
          await refreshAccounts({ silent: true });
          toast("配置已重载");
        } catch (error) {
          toast(error.message, "error");
        } finally {
          setButtonLoading(button, false);
        }
      }
    });

    $("#global-account-select").addEventListener("change", async (event) => {
      selectAccount(event.target.value);
      if (state.view === "aliases") await refreshAliases({ force: true });
      if (state.view === "inbox") {
        renderMessages();
        void refreshAliases({ force: true });
        await refreshInbox();
      }
    });

    $("#alias-search").addEventListener("input", (event) => {
      state.aliasSearch = event.target.value;
      renderAliases();
    });

    $("#refresh-view").addEventListener("click", refreshCurrentView);

    $("#inbox-filter-form").addEventListener("submit", async (event) => {
      event.preventDefault();
      await refreshInbox();
    });

    document.addEventListener("submit", async (event) => {
      if (event.target?.id !== "relogin-config-form") return;
      event.preventDefault();
      try {
        await saveReloginConfig(event.target);
      } catch (error) {
        toast(error.message, "error");
      }
    });

    $("#editor-form").addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!state.modalSubmit) return;
      const submit = $("#modal-submit");
      editorError();
      setButtonLoading(submit, true);
      try {
        await state.modalSubmit(event.currentTarget);
      } catch (error) {
        editorError(error.message);
      } finally {
        setButtonLoading(submit, false);
      }
    });

    $("#confirm-form").addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!state.confirmSubmit) return;
      const submit = $("#confirm-submit");
      setButtonLoading(submit, true);
      try {
        await state.confirmSubmit();
        $("#confirm-dialog").close();
        state.confirmSubmit = null;
      } catch (error) {
        toast(error.message, "error");
      } finally {
        setButtonLoading(submit, false);
      }
    });

    $$("dialog").forEach((dialog) => {
      dialog.addEventListener("click", (event) => {
        if (event.target !== dialog) return;
        const rect = dialog.getBoundingClientRect();
        const inside = event.clientX >= rect.left && event.clientX <= rect.right
          && event.clientY >= rect.top && event.clientY <= rect.bottom;
        if (!inside) dialog.close();
      });
    });
  }

  async function bootstrap() {
    renderIcons();
    bindEvents();
    renderMessages();
    try {
      await refreshReloginConfig();
      await refreshAccounts();
      void refreshAliases({ force: true });
      window.setInterval(() => refreshAccounts({ silent: true }).catch(() => {}), 60_000);
    } catch {
      // The offline state and toast are handled by refreshAccounts.
    }
  }

  bootstrap();
})();
