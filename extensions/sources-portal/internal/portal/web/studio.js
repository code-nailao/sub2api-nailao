import { authorizedJSON, clearSession, currentToken } from "./session.js";
import { openCustomSettings, openGroupSettings } from "./studio-settings.js";

const frame = document.querySelector("#studio");
const select = document.querySelector("#groups");
const reconnect = document.querySelector("#reconnect");
const message = document.querySelector("#message");
const loadingPanel = document.querySelector("#loading-panel");
const statusTitle = document.querySelector("#status-title");
const connectionBar = document.querySelector("#connection-bar");
const recoveryActions = document.querySelector("#recovery-actions");
const retry = document.querySelector("#retry");
const login = document.querySelector("#login");
const customButton = document.querySelector("#custom-connection");
const manageButton = document.querySelector("#manage-groups");
const ownKeyButton = document.querySelector("#use-own-key");
const manageRecoveryButton = document.querySelector("#manage-groups-recovery");
const params = new URLSearchParams(location.search);
const validRequestID = (value) => typeof value === "string" && /^[\w-]{16,80}$/.test(value);
const CUSTOM_CONNECTION_NEEDS_UPDATE = "custom_connection_needs_update";
let studioURL, mainOrigin, storageKey, hostConfig;
let groups = [], viewer = { displayName: "", balance: null };
// custom is the user's own-key connection, addressed like a group by a reserved
// ID. setupId and manageId are menu entries: choosing one opens a dialog.
let custom = { groupId: 0, setupId: 0, enabled: false, configured: false }, manageId = 0;
let selectedGroupId = null, authorizationName = "";
let generation = 0, shellRevision = 0, appliedShellRevision = 0;
let shellReady = false, integrated = false, locked = false, selectionUncertain = false;
let connectionPending, shellPending, connectionTimer, shellTimer, checkTimer;
let connectionApplied = false, bootstrapPending = false, initialGroupId = null;
let startupTimer, resaveHint = false;
function showState(state, text, resave = false) {
  resaveHint = resave;
  document.body.dataset.state = state;
  document.body.setAttribute("aria-busy", String(state === "loading"));
  loadingPanel.hidden = state === "ready";
  connectionBar.hidden = state !== "recovery" || locked;
  recoveryActions.hidden = state !== "recovery";
  statusTitle.textContent = state === "loading" ? "正在打开图片工作台" : locked ? "请重新进入工作台" : "工作台连接需要处理";
  retry.hidden = locked;
  retry.disabled = Boolean(connectionPending) || bootstrapPending;
  retry.textContent = resave ? "打开自定义连接" : "重试连接";
  login.hidden = !locked;
  if (text) message.textContent = text;
}
function recoverView(text, resave = false) {
  clearTimeout(startupTimer);
  showState("recovery", text, resave);
  // Only a connection that was actually acknowledged may use an older peer.
  // This is a compatibility recovery, never a shortcut around authentication.
  if (connectionApplied && !locked) frame.hidden = false;
}
function startStartupTimer() {
  clearTimeout(startupTimer);
  startupTimer = setTimeout(() => {
    if (!integrated && !locked) recoverView("连接用时较长。可以重试连接，或稍后从主站重新进入；已有作品与草稿不会被清空。");
  }, 20000);
}
const handledRequests = new Set();

function postStudio(data) {
  if (!locked && studioURL && frame.contentWindow) frame.contentWindow.postMessage(data, studioURL.origin);
}
function notifyHost(ready) {
  if (!hostConfig || window.parent === window) return;
  window.parent.postMessage({ type: "sources:studio-host-config-applied", version: 1,
    requestId: hostConfig.requestId, revision: hostConfig.revision,
    shellMode: hostConfig.shellMode, integrated: ready }, location.origin);
}
function setIntegrated(ready) {
  integrated = ready;
  // A shell acknowledgement does not fix a stale custom connection.
  if (ready && !resaveHint) { clearTimeout(startupTimer); showState("ready"); }
  document.body.dataset.integrated = String(ready);
  renderLegacyGroups();
  notifyHost(ready);
}
function lock(text) {
  if (locked) return;
  locked = true;
  generation++;
  connectionPending = shellPending = null;
  viewer = { displayName: "", balance: null };
  authorizationName = "";
  clearInterval(connectionTimer);
  clearInterval(shellTimer);
  clearInterval(checkTimer);
  clearTimeout(startupTimer);
  setIntegrated(false);
  frame.src = "about:blank";
  frame.hidden = true;
  select.disabled = true;
  recoverView(text);
}
document.querySelector("#lock").onclick = () => {
  clearSession();
  lock("工作台已锁定，请从主站重新打开。");
};
window.addEventListener("sources:locked", () => lock("主站已退出或切换账号，请重新进入。"));

async function api(path, body) {
  const result = await authorizedJSON(path, { method: body ? "POST" : "GET", body });
  if (path === "/api/studio/connection" && result.status === 409 && result.data?.error === CUSTOM_CONNECTION_NEEDS_UPDATE) {
    throw Error(typeof result.data.message === "string" && result.data.message
      ? result.data.message : "密钥所在的分组已经变化，请重新保存自定义连接。", { cause: CUSTOM_CONNECTION_NEEDS_UPDATE });
  }
  if (!result.ok) throw Error(result.status === 401
    ? "登录已失效，请重新登录主站后打开。"
    : result.status === 403 ? "此分组的授权已撤销或过期，请在主站 API 密钥页面核对。"
    : "暂时无法连接此分组，请重试。已有作品和输入内容已保留。");
  return result.data;
}
function normalizeGroups(value) {
  if (!Array.isArray(value)) return [];
  return value.filter((group) => group && Number.isSafeInteger(group.id) && group.id > 0 && typeof group.name === "string")
    .map(({ id, name }) => ({ id, name }));
}
function renderLegacyGroups() {
  select.replaceChildren();
  if (selectedGroupId === null) {
    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = groups.length ? "请选择生图分组" : "暂无可用分组";
    select.append(placeholder);
  }
  for (const group of groups) {
    const option = document.createElement("option");
    option.value = String(group.id);
    option.textContent = group.name;
    select.append(option);
  }
  select.value = selectedGroupId === null ? "" : String(selectedGroupId);
  // A peer that may already have applied the new shell must never receive a
  // metadata-less legacy switch: its strict receiver correctly rejects it.
  select.disabled = Boolean(connectionPending) || !groups.length || locked || shellRevision > 0;
  reconnect.hidden = locked || integrated || shellRevision === 0;
  reconnect.disabled = Boolean(connectionPending) || selectionUncertain || locked;
  retry.disabled = Boolean(connectionPending) || bootstrapPending;
  customButton.hidden = ownKeyButton.hidden = locked || !custom.enabled;
  manageButton.hidden = manageRecoveryButton.hidden = locked || !manageId;
}
function normalizeCustom(value) {
  const id = (candidate) => Number.isSafeInteger(candidate) && candidate > 0 ? candidate : 0;
  const groupId = id(value?.group_id), setupId = id(value?.setup_id);
  const enabled = value?.enabled === true && groupId > 0 && setupId > 0;
  return { groupId, setupId, enabled, configured: enabled && value?.configured === true };
}
// What the client's group menu lists: the groups, then the entries that open settings.
function shellGroups() {
  const entries = [...groups];
  if (custom.enabled) entries.push({ id: custom.setupId, name: custom.configured ? "修改自定义连接…" : "＋ 自定义连接（用我的密钥）…" });
  if (manageId) entries.push({ id: manageId, name: "⚙ 管理工作台分组…" });
  return entries;
}
function openSettings(kind) {
  if (locked) return;
  document.querySelector("#connection-menu").open = false;
  if (kind === "manage") {
    openGroupSettings({ onSaved: () => void refreshMetadata() });
    return;
  }
  openCustomSettings({ onSaved: (configured) => {
    // The client changes group only on its own request, so the page starts
    // again on the saved connection. Drafts and submitted tasks are kept.
    try {
      if (configured) localStorage.setItem(storageKey, String(custom.groupId));
      else if (localStorage.getItem(storageKey) === String(custom.groupId)) localStorage.removeItem(storageKey);
    } catch { /* Storage is optional. */ }
    location.reload();
  } });
}
customButton.onclick = ownKeyButton.onclick = () => openSettings("custom");
manageButton.onclick = manageRecoveryButton.onclick = () => openSettings("manage");

function recoverShell() {
  if (locked || connectionPending || selectionUncertain || shellRevision === 0) return;
  message.textContent = "正在恢复连接，草稿和已提交任务会保留…";
  publishShell();
}
reconnect.onclick = recoverShell;
retry.onclick = () => {
  if (locked || connectionPending || bootstrapPending) return;
  if (resaveHint) { openSettings("custom"); return; }
  if (!studioURL) { void bootstrap(); return; }
  if (selectionUncertain || !connectionApplied) {
    const groupId = selectedGroupId ?? initialGroupId;
    if (groups.some((group) => group.id === groupId)) { void connect(groupId); return; }
  }
  if (shellReady && !selectionUncertain) { recoverShell(); return; }
  recoverView("工作台仍在加载。请稍候，或从主站重新进入；已有作品与草稿会保留。");
};
document.querySelector("#connection-menu").addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  event.currentTarget.open = false;
  event.currentTarget.querySelector("summary")?.focus();
});
function updateMetadata(data) {
  groups = normalizeGroups(data.groups);
  custom = normalizeCustom(data.custom);
  manageId = data.can_manage === true && Number.isSafeInteger(data.manage_id) && data.manage_id > 0 ? data.manage_id : 0;
  if (custom.configured) groups.push({ id: custom.groupId, name: "自定义连接（我的密钥）" });
  viewer = {
    displayName: typeof data.viewer?.displayName === "string" ? data.viewer.displayName.slice(0, 200) : "",
    balance: typeof data.viewer?.balance === "number" && Number.isFinite(data.viewer.balance) ? data.viewer.balance : null,
  };
  if (selectedGroupId !== null && !groups.some((group) => group.id === selectedGroupId)) {
    selectedGroupId = null;
    authorizationName = "";
  }
  renderLegacyGroups();
}
function publishShell() {
  if (!shellReady || !studioURL || !mainOrigin || locked || connectionPending || selectionUncertain) return;
  clearInterval(shellTimer);
  shellPending = {
    type: "sub2api:image-studio-shell-config", version: 1,
    requestId: crypto.randomUUID(), revision: ++shellRevision,
    shellMode: hostConfig?.shellMode || (window.parent === window ? "standalone" : "embedded"),
    groups: shellGroups(), selectedGroupId, viewer, mainOrigin, authorizationName,
    ...(hostConfig?.standaloneUrl ? { standaloneUrl: hostConfig.standaloneUrl } : {}),
    theme: hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light"),
    lang: hostConfig?.lang || params.get("lang") || "zh",
  };
  const pending = shellPending;
  renderLegacyGroups();
  postStudio(pending);
  let attempts = 0;
  shellTimer = setInterval(() => {
    if (shellPending !== pending) return;
    if (++attempts > 30) {
      clearInterval(shellTimer);
      shellPending = null;
      if (!resaveHint) {
        message.textContent = "连接暂未确认，请点击“恢复连接”重试。";
        setIntegrated(false);
        recoverView(message.textContent);
      }
      return;
    }
    postStudio(pending);
  }, 500);
}
// text null sends an empty message: the client then shows no notice at all,
// instead of its default "not confirmed" one.
function selectionResult(pending, status, text = "", reconnectRequired = false) {
  if (!pending?.requestId) return;
  postStudio({ type: "sub2api:image-studio-select-group-result", version: 1,
    requestId: pending.requestId, revision: pending.revision, status,
    ...(text === null ? { message: "" } : text ? { message: text } : {}), reconnectRequired });
}
function sendConnection() {
  const pending = connectionPending;
  if (!pending?.config) return;
  postStudio({ type: "sub2api:image-studio-config", payload: pending.config,
    ...(pending.requestId ? { requestId: pending.requestId, revision: pending.revision } : {}) });
}
async function connect(groupId, request) {
  if (locked || connectionPending || !groups.some((group) => group.id === groupId)) return;
  const pending = connectionPending = { groupId, generation: ++generation,
    requestId: request?.requestId, revision: request?.revision,
    restoreOnly: request?.restoreOnly === true, config: null };
  select.disabled = true;
  message.textContent = "正在连接所选分组…";
  try {
    const config = await api("/api/studio/connection", { group_id: groupId, ...(pending.restoreOnly ? { restore_only: true } : {}) });
    if (locked || connectionPending !== pending) return;
    pending.config = config;
    sendConnection();
    clearInterval(connectionTimer);
    let attempts = 0;
    connectionTimer = setInterval(() => {
      if (connectionPending !== pending) return;
      if (++attempts > 30) {
        clearInterval(connectionTimer);
        connectionPending = null;
        selectionUncertain = true;
        generation++;
        selectionResult(pending, "error", "连接尚未确认，请重新选择分组后再生成。", true);
        if (!integrated) frame.hidden = true;
        message.textContent = "连接尚未确认，请重新选择分组。";
        renderLegacyGroups();
        if (!integrated) recoverView(message.textContent);
        return;
      }
      sendConnection();
    }, 500);
  } catch (error) {
    if (locked || connectionPending !== pending) return;
    connectionPending = null;
    selectionResult(pending, "error", error.message);
    message.textContent = error.message;
    const resave = error.cause === CUSTOM_CONNECTION_NEEDS_UPDATE;
    // A stale custom connection needs its own dialog, not a plain retry.
    if (!integrated || resave) recoverView(error.message, resave);
    if (resave) frame.hidden = true;
    renderLegacyGroups();
    publishShell();
  }
}

window.addEventListener("message", async (event) => {
  if (locked) return;
  const data = event.data;
  if (event.source === window.parent && window.parent !== window && event.origin === location.origin) {
    if (data?.type !== "sources:studio-host-config" || data.version !== 1 || !validRequestID(data.requestId) || !Number.isSafeInteger(data.revision) || data.revision < 1 || (hostConfig && data.revision < hostConfig.revision)) return;
    if (!["embedded", "standalone"].includes(data.shellMode) || !["light", "dark"].includes(data.theme)) return;
    let standalone;
    try { standalone = new URL(data.standaloneUrl); } catch { return; }
    if (standalone.origin !== location.origin || standalone.username || standalone.password || !/^\/custom\/[^/?#]+$/.test(standalone.pathname) || standalone.search !== "?studio_view=standalone" || standalone.hash) return;
    hostConfig = { requestId: data.requestId, revision: data.revision, shellMode: data.shellMode,
      standaloneUrl: standalone.href, theme: data.theme, lang: typeof data.lang === "string" ? data.lang.slice(0, 20) : "zh" };
    document.documentElement.style.colorScheme = data.theme;
    document.documentElement.dataset.theme = data.theme;
    publishShell();
    return;
  }
  if (!studioURL || event.source !== frame.contentWindow || event.origin !== studioURL.origin || !data) return;
  if (data.type === "sub2api:image-studio-lock" && data.version === 1 && data.revision === appliedShellRevision) {
    clearSession();
    lock("工作台已锁定，请从主站重新打开。");
    return;
  }
  if (data.type === "sub2api:image-studio-shell-ready" && data.version === 1) {
    shellReady = true;
    publishShell();
    return;
  }
  if (data.type === "sub2api:image-studio-shell-config-applied" && data.version === 1) {
    const pending = shellPending;
    if (!pending || data.requestId !== pending.requestId || data.revision !== pending.revision) return;
    try { await currentToken(); } catch { return; }
    if (locked || shellPending !== pending) return;
    shellPending = null;
    clearInterval(shellTimer);
    appliedShellRevision = pending.revision;
    // A stale custom connection keeps its hint instead of revealing the page.
    if (!resaveHint) { frame.hidden = false; message.textContent = ""; }
    setIntegrated(true);
    return;
  }
  if (data.type === "sub2api:image-studio-select-group" && data.version === 1) {
    if (!validRequestID(data.requestId) || !Number.isSafeInteger(data.revision) || !Number.isSafeInteger(data.groupId)) return;
    if (handledRequests.has(data.requestId)) return;
    handledRequests.add(data.requestId);
    if (handledRequests.size > 100) handledRequests.delete(handledRequests.values().next().value);
    const settings = custom.enabled && data.groupId === custom.setupId ? "custom" : manageId && data.groupId === manageId ? "manage" : "";
    if (!integrated || shellPending || data.revision !== appliedShellRevision || connectionPending || (!settings && !groups.some((group) => group.id === data.groupId))) {
      selectionResult(data, "error", "分组信息已更新，请重新选择。", selectionUncertain);
      return;
    }
    if (settings) {
      // A menu entry, not a group: nothing is authorized and the group stays.
      // The dialog is the feedback, so the client is told to show no notice.
      selectionResult(data, "error", null, selectionUncertain);
      openSettings(settings);
      return;
    }
    void connect(data.groupId, data);
    return;
  }
  if (data.type !== "sub2api:image-studio-config-applied" || data.payload?.status !== "applied") return;
  const pending = connectionPending;
  if (!pending?.config) return;
  if (!pending.restoreOnly && data.payload.profileId !== `sub2api-image-studio-group-${pending.groupId}`) return;
  if (pending.requestId && (data.payload.requestId !== pending.requestId || data.payload.revision !== pending.revision)) return;
  try { await currentToken(); } catch { return; }
  if (locked || connectionPending !== pending || pending.generation !== generation) return;
  clearInterval(connectionTimer);
  connectionPending = null;
  selectionUncertain = false;
  if (!pending.restoreOnly) {
    selectedGroupId = pending.groupId;
    authorizationName = typeof pending.config.authorizationName === "string" ? pending.config.authorizationName : "";
    try { localStorage.setItem(storageKey, String(selectedGroupId)); } catch { /* Storage is optional. */ }
  }
  connectionApplied = true;
  resaveHint = false;
  // Do not reveal the old intermediate surface between connection and shell ACK.
  if (integrated || document.body.dataset.state === "recovery") frame.hidden = false;
  message.textContent = integrated ? "" : "连接已建立，正在准备工作台…";
  renderLegacyGroups();
  selectionResult(pending, "applied");
  publishShell();
});

frame.addEventListener("load", () => {
  if (locked) return;
  sendConnection();
  publishShell();
});
if (window.parent !== window) window.parent.postMessage({ type: "sources:studio-host-ready", version: 1 }, location.origin);

async function refreshMetadata() {
  if (locked || connectionPending) return;
  try {
    const fresh = await api("/api/studio/groups");
    if (locked) return;
    updateMetadata(fresh);
    publishShell();
  } catch (error) {
    if (locked) return;
    viewer = { displayName: viewer.displayName, balance: null };
    if (!resaveHint) message.textContent = error.message;
    publishShell();
  }
}
async function bootstrap() {
  if (bootstrapPending || locked) return;
  bootstrapPending = true;
  showState("loading", "正在连接工作台，请稍候…");
  startStartupTimer();
  try {
    const data = await api("/api/studio/groups");
    if (!locked) {
      const candidate = new URL(data.studio_url);
      if (data.main_origin !== location.origin || !["https:", "http:"].includes(candidate.protocol) || candidate.username || candidate.password) throw Error("工作台配置暂不可用。");
      studioURL = candidate;
      mainOrigin = data.main_origin;
      storageKey = `sources:studio:last-group:${data.user_id}`;
      studioURL.searchParams.set("sources_account", String(data.user_id));
      studioURL.searchParams.set("ui_mode", "embedded");
      studioURL.searchParams.set("src_host", location.origin);
      studioURL.searchParams.set("theme", hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light"));
      document.querySelector("#keys").href = mainOrigin + "/keys";
      updateMetadata(data);
      let remembered;
      try { remembered = localStorage.getItem(storageKey); } catch { /* Storage is optional. */ }
      const initial = remembered ? groups.find((group) => group.id === Number(remembered)) : groups[0];
      initialGroupId = initial?.id ?? null;
      select.onchange = () => {
        if (shellRevision > 0) { recoverShell(); return; }
        const id = Number(select.value);
        if (id > 0) void connect(id);
      };
      frame.src = studioURL.href;
      if (initial) void connect(initial.id);
      else recoverView(groups.length ? "上次使用的分组已不可用，请重新选择。"
        : custom.enabled ? "当前账号没有已登记的生图分组。可以点“使用我的密钥”，用自己的 API 密钥连接。"
        : "当前账号没有可用的生图分组，请联系管理员。");
      publishShell();
      checkTimer = setInterval(async () => {
        if (document.visibilityState === "visible") await refreshMetadata();
      }, 30000);
    }
  } catch (error) {
    if (!locked) recoverView(error.message);
  } finally {
    bootstrapPending = false;
    renderLegacyGroups();
  }
}
await bootstrap();
