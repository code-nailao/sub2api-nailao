import { authorizedJSON, clearSession, currentToken } from "./session.js";
import { openCustomSettings, openGroupSettings } from "./studio-settings.js";

const params = new URLSearchParams(location.search);
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
let studioURL, configuration, hostConfig;
let ack = false, generation = 0, locked = false, connecting = false, bootstrapping = false, resaveHint = false, resaveClearRequestId = null;
let sendTimer, checkTimer, startupTimer;
const granted = new Map();
// The user's own-key connection is addressed like a group, by a reserved ID.
let custom = { groupId: 0, enabled: false, configured: false };
const validRequestID = (value) => typeof value === "string" && /^[\w-]{16,80}$/.test(value);
const CUSTOM_CONNECTION_NEEDS_UPDATE = "custom_connection_needs_update";

function showState(state, text) {
  document.body.dataset.state = state;
  document.body.setAttribute("aria-busy", String(state === "loading"));
  statusPanel.hidden = state === "ready";
  controls.hidden = state === "loading" || locked;
  recovery.hidden = state !== "recovery";
  statusTitle.textContent = state === "loading" ? "正在打开无限画布" : locked ? "请重新进入画布" : "画布连接需要处理";
  retry.hidden = locked;
  retry.disabled = connecting || bootstrapping;
  // The re-save entry stays until a new configuration is acknowledged or the
  // user handles the connection, even if the next attempt fails differently.
  retry.textContent = resaveHint ? "打开自定义连接" : "重试连接";
  login.hidden = !locked;
  if (text) message.textContent = text;
}
function notifyHost(ready) {
  if (!hostConfig || window.parent === window) return;
  window.parent.postMessage({ type: "sources:studio-host-config-applied", version: 1,
    requestId: hostConfig.requestId, revision: hostConfig.revision,
    shellMode: hostConfig.shellMode, integrated: ready }, location.origin);
}
function recover(text) {
  clearTimeout(startupTimer);
  showState("recovery", text);
}
function armStartupTimeout() {
  clearTimeout(startupTimer);
  startupTimer = setTimeout(() => {
    if (!ack && !locked) recover("连接用时较长。可重试连接；已有画布和任务会保留。");
  }, 20000);
}
function applyAppearance() {
  const theme = hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light");
  document.documentElement.dataset.theme = theme;
  document.documentElement.style.colorScheme = theme;
  if (ack && !locked && studioURL && frame.contentWindow) {
    frame.contentWindow.postMessage({ type: "sources:canvas-appearance", version: 1,
      theme, lang: hostConfig?.lang?.startsWith("en") ? "en-US" : "zh-CN" }, studioURL.origin);
  }
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
  generation++;
  configuration = null;
  granted.clear();
  ack = false;
  clearInterval(sendTimer);
  clearInterval(checkTimer);
  clearTimeout(startupTimer);
  frame.src = "about:blank";
  frame.hidden = true;
  select.disabled = true;
  customButton.hidden = manageButton.hidden = ownKeyButton.hidden = true;
  recover(text);
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
async function api(path, body) {
  const result = await authorizedJSON(path, { method: body ? "POST" : "GET", body });
  if (path === "/api/canvas/connection" && result.status === 409 && result.data?.error === CUSTOM_CONNECTION_NEEDS_UPDATE) {
    throw Error(typeof result.data.message === "string" && result.data.message
      ? result.data.message : "密钥所在的分组已经变化，请重新保存自定义连接。", { cause: CUSTOM_CONNECTION_NEEDS_UPDATE });
  }
  if (!result.ok) throw Error(result.status === 401
    ? "登录已失效，请重新登录主站后打开画布。"
    : result.status === 403 ? "画布授权已撤销或过期，请在主站 API 密钥页面核对。"
    : "授权暂不可用，请稍后重试；不会自动换组或创建新任务。");
  return result.data;
}
function send() {
  if (!locked && configuration && frame.contentWindow && !ack) {
    frame.contentWindow.postMessage({ type: "sub2api:media-studio-config", payload: configuration }, studioURL.origin);
  }
}
function startSending(current) {
  if (current !== generation || locked || !configuration) return;
  send();
  clearInterval(sendTimer);
  let attempts = 0;
  sendTimer = setInterval(() => {
    if (current !== generation || ack || locked) { clearInterval(sendTimer); return; }
    if (++attempts > 30) {
      clearInterval(sendTimer);
      recover("画布尚未确认连接。可重试确认；不会重新生成或重复提交任务。");
      return;
    }
    send();
  }, 500);
}
frame.addEventListener("load", () => startSending(generation));

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
    if (ack && !locked) notifyHost(true);
    return;
  }
  if (locked || event.source !== frame.contentWindow || event.origin !== studioURL?.origin || !configuration) return;
  if (data?.type === "sub2api:canvas-ready") { ack = false; startSending(generation); return; }
  if (data?.type === "sub2api:canvas-restore-connection") {
    const connectionId = data.connectionId, requestId = data.requestId;
    if (typeof connectionId !== "string" || !/^group-[1-9][0-9]*$/.test(connectionId) || typeof requestId !== "string" || !/^[a-f0-9-]{36}$/.test(requestId)) return;
    const current = generation, target = frame.contentWindow;
    try {
      const config = await api("/api/canvas/connection", { group_id: Number(connectionId.slice(6)), restore_only: true });
      if (current !== generation || target !== frame.contentWindow || locked) return;
      granted.set(connectionId, config);
      target.postMessage({ type: "sub2api:media-studio-config", payload: { ...config, requestId, selectActive: false } }, studioURL.origin);
    } catch (error) {
      if (current !== generation || target !== frame.contentWindow || locked) return;
      target.postMessage({ type: "sub2api:canvas-restore-error", connectionId, requestId }, studioURL.origin);
      if (error.cause === CUSTOM_CONNECTION_NEEDS_UPDATE) { resaveHint = true; resaveClearRequestId = null; recover(error.message); }
    }
    return;
  }
  if (data?.type !== "sub2api:media-studio-config-applied" || data?.payload?.status !== "applied" || data?.payload?.requestId !== configuration.requestId || data?.payload?.profileId !== `sub2api-media-studio-group-${select.value}`) return;
  const receivedGeneration = generation;
  try { await currentToken(); } catch { return; }
  if (receivedGeneration !== generation || event.source !== frame.contentWindow || locked) return;
  ack = true;
  clearInterval(sendTimer);
  clearTimeout(startupTimer);
  if (data.payload.requestId === resaveClearRequestId) { resaveHint = false; resaveClearRequestId = null; }
  // A stale custom connection keeps its hint instead of revealing the page.
  if (!resaveHint) { frame.hidden = false; showState("ready"); }
  applyAppearance();
  notifyHost(true);
});
async function connect() {
  if (locked || connecting || !studioURL || !/^[1-9][0-9]*$/.test(select.value)) return false;
  const current = ++generation;
  const previous = configuration;
  const previousAck = ack;
  connecting = true;
  select.disabled = true;
  showState("loading", "正在连接所选分组…");
  frame.hidden = true;
  clearInterval(sendTimer);
  armStartupTimeout();
  try {
    const config = await api("/api/canvas/connection", { group_id: Number(select.value) });
    if (current !== generation || locked) return false;
    granted.set(config.selectedConnectionId, config);
    configuration = { ...config,
      connections: [...granted.values()].flatMap((item) => item.connections),
      capabilities: [...granted.values()].flatMap((item) => item.capabilities), requestId: crypto.randomUUID() };
    // Only this new configuration's acknowledgement clears a re-save hint.
    resaveClearRequestId = configuration.requestId;
    ack = false;
    if (!frame.getAttribute("src")) frame.src = studioURL.href;
    else startSending(current);
    return true;
  } catch (error) {
    if (current !== generation || locked) return false;
    configuration = previous;
    ack = previousAck;
    if (previous) select.value = previous.selectedConnectionId.slice(6);
    frame.hidden = !previousAck;
    if (error.cause === CUSTOM_CONNECTION_NEEDS_UPDATE) { resaveHint = true; resaveClearRequestId = null; }
    recover(error.message);
    return false;
  } finally {
    if (current === generation && !locked) {
      connecting = false;
      select.disabled = false;
      retry.disabled = bootstrapping;
    }
  }
}
retry.onclick = () => {
  if (locked || connecting || bootstrapping) return;
  if (resaveHint) { openCustomConnectionSettings(); return; }
  if (!studioURL) { void bootstrap(); return; }
  if (configuration && !ack) {
    // Resend the existing configuration, not a second authorization request.
    showState("loading", "正在确认画布连接…");
    armStartupTimeout();
    startSending(generation);
    return;
  }
  if (configuration && ack) { showState("ready"); notifyHost(true); return; }
  void connect();
};
function watchSession() {
  clearInterval(checkTimer);
  checkTimer = setInterval(() => {
    if (!locked && document.visibilityState === "visible") void api("/api/canvas/groups").catch((error) => { if (!resaveHint) recover(error.message); });
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
  customButton.hidden = ownKeyButton.hidden = !custom.enabled;
  manageButton.hidden = data.can_manage !== true;
  return entries.length;
}
// After a settings change: read the groups again and reconnect only when the
// selected entry changed (or selectId asks for one).
async function reloadGroups(selectId) {
  if (locked || connecting || bootstrapping || !studioURL) return;
  try {
    const data = await api("/api/canvas/groups");
    if (locked) return;
    const previous = select.value;
    const count = renderGroups(data, selectId || previous);
    select.disabled = !count;
    if (!count) {
      recover(noGroups());
      return;
    }
    select.onchange = connect;
    if ((selectId || select.value !== previous || !configuration) && await connect()) watchSession();
  } catch (error) {
    if (!locked) recover(error.message);
  }
}
const connectionMenu = document.querySelector("#canvas-connection-menu");
function openCustomConnectionSettings() {
  if (locked) return;
  connectionMenu.open = false;
  openCustomSettings({ onSaved: (configured) => {
    // Saving or removing is the explicit handling that clears the hint.
    resaveHint = false;
    resaveClearRequestId = null;
    const removedId = `group-${custom.groupId}`;
    if (configured) {
      // The refresh below connects to the newly saved connection.
      showState("loading", "正在连接所选分组…");
    } else if (configuration && ack && configuration.selectedConnectionId !== removedId) {
      // The acknowledged page still uses another connection; render it now
      // instead of leaving the re-save button until the list refresh.
      frame.hidden = false;
      showState("ready");
      notifyHost(true);
    } else {
      // The page was using the removed connection: drop it and reconnect.
      if (configuration?.selectedConnectionId === removedId) { configuration = null; ack = false; }
      frame.hidden = true;
      recover("自定义连接已移除，正在刷新分组…");
    }
    // A changed or removed connection must not be resent from the earlier grant.
    granted.delete(removedId);
    void reloadGroups(configured ? String(custom.groupId) : "");
  } });
}
customButton.onclick = ownKeyButton.onclick = openCustomConnectionSettings;
manageButton.onclick = () => {
  if (locked) return;
  connectionMenu.open = false;
  openGroupSettings({ onSaved: () => void reloadGroups("") });
};
async function bootstrap() {
  if (locked || bootstrapping) return;
  bootstrapping = true;
  showState("loading", "正在连接画布，请稍候…");
  armStartupTimeout();
  try {
    const data = await api("/api/canvas/groups");
    if (locked) return;
    studioURL = new URL(data.canvas_url);
    studioURL.searchParams.set("sources_account", String(data.user_id));
    studioURL.searchParams.set("ui_mode", "embedded");
    studioURL.searchParams.set("src_host", location.origin);
    studioURL.searchParams.set("theme", hostConfig?.theme || (params.get("theme") === "dark" ? "dark" : "light"));
    document.querySelector("#keys").href = data.main_origin + "/keys";
    if (!renderGroups(data)) {
      select.disabled = true;
      recover(noGroups());
      return;
    }
    select.onchange = connect;
    if (await connect()) watchSession();
  } catch (error) {
    if (!locked) recover(error.message);
  } finally {
    bootstrapping = false;
    retry.disabled = connecting;
  }
}
setShell();
if (window.parent !== window) window.parent.postMessage({ type: "sources:studio-host-ready", version: 1 }, location.origin);
await bootstrap();
