import { authorizedJSON, clearSession, currentToken } from "./session.js";
import { openCustomSettings, openGroupSettings } from "./studio-settings.js";
import { connectionRounds } from "./request-round.js";

let frame = document.querySelector("#studio");
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
const CUSTOM_RESAVE_HINT = "密钥所在的分组已变更，请重新保存自定义连接。";
let studioURL, mainOrigin, storageKey, hostConfig;
let groups = [], viewer = { displayName: "", balance: null };
// custom is the user's own-key connection, addressed like a group by a reserved
// ID. setupId and manageId are menu entries: choosing one opens a dialog.
let custom = { groupId: 0, setupId: 0, enabled: false, configured: false }, manageId = 0;
let selectedGroupId = null, authorizationName = "";
let shellRevision = 0, appliedShellRevision = 0;
let shellReady = false, integrated = false, locked = false, selectionUncertain = false;
let connectionPending, shellPending, connectionTimer, shellTimer, checkTimer;
let connectionApplied = false, initialGroupId = null;
let bootstrapRound, compatibilityTimer, expiredLegacyAttempt, needsCustomResave = false;
const rounds = connectionRounds();
// Every staleness decision goes through the token the request started with.
const isCurrent = (started) => !locked && rounds.isCurrent(started);
const isConnecting = () => Boolean(connectionPending) && isCurrent(connectionPending.started);
const isBootstrapping = () => Boolean(bootstrapRound) && isCurrent(bootstrapRound);
const ordinaryGroups = () => groups.filter(({ id }) => id !== custom.groupId);
const connectionTimeout = "连接用时较长。可以重试连接，或稍后从主站重新进入。已有作品与草稿会保留。";

function updateControls() {
  // The native menu and the retry stay usable through every wait, so a stuck
  // request always leaves an action the user can take.
  select.disabled = locked || !groups.length;
  reconnect.hidden = locked || integrated || shellRevision === 0;
  reconnect.disabled = locked || isConnecting() || selectionUncertain;
  retry.hidden = locked;
  retry.disabled = locked;
  retry.textContent = needsCustomResave && !locked ? "打开自定义连接" : "重试连接";
  customButton.hidden = locked || !custom.enabled;
  ownKeyButton.hidden = customButton.hidden || needsCustomResave;
  manageButton.hidden = manageRecoveryButton.hidden = locked || !manageId;
}
function showState(state, text) {
  const resave = needsCustomResave && !locked;
  if (resave) {
    state = "recovery";
    frame.hidden = !connectionApplied;
    text = text && text !== CUSTOM_RESAVE_HINT ? `${CUSTOM_RESAVE_HINT} ${text}` : CUSTOM_RESAVE_HINT;
  }
  document.body.dataset.state = state;
  document.body.setAttribute("aria-busy", String(state === "loading"));
  loadingPanel.hidden = state === "ready";
  connectionBar.hidden = state !== "recovery" || locked;
  recoveryActions.hidden = state !== "recovery";
  statusTitle.textContent = state === "loading" ? "正在打开图片工作台" : locked ? "请重新进入工作台" : "工作台连接需要处理";
  updateControls();
  login.hidden = !locked;
  if (text) message.textContent = text;
}
function recoverView(text) {
  showState("recovery", text);
  // Only a connection that was actually acknowledged may use an older peer.
  // This is a compatibility recovery, never a shortcut around authentication.
  if (connectionApplied && !locked) frame.hidden = false;
}
function waitForCompatibleShell() {
  clearTimeout(compatibilityTimer);
  const started = rounds.beginView();
  const timer = setTimeout(() => {
    if (timer === compatibilityTimer && isCurrent(started) && !integrated) recoverView(connectionTimeout);
  }, 20000);
  compatibilityTimer = timer;
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
function setIntegrated(shellApplied) {
  // A connection that needs a resave is not integrated, whatever the shell says.
  integrated = shellApplied && connectionApplied && !needsCustomResave;
  if (integrated) {
    frame.hidden = false;
    // A shell ACK does not fix a stale connection. Keep its recovery action visible.
    if (!needsCustomResave) { clearTimeout(compatibilityTimer); showState("ready"); }
  } else if (!connectionApplied) frame.hidden = true;
  document.body.dataset.integrated = String(integrated);
  renderLegacyGroups();
  notifyHost(integrated);
}
function lock(text) {
  if (locked) return;
  locked = true;
  rounds.retire();
  connectionPending = shellPending = null;
  expiredLegacyAttempt = undefined;
  bootstrapRound = undefined;
  viewer = { displayName: "", balance: null };
  authorizationName = "";
  clearInterval(connectionTimer);
  clearInterval(shellTimer);
  clearInterval(checkTimer);
  clearTimeout(compatibilityTimer);
  connectionApplied = false;
  setIntegrated(false);
  startDocument();
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

async function api(path, isCurrent, body) {
  const result = await authorizedJSON(path, { method: body ? "POST" : "GET", body, isCurrent });
  if (path === "/api/studio/connection" && result.status === 409 && result.data?.error === CUSTOM_CONNECTION_NEEDS_UPDATE) {
    throw Error(CUSTOM_RESAVE_HINT, { cause: CUSTOM_CONNECTION_NEEDS_UPDATE });
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
  const choices = needsCustomResave ? ordinaryGroups() : groups;
  const selected = choices.some(({ id }) => id === selectedGroupId) ? selectedGroupId : null;
  select.replaceChildren();
  if (selected === null) {
    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = choices.length ? "请选择生图分组" : "暂无可用分组";
    select.append(placeholder);
  }
  for (const group of choices) {
    const option = document.createElement("option");
    option.value = String(group.id);
    option.textContent = group.name;
    select.append(option);
  }
  select.value = selected === null ? "" : String(selected);
  updateControls();
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
  openCustomSettings({ captureConnection: rounds.guardConnection, onSaved: (configured) => {
    if (locked) return;
    rounds.retire();
    connectionPending = shellPending = null;
    bootstrapRound = undefined;
    clearInterval(connectionTimer);
    clearInterval(shellTimer);
    clearTimeout(compatibilityTimer);
    updateControls();
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
  if (locked || isConnecting() || selectionUncertain || shellRevision === 0) return;
  const text = "正在恢复连接，草稿和已提交任务会保留…";
  if (needsCustomResave) recoverView(text);
  else message.textContent = text;
  publishShell();
}
reconnect.onclick = recoverShell;
retry.onclick = () => {
  if (locked) return;
  if (needsCustomResave) { openSettings("custom"); return; }
  if (isConnecting() || isBootstrapping()) return;
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
  if (!shellReady || !studioURL || !mainOrigin || locked || isConnecting() || selectionUncertain) return;
  clearInterval(shellTimer);
  const config = {
    type: "sub2api:image-studio-shell-config", version: 1,
    requestId: crypto.randomUUID(), revision: ++shellRevision,
    shellMode: hostConfig?.shellMode || (window.parent === window ? "standalone" : "embedded"),
    groups: shellGroups(), selectedGroupId, viewer, mainOrigin, authorizationName,
    ...(hostConfig?.standaloneUrl ? { standaloneUrl: hostConfig.standaloneUrl } : {}),
    theme: hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light"),
    lang: hostConfig?.lang || params.get("lang") || "zh",
  };
  const pending = shellPending = { config, started: rounds.beginView() };
  renderLegacyGroups();
  postStudio(config);
  let attempts = 0;
  shellTimer = setInterval(() => {
    if (shellPending !== pending) return;
    if (!isCurrent(pending.started)) {
      clearInterval(shellTimer);
      shellPending = null;
      return;
    }
    if (++attempts > 30) {
      clearInterval(shellTimer);
      shellPending = null;
      setIntegrated(false);
      recoverView("连接暂未确认，请点击“恢复连接”重试。");
      return;
    }
    postStudio(config);
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
function sendConnection(pending) {
  if (!pending?.config || connectionPending !== pending || !isCurrent(pending.started) || !rounds.isWaiting(pending.started)) return;
  pending.target = frame.contentWindow;
  if (!pending.target) return;
  postStudio({ type: "sub2api:image-studio-config", payload: pending.config,
    ...(pending.requestId ? { requestId: pending.requestId, revision: pending.revision } : {}) });
}
function bindFrameLoad(target) {
  target.addEventListener("load", () => {
    if (locked || frame !== target) return;
    sendConnection(connectionPending);
    if (shellPending && isCurrent(shellPending.started)) postStudio(shellPending.config);
    else if (!integrated) publishShell();
  });
}
// The one place a new child document begins: the page navigates or replaces
// the frame, or the shell says it restarted. A bare load is not a document.
function startDocument() {
  rounds.newDocument();
  shellPending = null;
  clearInterval(shellTimer);
  if (connectionPending) {
    if (connectionPending.requestId) {
      // A selection the old child made is not the new child's request.
      clearInterval(connectionTimer);
      connectionPending = undefined;
    } else {
      connectionPending.started = rounds.readdress(connectionPending.started);
    }
  }
  frame.hidden = true;
}
function replaceFrame() {
  const previous = frame;
  frame = previous.cloneNode(false);
  startDocument();
  shellReady = false;
  shellRevision = appliedShellRevision = 0;
  connectionApplied = false;
  if (integrated) setIntegrated(false);
  bindFrameLoad(frame);
  previous.replaceWith(frame);
}
function connectionTimedOut(pending, text) {
  clearInterval(connectionTimer);
  connectionPending = undefined;
  if (pending.config && !pending.requestId) expiredLegacyAttempt = pending;
  selectionUncertain = selectionUncertain || Boolean(pending.target);
  selectionResult(pending, "error", "连接尚未确认，请重新选择分组后再生成。", selectionUncertain);
  if (!integrated || needsCustomResave) recoverView(text);
  else message.textContent = text;
  renderLegacyGroups();
}
async function connect(groupId, request) {
  if (locked || isConnecting() || isBootstrapping() || !groups.some((group) => group.id === groupId)) return;
  // A selection the child made ends with that child's document; the page's own
  // menu outlives it.
  const started = rounds.beginAttempt(request ? "view" : "page");
  const pending = connectionPending = { groupId, started,
    requestId: request?.requestId, revision: request?.revision,
    restoreOnly: request?.restoreOnly === true, config: null };
  clearInterval(shellTimer);
  shellPending = null;
  clearTimeout(compatibilityTimer);
  const expired = expiredLegacyAttempt;
  expiredLegacyAttempt = undefined;
  const replacePeer = !pending.requestId && (expired?.target || (shellRevision > 0 && (needsCustomResave || !connectionApplied)));
  // Retry custom connections through Portal so it checks the Key's current group.
  const cached = !pending.requestId && !needsCustomResave && groupId !== custom.groupId && expired?.groupId === groupId
    ? expired.config : undefined;
  rounds.deadline(pending.started, 20000, () => connectionTimedOut(pending, connectionTimeout));
  const connectingText = "正在连接所选分组…";
  if (needsCustomResave) recoverView(connectingText);
  else message.textContent = connectingText;
  updateControls();
  const waiting = (token) => connectionPending === pending && isCurrent(token);
  const authorizationCurrent = () => waiting(started);
  try {
    let config = cached;
    if (config) await currentToken();
    else config = await api("/api/studio/connection", authorizationCurrent, { group_id: groupId, ...(pending.restoreOnly ? { restore_only: true } : {}) });
    if (!authorizationCurrent()) return;
    // A peer that saw the shell may reject a legacy selection even if its ACK
    // never arrived. A new frame also isolates retries from old legacy ACKs.
    if (replacePeer) replaceFrame();
    pending.started = rounds.readdress(pending.started);
    if (needsCustomResave) {
      needsCustomResave = false;
      recoverView("已收到工作台连接配置，正在等待确认…");
    }
    pending.config = config;
    sendConnection(pending);
    let attempts = 0;
    const timer = setInterval(() => {
      if (!waiting(pending.started) || timer !== connectionTimer) return;
      if (++attempts > 30) {
        rounds.retire();
        connectionTimedOut(pending, "连接尚未确认，请重新选择分组。");
        return;
      }
      sendConnection(pending);
    }, 500);
    connectionTimer = timer;
    updateControls();
  } catch (error) {
    if (!authorizationCurrent()) return;
    rounds.finish(pending.started);
    connectionPending = null;
    selectionResult(pending, "error", error.message);
    if (error.cause === CUSTOM_CONNECTION_NEEDS_UPDATE) needsCustomResave = true;
    if (!integrated || needsCustomResave) recoverView(error.message);
    else message.textContent = error.message;
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
    // A shell that boots is a new child document. It is always processed, and
    // the page answers it instead of the document that came before.
    startDocument();
    shellReady = true;
    publishShell();
    sendConnection(connectionPending);
    return;
  }
  if (data.type === "sub2api:image-studio-shell-config-applied" && data.version === 1) {
    const pending = shellPending;
    if (!pending || !isCurrent(pending.started) || data.requestId !== pending.config.requestId || data.revision !== pending.config.revision) return;
    try { await currentToken(); } catch { return; }
    if (shellPending !== pending || !isCurrent(pending.started)) return;
    shellPending = null;
    clearInterval(shellTimer);
    appliedShellRevision = pending.config.revision;
    if (!connectionApplied) showState("recovery");
    else if (!needsCustomResave) message.textContent = "";
    setIntegrated(true);
    return;
  }
  if (data.type === "sub2api:image-studio-select-group" && data.version === 1) {
    if (!validRequestID(data.requestId) || !Number.isSafeInteger(data.revision) || !Number.isSafeInteger(data.groupId)) return;
    if (handledRequests.has(data.requestId)) return;
    handledRequests.add(data.requestId);
    if (handledRequests.size > 100) handledRequests.delete(handledRequests.values().next().value);
    const settings = custom.enabled && data.groupId === custom.setupId ? "custom" : manageId && data.groupId === manageId ? "manage" : "";
    if (!integrated || shellPending || data.revision !== appliedShellRevision || isConnecting() || (!settings && !groups.some((group) => group.id === data.groupId))) {
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
  if (!pending?.config || !isCurrent(pending.started) || !isConnecting() || pending.target !== event.source) return;
  if (!pending.restoreOnly && data.payload.profileId !== `sub2api-image-studio-group-${pending.groupId}`) return;
  if (pending.requestId && (data.payload.requestId !== pending.requestId || data.payload.revision !== pending.revision)) return;
  try { await currentToken(); } catch { return; }
  if (!isCurrent(pending.started) || connectionPending !== pending || pending.target !== frame.contentWindow || !rounds.finish(pending.started)) return;
  clearInterval(connectionTimer);
  connectionPending = null;
  selectionUncertain = false;
  if (!pending.restoreOnly) {
    selectedGroupId = pending.groupId;
    authorizationName = typeof pending.config.authorizationName === "string" ? pending.config.authorizationName : "";
    try { localStorage.setItem(storageKey, String(selectedGroupId)); } catch { /* Storage is optional. */ }
  }
  connectionApplied = true;
  rounds.accepted();
  if (!integrated) waitForCompatibleShell();
  // Do not reveal the old intermediate surface between connection and shell ACK.
  if (integrated || document.body.dataset.state === "recovery") frame.hidden = false;
  message.textContent = integrated ? "" : "连接已建立，正在准备工作台…";
  renderLegacyGroups();
  selectionResult(pending, "applied");
  publishShell();
});

bindFrameLoad(frame);
if (window.parent !== window) window.parent.postMessage({ type: "sources:studio-host-ready", version: 1 }, location.origin);

async function refreshMetadata() {
  if (locked || isConnecting() || isBootstrapping()) return;
  const started = rounds.beginRead("metadata");
  const current = () => isCurrent(started);
  try {
    const fresh = await api("/api/studio/groups", current);
    if (!current()) return;
    updateMetadata(fresh);
    publishShell();
  } catch (error) {
    if (!current()) return;
    viewer = { displayName: viewer.displayName, balance: null };
    if (needsCustomResave) recoverView(error.message);
    else message.textContent = error.message;
    updateControls();
    publishShell();
  }
}
async function bootstrap() {
  if (locked || isBootstrapping()) return;
  const started = bootstrapRound = rounds.beginAttempt();
  showState("loading", "正在连接工作台，请稍候…");
  rounds.deadline(started, 20000, () => {
    bootstrapRound = undefined;
    recoverView(connectionTimeout);
  });
  const waiting = () => isCurrent(started);
  try {
    const data = await api("/api/studio/groups", waiting);
    if (!waiting()) return;
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
    rounds.finish(started);
    bootstrapRound = undefined;
    updateMetadata(data);
    let remembered;
    try { remembered = localStorage.getItem(storageKey); } catch { /* Storage is optional. */ }
    const initial = remembered ? groups.find((group) => group.id === Number(remembered)) : groups[0];
    initialGroupId = initial?.id ?? null;
    select.onchange = () => {
      const id = Number(select.value);
      if (needsCustomResave && id === custom.groupId) return;
      if (shellRevision > 0 && connectionApplied && !needsCustomResave) { recoverShell(); return; }
      if (id > 0) void connect(id);
    };
    startDocument();
    frame.src = studioURL.href;
    if (initial) void connect(initial.id);
    else recoverView(groups.length ? "上次使用的分组已不可用，请重新选择。"
      : custom.enabled ? "当前账号没有已登记的生图分组。可以点“使用我的密钥”，用自己的 API 密钥连接。"
      : "当前账号没有可用的生图分组，请联系管理员。");
    publishShell();
    clearInterval(checkTimer);
    checkTimer = setInterval(async () => {
      if (document.visibilityState === "visible") await refreshMetadata();
    }, 30000);
  } catch (error) {
    if (!waiting() || !rounds.finish(started)) return;
    bootstrapRound = undefined;
    recoverView(error.message);
  }
}
await bootstrap();
