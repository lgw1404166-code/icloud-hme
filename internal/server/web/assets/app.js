(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

  const state = {
    view: "accounts",
    accounts: [],
    aliases: [],
    messages: [],
    inboxError: "",
    selectedAccountId: localStorage.getItem("icloud-hme.selected-account") || "",
    selectedMessageId: "",
    inboxFolder: "all",
    aliasFilter: "all",
    aliasSearch: "",
    aliasesLoadedFor: "",
    modalSubmit: null,
    confirmSubmit: null,
  };

  const viewMeta = {
    accounts: { title: "账号" },
    aliases: { title: "别名" },
    inbox: { title: "收件箱" },
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
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return String(value);
    return new Intl.DateTimeFormat("zh-CN", {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    }).format(parsed);
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

  function filteredAliases() {
    const search = state.aliasSearch.trim().toLowerCase();
    return state.aliases.filter((alias) => {
      const statusMatch = state.aliasFilter === "all"
        || (state.aliasFilter === "active" && alias.active)
        || (state.aliasFilter === "inactive" && !alias.active);
      const searchMatch = !search || `${alias.email || ""} ${alias.label || ""}`.toLowerCase().includes(search);
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
        <td data-label="标签"><span>${escapeHTML(alias.label || "-")}</span></td>
        <td data-label="状态"><span class="status-badge ${alias.active ? "status-active" : "status-inactive"}">${alias.active ? "使用中" : "已停用"}</span></td>
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
        <div class="mail-content">${escapeHTML(message.preview || "无预览内容")}</div>
      </div>`;
    detail.classList.add("is-open");
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

    if (!state.messages.some((message) => message.id === state.selectedMessageId)) {
      state.selectedMessageId = state.messages[0].id;
    }
    list.innerHTML = state.messages.map((message) => `
      <button class="mail-item${message.id === state.selectedMessageId ? " is-selected" : ""}" type="button" data-action="select-message" data-id="${escapeHTML(message.id)}">
        <span class="mail-item-head"><strong>${escapeHTML(message.from || "未知发件人")}</strong><span class="mail-item-meta"><span class="mail-folder-badge${message.folder === "junk" ? " is-junk" : ""}">${escapeHTML(folderLabel(message.folder || "inbox"))}</span><time>${escapeHTML(formatDate(message.date))}</time></span></span>
        <span class="mail-subject">${escapeHTML(message.subject || "无主题")}</span>
        <span class="mail-preview">${escapeHTML(message.preview || "无预览内容")}</span>
      </button>`).join("");
    const selected = state.messages.find((message) => message.id === state.selectedMessageId);
    renderMailDetail(selected);
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
      state.messages = data?.messages || [];
      state.inboxError = "";
      state.selectedMessageId = state.messages[0]?.id || "";
      renderMessages();
      $("#inbox-summary").textContent = `${account.name || account.id} · ${folderLabel(data?.folder || state.inboxFolder)} · ${state.messages.length} 封邮件 · ${data?.method === "imap" ? "IMAP" : "Web API"}`;
    } catch (error) {
      state.messages = [];
      state.inboxError = error.message;
      renderMessages();
    }
  }

  async function refreshCurrentView() {
    setRefreshLoading(true);
    try {
      if (state.view === "accounts") await refreshAccounts({ silent: true });
      if (state.view === "aliases") await refreshAliases({ force: true });
      if (state.view === "inbox") await refreshInbox();
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
      await refreshAliases();
      await refreshInbox();
    }
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
        <label class="field"><span>Cookie</span><textarea name="cookies" required placeholder="JSON、浏览器导出数组或 Cookie Header" spellcheck="false"></textarea><small>请先在浏览器登录 iCloud，再导出 icloud.com 的 Cookie；账号创建时会立即校验。</small></label>`,
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
      body: `<label class="field"><span>Cookie</span><textarea name="cookies" required placeholder="JSON、浏览器导出数组或 Cookie Header" spellcheck="false"></textarea></label>`,
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
      title: "创建邮箱别名",
      submitLabel: "创建别名",
      body: `<label class="field"><span>标签</span><input name="label" maxlength="120" autocomplete="off" placeholder="可选"></label>`,
      onSubmit: async (form) => {
        const label = String(new FormData(form).get("label") || "").trim();
        const result = await request("/api/create", {
          method: "POST",
          body: JSON.stringify({ account_id: account.id, label }),
        });
        closeEditor();
        state.aliasesLoadedFor = "";
        await refreshAliases({ force: true });
        await refreshAccounts({ silent: true });
        toast(`已创建 ${result.email}`);
      },
    });
  }

  async function copyText(value) {
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
    toast("邮箱已复制");
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
    state.inboxError = "";
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
      if (action === "copy-email") copyText(button.dataset.email || "");
      if (action === "toggle-alias") toggleAlias(button);
      if (action === "delete-alias") deleteAlias(button);
      if (action === "close-message") $("#mail-detail").classList.remove("is-open");
      if (action === "open-aliases") {
        selectAccount(button.dataset.id);
        $("#global-account-select").value = button.dataset.id;
        await setView("aliases");
      }
      if (action === "select-message") {
        state.selectedMessageId = button.dataset.id;
        renderMessages();
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
        await refreshAliases({ force: true });
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
      await refreshAccounts();
      void refreshAliases({ force: true });
    } catch {
      // The offline state and toast are handled by refreshAccounts.
    }
  }

  bootstrap();
})();
