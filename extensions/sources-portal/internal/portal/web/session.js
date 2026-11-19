// Portal is served on reviewed same-origin paths of Sub. No second token store.
const params = new URLSearchParams(location.search);
for (const key of ["token", "user_id", "src_url"]) params.delete(key);
history.replaceState(
  null,
  "",
  location.pathname + (params.size ? "?" + params : ""),
);
let locked = false;
const identity = sessionIdentity(localStorage.getItem("auth_token"));
let pendingSessionCheck;
function sessionIdentity(token) {
  if (!token) return "";
  try {
    const payload = token.split(".")[1];
    const value = JSON.parse(
      atob(payload.replace(/-/g, "+").replace(/_/g, "/")),
    );
    return `${value.user_id}:${value.sid || ""}`;
  } catch {
    return token;
  } // Not authentication: the server still validates every API call.
}
export const sessionReady = fetch("/public/runtime", {
  credentials: "omit",
}).then(async (r) => {
  if (!r.ok) throw Error("主站身份配置暂不可用");
  const config = await r.json();
  if (config.main_origin !== location.origin)
    throw Error("请从主站同源菜单打开此页面");
  return config;
});
export function clearSession() {
  if (locked) return;
  locked = true;
  window.dispatchEvent(new Event("sources:locked"));
}
export function isCurrentSession() {
  const token = localStorage.getItem("auth_token");
  return !locked && Boolean(token) && sessionIdentity(token) === identity;
}
export async function currentToken() {
  await sessionReady;
  const token = localStorage.getItem("auth_token");
  if (!isCurrentSession()) {
    clearSession();
    throw Error("主站已退出或切换账号，请重新进入。");
  }
  return token;
}
window.addEventListener("storage", (e) => {
  if (
    (e.key === "auth_token" || e.key === null) &&
    sessionIdentity(localStorage.getItem("auth_token")) !== identity
  )
    clearSession();
});

// Only the native parent owns token refresh. The signal contains no credentials;
// the child rereads the same-origin token only after verifying its session again.
function checkParentSession() {
  if (window.parent === window || locked) return Promise.resolve(false);
  if (pendingSessionCheck) return pendingSessionCheck;
  const parent = window.parent;
  const requestId = crypto.randomUUID();
  const pending = new Promise((resolve) => {
    const finish = (ok) => {
      clearTimeout(timeout);
      window.removeEventListener("message", receive);
      window.removeEventListener("sources:locked", cancel);
      resolve(ok);
    };
    const receive = (event) => {
      if (
        event.source === parent &&
        event.origin === location.origin &&
        event.data?.type === "sub2api:session-checked" &&
        event.data?.requestId === requestId
      ) finish(event.data.ok === true);
    };
    const cancel = () => finish(false);
    const timeout = setTimeout(cancel, 35000);
    window.addEventListener("message", receive);
    window.addEventListener("sources:locked", cancel);
    parent.postMessage({ type: "sub2api:session-check", requestId }, location.origin);
  });
  pendingSessionCheck = pending;
  void pending.finally(() => {
    if (pendingSessionCheck === pending) pendingSessionCheck = undefined;
  });
  return pending;
}

// One transport boundary for every authenticated UI. A late response must not
// refill a cleared page or deliver a key after logout/account switch.
/** @param {string} path @param {{method?:string,body?:unknown,rawBody?:Blob,contentType?:string,responseType?:string}} options */
export async function authorizedJSON(path, options = {}) {
  const url = new URL(path, location.origin);
  if (
    url.origin !== location.origin ||
    !/^\/api\/(balance|home|studio|canvas|intelligence-tests)(\/|$)/.test(url.pathname)
  ) {
    throw Error("扩展接口地址无效");
  }
  const request = {
    method: options.method || "GET",
    credentials: "omit",
    cache: "no-store",
    redirect: "error",
    headers: {
      ...(options.body === undefined && !options.rawBody
        ? {}
        : { "Content-Type": options.contentType || "application/json" }),
    },
    ...(options.rawBody
      ? { body: options.rawBody }
      : options.body === undefined
        ? {}
        : { body: JSON.stringify(options.body) }),
  };
  const send = async (token) => {
    const response = await fetch(url, {
      ...request,
      headers: { ...request.headers, Authorization: "Bearer " + token },
    });
    const data =
      response.status === 204
        ? null
        : options.responseType === "blob" && response.ok
          ? await response.blob()
          : await response.json();
    await currentToken();
    return { ok: response.ok, status: response.status, data };
  };
  const token = await currentToken();
  let result = await send(token);
  // This exact error is emitted by Portal's identity middleware before dispatch.
  // Other 4xx, 5xx and network/unknown outcomes must never replay a business write.
  if (result.status === 401 && result.data?.error === "admin_login_required") {
    let freshToken = await currentToken();
    if (freshToken === token) {
      await checkParentSession();
      freshToken = await currentToken();
    }
    if (freshToken !== token) result = await send(freshToken);
  }
  if (result.status === 401) clearSession();
  return result;
}
