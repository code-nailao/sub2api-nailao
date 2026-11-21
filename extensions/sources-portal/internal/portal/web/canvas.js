import { authorizedJSON, clearSession, currentToken } from "./session.js";
import { openCustomSettings, openGroupSettings } from "./studio-settings.js";
import { connectionRounds } from "./request-round.js";

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
// The stored connection the resave entry is about. It is page state, so it is
// not tied to the child document that happened to report the conflict.
let resaveConnectionId;
let sendTimer, checkTimer, acknowledgment;
const rounds = connectionRounds();
const granted = new Map();
// The user's own-key connection is addressed like a group, by a reserved ID.
let custom = { groupId: 0, enabled: false, configured: false };
const validRequestID = (value) => typeof value === "string" && /^[\w-]{16,80}$/.test(value);
const connectionKey = (connectionId) => `connection:${connectionId}`;
// Every staleness decision goes through the token the request started with.
// startDocument() ends a verdict from an earlier document; a failure for the
// same connection in this one leaves the 409 guidance in place.
const resaveActive = () => !locked && needsCustomResave;
const working = () => Boolean(attempt) && rounds.isWaiting(attempt.started);
const isWaiting = (operation) => !locked && attempt === operation && rounds.isWaiting(operation.started);
const readingOrAuthorizing = () => working() && ["bootstrap", "authorize"].includes(attempt.phase);
const confirmed = () => Boolean(acknowledgment && acknowledgment.payload === connection?.payload &&
  rounds.isCurrent(acknowledgment.started) && !connection.invalid);

function waitingFor(operation) {
  return () => isWaiting(operation) && rounds.isCurrent(operation.started);
}

function clearResave() {
  needsCustomResave = false;
  resaveConnectionId = undefined;
}
// The one place a new child document begins: the page navigates or replaces
// the frame, or the child says it restarted. A bare load is not a new document.
// The resave entry is the page's own guidance about a stored connection; a
// restarted child does not answer it, so it outlives the document until a save
// or a successful connection supersedes it.
function startDocument() {
  rounds.newDocument();
  acknowledgment = undefined;
  if (connection) connection = { ...connection, started: rounds.readdress(connection.started) };
  frame.hidden = true;
}
function navigateFrame(src) {
  startDocument();
  frame.src = src;
}

function updateControls() {
  // The native menu and the retry stay usable through every wait, so a stuck
  // request always leaves an action the user can take.
  select.disabled = locked || !select.children.length;
  retry.hidden = locked;
  retry.disabled = locked;
  retry.textContent = resaveActive() ? "打开自定义连接" : "重试连接";
  ownKeyButton.hidden = locked || resaveActive() || !custom.enabled;
}
function showState(state, text) {
  const resave = resaveActive();
  if (resave) {
    state = "recovery";
    if (!confirmed()) frame.hidden = true;
    text = text && text !== CUSTOM_RESAVE_HINT ? `${CUSTOM_RESAVE_HINT} ${text}` : CUSTOM_RESAVE_HINT;
  }
  document.body.dataset.state = state;
  document.body.setAttribute("aria-busy", String(state === "loading"));
  statusPanel.hidden = state === "ready";
  controls.hidden = locked;
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
    shellMode: hostConfig.shellMode, integrated: ready && confirmed() && !frame.hidden }, location.origin);
}
function begin(phase, state, text) {
  const previous = connection;
  const keepAcknowledgment = confirmed();
  const operation = { phase, previous };
  operation.started = rounds.beginAttempt();
  // Starting authorization leaves the current configuration in place. Keep
  // its accepted ACK until we send another configuration or the child resets.
  if (keepAcknowledgment) acknowledgment = { payload: previous.payload, started: rounds.captureConnection() };
  attempt = operation;
  clearInterval(sendTimer);
  sendTimer = undefined;
  rounds.deadline(operation.started, 20000, () => {
    fail(operation, "连接用时较长。可重试连接；已有画布和任务会保留。");
  });
  showState(state, text);
  return operation;
}
function finish(operation) {
  if (attempt !== operation) return;
  rounds.finish(operation.started);
  attempt = undefined;
  clearInterval(sendTimer);
  sendTimer = undefined;
}
function fail(operation, text) {
  finish(operation);
  // Reuse the returned grant after an ACK timeout, or the previous grant if
  // fetching or authorizing the new group did not succeed.
  const previous = operation.phase === "confirm" ? connection : operation.previous;
  connection = previous;
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
  if (!confirmed()) return;
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
  rounds.retire();
  attempt = connection = undefined;
  granted.clear();
  clearInterval(sendTimer);
  clearInterval(checkTimer);
  navigateFrame("about:blank");
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
function isConfirming(operation, candidate) {
  return operation && candidate && isWaiting(operation) && operation.phase === "confirm" &&
    rounds.isCurrent(candidate.started) && !candidate.invalid && !resaveActive();
}
function startSending(operation, candidate = connection) {
  if (sendTimer !== undefined || !isConfirming(operation, candidate)) return;
  const send = () => frame.contentWindow?.postMessage({ type: "sub2api:media-studio-config", payload: candidate.payload }, studioURL.origin);
  send();
  let attempts = 0;
  const timer = setInterval(() => {
    if (timer !== sendTimer || !isConfirming(operation, candidate)) return;
    if (++attempts > 30) {
      rounds.retire();
      fail(operation, "画布尚未确认连接。可重试确认；不会重新生成或重复提交任务。");
      return;
    }
    send();
  }, 500);
  sendTimer = timer;
}
function confirm(operation, grant) {
  operation.phase = "confirm";
  clearInterval(sendTimer);
  sendTimer = acknowledgment = undefined;
  const payload = { ...grant,
    connections: [...granted.values()].flatMap((item) => item.connections),
    capabilities: [...granted.values()].flatMap((item) => item.capabilities),
    requestId: crypto.randomUUID() };
  frame.hidden = true;
  const navigating = !frame.getAttribute("src");
  if (navigating) navigateFrame(studioURL.href);
  connection = { grant, payload, started: rounds.captureConnection() };
  if (!navigating) startSending(operation);
  showState("loading", "正在确认画布连接…");
}
function repeatConfirmation() {
  const grant = granted.get(connection.grant.selectedConnectionId) ?? connection.grant;
  const operation = begin("confirm", "loading", "正在确认画布连接…");
  confirm(operation, grant);
}
function rehandshake() {
  const wasConfirmed = confirmed();
  // A child that says it restarted begins a new document on the same
  // WindowProxy. It is always processed, with or without a connection; the
  // previous document's answers and any verdict it got are void.
  startDocument();
  if (wasConfirmed) notifyHost(false);
  if (!connection || resaveActive() || readingOrAuthorizing() ||
      savedGroupToReconnect !== undefined || connection.invalid) {
    updateControls();
    return;
  }
  if (attempt?.phase === "confirm") confirm(attempt, connection.grant);
  else repeatConfirmation();
}
// A load only says there is something to configure. It never decides that a
// document changed; only navigation or canvas-ready does.
frame.addEventListener("load", () => startSending(attempt));

function requireResave(connectionId) {
  needsCustomResave = true;
  resaveConnectionId = connectionId;
  const invalidate = (candidate) => candidate?.grant.selectedConnectionId === connectionId
    ? { ...candidate, invalid: true }
    : candidate;
  if (attempt) attempt.previous = invalidate(attempt.previous);
  // Losing the running connection is a page decision: it needs a resave, not
  // an answer that ends with whatever document happened to report it. The
  // entry stays on the page until a save or a successful connection clears it.
  connection = invalidate(connection);
  granted.delete(connectionId);
}
async function restoreConnection(connectionId, requestId, target) {
  // An answer owed to the document that asked, about one connection: a newer
  // restore or an accepted authorization for it supersedes this one.
  const started = rounds.beginChild(connectionKey(connectionId));
  const current = () => !locked && rounds.isCurrent(started) && target === frame.contentWindow;
  let config;
  try {
    config = await api("/api/canvas/connection", current, { group_id: Number(connectionId.slice(6)), restore_only: true });
  } catch (error) {
    if (!current()) return;
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
  granted.set(connectionId, config);
  target.postMessage({ type: "sub2api:media-studio-config", payload: { ...config, requestId, selectActive: false } }, studioURL.origin);
  // A fresh answer supersedes the verdict about this connection only.
  if (needsCustomResave && resaveConnectionId === connectionId) {
    clearResave();
    if (connection?.invalid && connection.grant.selectedConnectionId === connectionId) {
      repeatConfirmation();
      return;
    }
    if (confirmed() && !working()) showReady();
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
  if (locked || event.source !== frame.contentWindow || event.origin !== studioURL?.origin || !data) return;
  // A child that restarts is a new document even when the page holds no
  // connection yet; it is always processed.
  if (data.type === "sub2api:canvas-ready") {
    rehandshake();
    return;
  }
  if (!connection) return;
  if (data?.type === "sub2api:canvas-restore-connection") {
    const { connectionId, requestId } = data;
    if (typeof connectionId !== "string" || !/^group-[1-9][0-9]*$/.test(connectionId) || typeof requestId !== "string" || !/^[a-f0-9-]{36}$/.test(requestId)) return;
    void restoreConnection(connectionId, requestId, event.source);
    return;
  }
  if (data?.type !== "sub2api:media-studio-config-applied" || data.payload?.status !== "applied") return;
  const pending = attempt;
  const candidate = connection;
  if (!isConfirming(pending, candidate)) return;
  if (data.payload.requestId !== candidate.payload.requestId || data.payload.profileId !== `sub2api-media-studio-${candidate.payload.selectedConnectionId}`) return;
  try { await currentToken(); } catch { return; }
  if (!isConfirming(pending, candidate) || event.source !== frame.contentWindow) return;
  finish(pending);
  rounds.accepted();
  acknowledgment = { payload: candidate.payload, started: rounds.captureConnection() };
  // A selection made while this one was being authorized is not the connection.
  select.value = candidate.payload.selectedConnectionId.slice(6);
  showReady();
});

async function authorize(operation, groupId) {
  savedGroupToReconnect = undefined;
  operation.phase = "authorize";
  if (!needsCustomResave) frame.hidden = true;
  showState("loading", "正在连接所选分组…");
  const waiting = waitingFor(operation);
  try {
    const config = await api("/api/canvas/connection", waiting, { group_id: groupId });
    if (!waiting()) return;
    clearResave();
    granted.set(config.selectedConnectionId, config);
    // A newer authorization answers this connection; an older restore no longer does.
    rounds.renew(connectionKey(config.selectedConnectionId));
    confirm(operation, config);
    watchSession();
  } catch (error) {
    if (!waiting()) return;
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
  checkTimer = setInterval(() => {
    if (!locked && document.visibilityState === "visible") return refreshGroups();
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
// Both background checks and admin refreshes are scoped to the same connection
// and document. Neither owns the connection deadline.
async function refreshGroups(updateMenu = false) {
  const started = rounds.beginRead(updateMenu ? "menu" : "poll");
  const current = () => rounds.isCurrent(started);
  try {
    const data = await api("/api/canvas/groups", current);
    if (!current() || !updateMenu) return;
    const previous = select.value;
    if (!renderGroups(data, previous)) { showState("recovery", noGroups()); return; }
    if (select.value !== previous || !connection) void connect();
  } catch (error) {
    if (current()) showState("recovery", error.message);
  }
}
function reloadGroups() {
  if (locked || readingOrAuthorizing() || !studioURL || savedGroupToReconnect !== undefined) return;
  return refreshGroups(true);
}
async function reconnectSavedGroup(selectId) {
  savedGroupToReconnect = selectId;
  const operation = begin("savedGroups", "recovery", "正在更新画布连接…");
  const waiting = waitingFor(operation);
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
      if (confirmed()) showReady();
      else showState("recovery");
    }
  } catch (error) {
    if (waiting()) fail(operation, error.message);
  }
}
const connectionMenu = document.querySelector("#canvas-connection-menu");
function openCustomConnectionSettings() {
  if (locked) return;
  connectionMenu.open = false;
  openCustomSettings({ captureConnection: rounds.guardConnection, onSaved: (configured) => {
    if (locked) return;
    // A changed or removed connection must not be resent from the earlier grant.
    const customId = `group-${custom.groupId}`;
    rounds.renew(connectionKey(customId));
    granted.delete(customId);
    if (connection?.payload.selectedConnectionId === customId) {
      connection = undefined;
      frame.hidden = true;
    }
    clearResave();
    void reconnectSavedGroup(configured ? String(custom.groupId) : "");
  } });
}
customButton.onclick = ownKeyButton.onclick = openCustomConnectionSettings;
manageButton.onclick = () => {
  if (locked) return;
  connectionMenu.open = false;
  openGroupSettings({ captureConnection: rounds.guardConnection, onSaved: () => void reloadGroups() });
};
async function bootstrap() {
  if (locked || readingOrAuthorizing()) return;
  const operation = begin("bootstrap", "loading", "正在连接画布，请稍候…");
  const waiting = waitingFor(operation);
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
    if (waiting()) fail(operation, error.message);
  }
}
select.onchange = connect;
setShell();
if (window.parent !== window) window.parent.postMessage({ type: "sources:studio-host-ready", version: 1 }, location.origin);
await bootstrap();
