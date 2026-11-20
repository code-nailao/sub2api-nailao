import { authorizedJSON, clearSession, currentToken } from "./session.js";
import { openCustomSettings, openGroupSettings } from "./studio-settings.js";
import { requestRounds } from "./request-round.js";

const params = new URLSearchParams(location.search);
const CUSTOM_CONNECTION_NEEDS_UPDATE = "custom_connection_needs_update";
const CUSTOM_RESAVE_HINT = "密钥所在的分组已变更，请重新保存自定义连接。";
const frame = document.querySelector("#studio");
const select = document.querySelector("#groups");
const message = document.querySelector("#message");
const controls = document.querySelector("#canvas-controls");
const statusPanel = document.querySelector("#canvas-status");
const statusTitle = document.querySelector("#canvas-status-title");
const recovery = document.querySelector("#canvas-recovery");
const retry = document.querySelector("#canvas-retry");
const login = document.querySelector("#canvas-login");
const open = document.querySelector("#canvas-open");
const title = document.querySelector("#canvas-title");
const home = document.querySelector("#canvas-home");
const customButton = document.querySelector("#canvas-custom");
const manageButton = document.querySelector("#canvas-manage");
const ownKeyButton = document.querySelector("#canvas-own-key");
let studioURL, connection, attempt, hostConfig, savedGroupToReconnect;
let locked = false, needsCustomResave = false;
let sendTimer, checkTimer;
let frameGeneration = 0;
const rounds = requestRounds();
const groupRefresh = requestRounds();
const groupPoll = requestRounds();
let nextRestoreRound = 0;
const restoreRounds = new Map();
const granted = new Map();
// The user's own-key connection is addressed like a group, by a reserved ID.
let custom = { groupId: 0, enabled: false, configured: false };
const validRequestID = (value) => typeof value === "string" && /^[\w-]{16,80}$/.test(value);
const isCurrent = (round) => !locked && round === rounds.current;
const isWaiting = (operation) => !locked && attempt === operation && rounds.isWaiting(operation.round);
const readingOrAuthorizing = () => rounds.isWaiting() && ["bootstrap", "authorize"].includes(attempt?.phase);
const confirmed = () => connection?.acknowledgedFrame === frameGeneration && !connection.invalid;

function updateControls() {
  select.disabled = locked || !select.children.length || readingOrAuthorizing();
  retry.hidden = locked;
  retry.disabled = locked || (!needsCustomResave && readingOrAuthorizing());
  retry.textContent = needsCustomResave && !locked ? "打开自定义连接" : "重试连接";
  ownKeyButton.hidden = locked || needsCustomResave || !custom.enabled;
}
function showState(state, text) {
  const resave = needsCustomResave && !locked;
  if (resave) {
    state = "recovery";
    if (!confirmed()) frame.hidden = true;
    text = text && text !== CUSTOM_RESAVE_HINT ? `${CUSTOM_RESAVE_HINT} ${text}` : CUSTOM_RESAVE_HINT;
  }
  document.body.dataset.state = state;
  document.body.setAttribute("aria-busy", String(state === "loading"));
  statusPanel.hidden = state === "ready";
  controls.hidden = state === "loading" || locked;
  recovery.hidden = state !== "recovery";
  statusTitle.textContent = state === "loading" ? "正在打开无限画布" : locked ? "请重新进入画布" : "画布连接需要处理";
  updateControls();
  login.hidden = !locked;
  if (text) message.textContent = text;
}
function notifyHost(ready) {
  if (!hostConfig || window.parent === window) return;
  window.parent.postMessage({ type: "sources:studio-host-config-applied", version: 1,
    requestId: hostConfig.requestId, revision: hostConfig.revision,
    shellMode: hostConfig.shellMode, integrated: ready }, location.origin);
}
function begin(phase, state, text) {
  const previous = connection;
  const operation = { round: rounds.start(), phase, previous };
  attempt = operation;
  clearInterval(sendTimer);
  restoreRounds.clear();
  rounds.deadline(operation.round, 20000, () => {
    fail(operation, "连接用时较长。可以重试连接。已有画布和任务会保留。");
  });
  showState(state, text);
  return operation;
}
function finish(operation) {
  if (attempt !== operation) return;
  rounds.finish(operation.round);
  attempt = undefined;
  clearInterval(sendTimer);
}
function fail(operation, text) {
  finish(operation);
  // Reuse the returned grant after an ACK timeout, or the previous grant if
  // fetching or authorizing the new group did not succeed.
  const previous = operation.phase === "confirm" ? connection : operation.previous;
  connection = previous ? { ...previous, round: rounds.current } : undefined;
  if (connection) select.value = connection.payload.selectedConnectionId.slice(6);
  frame.hidden = !confirmed();
  showState("recovery", text);
  if (!confirmed()) notifyHost(false);
}
function applyAppearance() {
  const theme = hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light");
  document.documentElement.dataset.theme = theme;
  document.documentElement.style.colorScheme = theme;
  if (confirmed() && !locked && studioURL && frame.contentWindow) {
    frame.contentWindow.postMessage({ type: "sources:canvas-appearance", version: 1,
      theme, lang: hostConfig?.lang?.startsWith("en") ? "en-US" : "zh-CN" }, studioURL.origin);
  }
}
function showReady() {
  frame.hidden = false;
  showState("ready");
  applyAppearance();
  notifyHost(true);
}
function setShell() {
  const standalone = hostConfig ? hostConfig.shellMode === "standalone" : window.parent === window;
  document.body.dataset.shellMode = standalone ? "standalone" : "embedded";
  title.hidden = home.hidden = !standalone;
  open.hidden = standalone || !hostConfig;
  if (hostConfig) open.href = hostConfig.standaloneUrl;
  applyAppearance();
}
function lock(text) {
  if (locked) return;
  locked = true;
  rounds.advance();
  groupRefresh.advance();
  groupPoll.advance();
  restoreRounds.clear();
  attempt = connection = undefined;
  granted.clear();
  clearInterval(sendTimer);
  clearInterval(checkTimer);
  frame.src = "about:blank";
  frame.hidden = true;
  select.disabled = true;
  customButton.hidden = manageButton.hidden = ownKeyButton.hidden = true;
  showState("recovery", text);
  notifyHost(false);
}
document.querySelector("#lock").onclick = () => {
  clearSession();
  lock("画布已锁定。请从主站重新打开；你可在 API 密钥页面撤销专用授权。");
};
window.addEventListener("sources:locked", () => lock("主站已退出或切换账号，请重新登录后打开画布。"));
document.querySelector("#canvas-connection-menu").addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  event.currentTarget.open = false;
  event.currentTarget.querySelector("summary")?.focus();
});
async function api(path, isCurrent, body) {
  const result = await authorizedJSON(path, { method: body ? "POST" : "GET", body, isCurrent });
  if (path === "/api/canvas/connection" && result.status === 409 && result.data?.error === CUSTOM_CONNECTION_NEEDS_UPDATE) {
    throw Error(CUSTOM_RESAVE_HINT, { cause: CUSTOM_CONNECTION_NEEDS_UPDATE });
  }
  if (!result.ok) throw Error(result.status === 401
    ? "登录已失效，请重新登录主站后打开画布。"
    : result.status === 403 ? "画布授权已撤销或过期，请在主站 API 密钥页面核对。"
    : "授权暂不可用，请稍后重试；不会自动换组或创建新任务。");
  return result.data;
}
function startSending(operation) {
  if (!isWaiting(operation) || operation.phase !== "confirm" || connection?.round !== operation.round || needsCustomResave) return;
  const payload = connection.payload;
  const send = () => frame.contentWindow?.postMessage({ type: "sub2api:media-studio-config", payload }, studioURL.origin);
  send();
  clearInterval(sendTimer);
  let attempts = 0;
  const timer = setInterval(() => {
    if (!isWaiting(operation) || timer !== sendTimer || needsCustomResave) return;
    if (++attempts > 30) {
      rounds.advance();
      fail(operation, "画布尚未确认连接。可以重试确认。已有任务会保留。");
      return;
    }
    send();
  }, 500);
  sendTimer = timer;
}
function confirm(operation, grant) {
  operation.phase = "confirm";
  const payload = { ...grant,
    connections: [...granted.values()].flatMap((item) => item.connections),
    capabilities: [...granted.values()].flatMap((item) => item.capabilities),
    requestId: crypto.randomUUID() };
  connection = { grant, payload, round: operation.round };
  frame.hidden = true;
  if (!frame.getAttribute("src")) frame.src = studioURL.href;
  else startSending(operation);
  showState("loading", "正在确认画布连接…");
}
function repeatConfirmation() {
  const grant = granted.get(connection.grant.selectedConnectionId) ?? connection.grant;
  const operation = begin("confirm", "loading", "正在确认画布连接…");
  confirm(operation, grant);
}
function reconnectFrame() {
  if (locked || !connection || !frame.getAttribute("src")) return;
  const wasConfirmed = confirmed();
  // Navigation can keep the same WindowProxy. A rollback must not reuse the
  // previous document's ACK, even if it reuses that connection's grant.
  frameGeneration++;
  restoreRounds.clear();
  frame.hidden = true;
  if (wasConfirmed) notifyHost(false);
  if (needsCustomResave || readingOrAuthorizing() || savedGroupToReconnect !== undefined || connection.invalid) return;
  if (attempt?.phase === "confirm") confirm(attempt, connection.grant);
  else repeatConfirmation();
}
frame.addEventListener("load", reconnectFrame);

function requireResave(connectionId) {
  needsCustomResave = true;
  granted.delete(connectionId);
  const invalidate = (candidate) => candidate?.grant.selectedConnectionId === connectionId
    ? { ...candidate, invalid: true }
    : candidate;
  if (attempt) attempt.previous = invalidate(attempt.previous);
  connection = invalidate(connection);
}
async function restoreConnection(connectionId, requestId, target) {
  const pageRound = rounds.current, restoreRound = ++nextRestoreRound;
  const activePayload = connection.payload;
  restoreRounds.set(connectionId, restoreRound);
  const current = () => isCurrent(pageRound) && target === frame.contentWindow &&
    connection?.payload === activePayload && restoreRounds.get(connectionId) === restoreRound;
  let config;
  try {
    config = await api("/api/canvas/connection", current, { group_id: Number(connectionId.slice(6)), restore_only: true });
  } catch (error) {
    if (!current()) return;
    restoreRounds.delete(connectionId);
    if (error.cause === CUSTOM_CONNECTION_NEEDS_UPDATE) {
      requireResave(connectionId);
      if (attempt?.phase === "confirm") finish(attempt);
      showState("recovery", error.message);
      if (!confirmed()) notifyHost(false);
    } else updateControls();
    target.postMessage({ type: "sub2api:canvas-restore-error", connectionId, requestId }, studioURL.origin);
    return;
  }
  if (!current()) return;
  restoreRounds.delete(connectionId);
  granted.set(connectionId, config);
  target.postMessage({ type: "sub2api:media-studio-config", payload: { ...config, requestId, selectActive: false } }, studioURL.origin);
  if (needsCustomResave) {
    needsCustomResave = false;
    if (connection.invalid && connection.grant.selectedConnectionId === connectionId) {
      repeatConfirmation();
      return;
    }
    if (confirmed() && !rounds.isWaiting()) showState("ready");
    else showState("recovery", "画布尚未确认连接，请重试连接。");
  } else updateControls();
}

window.addEventListener("message", async (event) => {
  const data = event.data;
  if (event.source === window.parent && window.parent !== window && event.origin === location.origin && data?.type === "sources:studio-host-config") {
    if (data.version !== 1 || !validRequestID(data.requestId) || !Number.isSafeInteger(data.revision) || data.revision < 1 || data.revision < (hostConfig?.revision || 0)) return;
    if (!["embedded", "standalone"].includes(data.shellMode) || !["light", "dark"].includes(data.theme)) return;
    let standalone;
    try { standalone = new URL(data.standaloneUrl); } catch { return; }
    if (standalone.origin !== location.origin || standalone.username || standalone.password || !/^\/custom\/[^/?#]+$/.test(standalone.pathname) || standalone.search !== "?studio_view=standalone" || standalone.hash) return;
    hostConfig = { requestId: data.requestId, revision: data.revision, shellMode: data.shellMode,
      standaloneUrl: standalone.href, theme: data.theme, lang: typeof data.lang === "string" ? data.lang.slice(0, 20) : "zh" };
    setShell();
    if (confirmed() && !frame.hidden) notifyHost(true);
    return;
  }
  if (locked || event.source !== frame.contentWindow || event.origin !== studioURL?.origin || !connection) return;
  if (data?.type === "sub2api:canvas-ready") {
    reconnectFrame();
    return;
  }
  if (data?.type === "sub2api:canvas-restore-connection") {
    const { connectionId, requestId } = data;
    if (typeof connectionId !== "string" || !/^group-[1-9][0-9]*$/.test(connectionId) || typeof requestId !== "string" || !/^[a-f0-9-]{36}$/.test(requestId)) return;
    void restoreConnection(connectionId, requestId, event.source);
    return;
  }
  if (data?.type !== "sub2api:media-studio-config-applied" || data.payload?.status !== "applied") return;
  const pending = attempt;
  const candidate = connection;
  if (!pending || !isWaiting(pending) || pending.phase !== "confirm" || candidate.round !== pending.round || candidate.invalid || needsCustomResave) return;
  if (data.payload.requestId !== candidate.payload.requestId || data.payload.profileId !== `sub2api-media-studio-${candidate.payload.selectedConnectionId}`) return;
  const acknowledgedFrame = frameGeneration;
  try { await currentToken(); } catch { return; }
  if (!isWaiting(pending) || connection !== candidate || needsCustomResave ||
      event.source !== frame.contentWindow || acknowledgedFrame !== frameGeneration) return;
  finish(pending);
  connection = { ...candidate, acknowledgedFrame };
  rounds.markApplied();
  showReady();
});

async function authorize(operation, groupId) {
  savedGroupToReconnect = undefined;
  operation.phase = "authorize";
  if (!needsCustomResave) frame.hidden = true;
  showState("loading", "正在连接所选分组…");
  const waiting = () => isWaiting(operation);
  try {
    const config = await api("/api/canvas/connection", waiting, { group_id: groupId });
    if (!waiting()) return;
    needsCustomResave = false;
    restoreRounds.clear();
    granted.set(config.selectedConnectionId, config);
    confirm(operation, config);
    watchSession();
  } catch (error) {
    if (!isWaiting(operation)) return;
    if (error.cause === CUSTOM_CONNECTION_NEEDS_UPDATE) requireResave(`group-${groupId}`);
    fail(operation, error.message);
  }
}
function connect() {
  if (locked || readingOrAuthorizing() || !studioURL || !/^[1-9][0-9]*$/.test(select.value)) return;
  return authorize(begin("authorize", "loading", "正在连接所选分组…"), Number(select.value));
}
retry.onclick = () => {
  if (locked) return;
  if (needsCustomResave) { openCustomConnectionSettings(); return; }
  if (readingOrAuthorizing()) return;
  if (savedGroupToReconnect !== undefined) { void reconnectSavedGroup(savedGroupToReconnect); return; }
  if (!studioURL) { void bootstrap(); return; }
  if (confirmed()) { showReady(); return; }
  if (connection && !connection.invalid) { repeatConfirmation(); return; }
  void connect();
};
function watchSession() {
  clearInterval(checkTimer);
  checkTimer = setInterval(async () => {
    if (locked || document.visibilityState !== "visible") return;
    const pageRound = rounds.current, pageGeneration = rounds.generation, poll = groupPoll.advance();
    const current = () => isCurrent(pageRound) && pageGeneration === rounds.generation && poll === groupPoll.current;
    try {
      await api("/api/canvas/groups", current);
    } catch (error) {
      if (current()) showState("recovery", error.message);
    }
  }, 30000);
}
const noGroups = () => custom.enabled
  ? "当前账号没有已登记的生图分组。可以点“使用我的密钥”，用自己的 API 密钥连接。"
  : "当前账号没有可用的生图分组，请联系管理员。";
// Fills the group menu and returns how many entries it has. keep is the value
// to stay on when it is still offered.
function renderGroups(data, keep) {
  const groupId = Number.isSafeInteger(data.custom?.group_id) && data.custom.group_id > 0 ? data.custom.group_id : 0;
  const enabled = data.custom?.enabled === true && groupId > 0;
  custom = { groupId, enabled, configured: enabled && data.custom?.configured === true };
  const entries = data.groups.map(({ id, name }) => ({ id, name }));
  if (custom.configured) entries.push({ id: custom.groupId, name: "自定义连接（我的密钥）" });
  select.replaceChildren();
  for (const group of entries) {
    const option = document.createElement("option");
    option.value = group.id;
    option.textContent = group.name;
    select.append(option);
  }
  if (keep && entries.some((group) => String(group.id) === keep)) select.value = keep;
  customButton.hidden = !custom.enabled;
  manageButton.hidden = data.can_manage !== true;
  updateControls();
  return entries.length;
}
// Admin metadata refreshes do not own the connection deadline.
async function reloadGroups() {
  if (locked || readingOrAuthorizing() || !studioURL || savedGroupToReconnect !== undefined) return;
  const pageRound = rounds.current, refresh = groupRefresh.advance();
  const current = () => isCurrent(pageRound) && refresh === groupRefresh.current;
  try {
    const data = await api("/api/canvas/groups", current);
    if (!current()) return;
    const previous = select.value;
    if (!renderGroups(data, previous)) { showState("recovery", noGroups()); return; }
    if (select.value !== previous || !connection) void connect();
  } catch (error) {
    if (current()) showState("recovery", error.message);
  }
}
async function reconnectSavedGroup(selectId) {
  savedGroupToReconnect = selectId;
  const operation = begin("savedGroups", "recovery", "正在更新画布连接…");
  const waiting = () => isWaiting(operation);
  try {
    const data = await api("/api/canvas/groups", waiting);
    if (!waiting()) return;
    const previous = select.value;
    if (!renderGroups(data, selectId || previous)) {
      savedGroupToReconnect = undefined;
      finish(operation);
      showState("recovery", noGroups());
    } else if (selectId || select.value !== previous || !connection || connection.invalid) {
      await authorize(operation, Number(select.value));
    } else {
      savedGroupToReconnect = undefined;
      finish(operation);
      connection = { ...connection, round: operation.round };
      showState(confirmed() ? "ready" : "recovery");
    }
  } catch (error) {
    if (isWaiting(operation)) fail(operation, error.message);
  }
}
const connectionMenu = document.querySelector("#canvas-connection-menu");
function openCustomConnectionSettings() {
  if (locked) return;
  connectionMenu.open = false;
  openCustomSettings({ getGeneration: () => rounds.generation, onSaved: (configured, started) => {
    if (locked || started !== rounds.generation) return;
    // A changed or removed connection must not be resent from the earlier grant.
    const customId = `group-${custom.groupId}`;
    granted.delete(customId);
    if (connection?.payload.selectedConnectionId === customId) {
      connection = undefined;
      frame.hidden = true;
    }
    needsCustomResave = false;
    void reconnectSavedGroup(configured ? String(custom.groupId) : "");
  } });
}
customButton.onclick = ownKeyButton.onclick = openCustomConnectionSettings;
manageButton.onclick = () => {
  if (locked) return;
  connectionMenu.open = false;
  openGroupSettings({ onSaved: () => void reloadGroups() });
};
async function bootstrap() {
  if (locked || readingOrAuthorizing()) return;
  const operation = begin("bootstrap", "loading", "正在连接画布，请稍候…");
  const waiting = () => isWaiting(operation);
  try {
    const data = await api("/api/canvas/groups", waiting);
    if (!waiting()) return;
    studioURL = new URL(data.canvas_url);
    studioURL.searchParams.set("sources_account", String(data.user_id));
    studioURL.searchParams.set("ui_mode", "embedded");
    studioURL.searchParams.set("src_host", location.origin);
    studioURL.searchParams.set("theme", hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light"));
    document.querySelector("#keys").href = data.main_origin + "/keys";
    if (!renderGroups(data)) {
      finish(operation);
      showState("recovery", noGroups());
      return;
    }
    await authorize(operation, Number(select.value));
  } catch (error) {
    if (isWaiting(operation)) fail(operation, error.message);
  }
}
select.onchange = connect;
setShell();
if (window.parent !== window) window.parent.postMessage({ type: "sources:studio-host-ready", version: 1 }, location.origin);
await bootstrap();
