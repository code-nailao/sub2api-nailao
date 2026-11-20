import { authorizedJSON } from "./session.js";
import { requestRounds } from "./request-round.js";

// Settings dialogs of the Portal page that hosts Image Studio and Canvas. They
// open above the client frame and call only this origin's /api/studio/ routes.
// Portal checks every value again; the checks here only save a round trip.

const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$/;
const API_LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const MAX_MODELS = 32;
const IMAGE_LIKE = /image|imagen|imagine|dall|flux|banana|seedream/i;
// Paths mirror the server contract and are never typed by the user.
const PROTOCOLS = [
  { value: "openai_images", label: "OpenAI 图片接口", prefix: "/v1" },
  { value: "grok_image", label: "Grok 图片接口", prefix: "/v1" },
  { value: "gemini_native", label: "Gemini 原生接口", prefix: "/v1beta" },
];
const GROUP_BLOCKERS = {
  group_inactive: "分组未启用",
  image_generation_disabled: "分组未开启图片生成",
  unsupported_platform: "平台暂不支持",
  group_missing: "分组已删除或停用",
};
const KEY_BLOCKERS = {
  no_group: "未绑定分组",
  media_engine_group: "请直接在分组列表中选择",
};

/** Candidates that look like image models first; the order is otherwise kept. */
export function sortCandidates(models) {
  const image = models.filter((id) => IMAGE_LIKE.test(id));
  return [...image, ...models.filter((id) => !IMAGE_LIKE.test(id))];
}

/** The managed entries to save, from the rows as the administrator left them. */
export function managedGroups(entries) {
  const groups = [];
  for (const { group, enabled, models, dirty, reset } of entries) {
    if (group.locked || reset) continue;
    const kept = group.source === "managed" || group.source === "override";
    if (!kept && !dirty) continue;
    const list = models.length ? models : group.file_models?.length ? group.file_models : group.models;
    if (!list.length) {
      if (enabled) return { error: `请为「${group.name || "#" + group.group_id}」至少选择一个模型。` };
      continue;
    }
    // A file entry put back exactly as the file has it needs no managed copy.
    if (!kept && group.source === "file" && enabled && list.join("\n") === group.models.join("\n")) continue;
    groups.push({ group_id: group.group_id, models: list, enabled });
  }
  return { groups };
}

/** Mirror the server's derived path for the read-only preview. */
export function customPathPrefix(platform, protocol) {
  if (platform === "antigravity") return protocol === "gemini_native" ? "/antigravity/v1beta" : null;
  return PROTOCOLS.find((item) => item.value === protocol)?.prefix || null;
}

export function customAddressChoice(labels, defaultLabel, savedLabel) {
  return { label: labels.includes(savedLabel) ? savedLabel : defaultLabel,
    removed: Boolean(savedLabel && !labels.includes(savedLabel)) };
}

export function customPreview(origin, platform, protocol) {
  const prefix = customPathPrefix(platform, protocol);
  if (!prefix) return "请选择接口类型。";
  const paths = protocol === "gemini_native" ? `${origin}${prefix}/models/<模型>:generateContent`
    : `${origin}${prefix}/images/generations（编辑：${origin}${prefix}/images/edits）`;
  return `路径由系统自动拼接，不需要填写：${paths}`;
}

export function customAPILabelRequest(labels, defaultLabel) {
  if (labels.length > 16 || labels.some((label) => !API_LABEL.test(label)) || new Set(labels).size !== labels.length ||
      (labels.length ? !labels.includes(defaultLabel) : defaultLabel !== "")) return { error: "API 子域名需为不重复的小写 DNS 标签，最多 16 个，并选择默认地址。" };
  return { custom_api_labels: labels, custom_api_default: defaultLabel };
}

/** The save request contains a selected address, never an editable path. */
export function customRequest({ keyId, protocol, apiLabel = "", platform = "", models }) {
  if (!Number.isSafeInteger(keyId) || keyId < 1) return { error: "请选择一把密钥。" };
  if (!customPathPrefix(platform, protocol)) return { error: "请选择接口类型。" };
  if (apiLabel && !API_LABEL.test(apiLabel)) return { error: "请选择 API 子域名。" };
  if (!models.length) return { error: "请至少选择一个模型。" };
  return { body: { key_id: keyId, protocol, api_label: platform === "antigravity" ? "" : apiLabel, models } };
}

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else if (key.includes("-") || key === "role") node.setAttribute(key, value);
    else node[key] = value;
  }
  for (const child of children.flat()) if (child !== null && child !== undefined) node.append(child);
  return node;
}
const button = (text, onclick, className, label) => el("button", { type: "button", text, onclick, class: className, "aria-label": label });

let current = null, dialogSession = 0;
function openDialog(title, hint) {
  current?.close();
  const session = ++dialogSession;
  const body = el("div", { class: "portal-dialog-body" });
  const status = el("p", { class: "portal-dialog-status", role: "status", "aria-live": "polite" });
  const actions = el("div", { class: "portal-dialog-actions" });
  const node = el("dialog", { class: "portal-dialog", "aria-label": title },
    el("h2", { text: title }), el("p", { class: "portal-dialog-hint", text: hint }), body, status, actions);
  // The close event arrives later, and in a background tab much later, so a
  // dialog closed from here is removed at once; the event covers the Escape key.
  const cleanup = () => {
    view.operations.advance();
    node.remove();
    window.removeEventListener("sources:locked", view.close);
    if (dialogSession === session) {
      dialogSession++;
      current = null;
    }
  };
  const view = { node, body, status, actions, operations: requestRounds(), heldControls: new Set(),
    isOpen: () => session === dialogSession && node.open,
    close: () => { if (node.open) node.close(); cleanup(); } };
  node.addEventListener("close", () => { if (dialogSession === session) cleanup(); });
  // Signing out or switching account in another tab closes an open dialog.
  window.addEventListener("sources:locked", view.close);
  document.body.append(node);
  node.showModal();
  current = view;
  return view;
}
function say(view, text, kind = "") {
  view.status.textContent = text;
  view.status.dataset.kind = kind;
}
async function busy(view, text, task, isRelevant = () => true) {
  if (!view.isOpen()) return;
  const round = view.operations.start();
  const ownsOperation = () => view.isOpen() && view.operations.isWaiting(round);
  const current = () => ownsOperation() && isRelevant();
  const controls = [...view.node.querySelectorAll("button,input,select")].filter((control) => !control.disabled);
  for (const control of controls) {
    view.heldControls.add(control);
    control.disabled = true;
  }
  say(view, text);
  try {
    await task(current);
  } catch (error) {
    if (current()) say(view, error.message || "操作未完成，请重试。", "error");
  } finally {
    if (ownsOperation()) {
      if (!isRelevant()) say(view, "");
      view.operations.finish(round);
      for (const control of view.heldControls) control.disabled = false;
      view.heldControls.clear();
    }
  }
}
async function request(path, options, messages = {}) {
  const result = await authorizedJSON(path, options);
  if (result.ok) return result.data;
  throw Error(messages[result.data?.error] || (result.status === 401 ? "登录已失效，请重新登录主站后再试。" : "暂时无法完成，请稍后重试。"));
}

// Models are fetched and then picked from the list. Typing a name is the
// fallback for a list that cannot be fetched or lacks a model.
function modelPicker({ selected, load, isOpen, onChange = () => {}, preselect = false }) {
  let models = [...selected], candidates = [], filter = "", sequence = 0, loaded = false;
  const count = el("span", { class: "portal-note" });
  const note = el("p", { class: "portal-note", role: "status" });
  const search = el("input", { type: "text", placeholder: "搜索模型...", autocomplete: "off", spellcheck: false, hidden: true, "aria-label": "搜索模型" });
  const list = el("div", { class: "portal-candidates" });
  const custom = el("input", { type: "text", placeholder: "输入自定义模型名称", autocomplete: "off", spellcheck: false, "aria-label": "自定义模型名称" });
  const say = (text, kind = "") => { note.textContent = text; note.dataset.kind = kind; };
  const change = (next) => { models = next; onChange(); render(); };
  const add = (id) => {
    if (models.includes(id)) { say("该模型已存在。"); return; }
    if (!MODEL_ID.test(id)) { say(`模型名称「${id}」含有不支持的字符。`, "error"); return; }
    if (models.length >= MAX_MODELS) { say(`最多选择 ${MAX_MODELS} 个模型。`, "error"); return; }
    change([...models, id]);
  };
  const addTyped = () => {
    const id = custom.value.trim();
    if (!id) return;
    custom.value = "";
    add(id);
  };
  const manual = el("details", { class: "portal-manual" }, el("summary", { text: "获取不到？手动填写模型名称" }),
    el("div", { class: "portal-model-add" }, custom, button("填入", addTyped)));
  const refreshButton = button("获取模型", () => void refresh());
  const clearButton = button("清除所有模型", () => change([]), "portal-link");
  function render() {
    // A picked model the list does not have (typed, or saved earlier) stays visible in front.
    const all = [...models.filter((id) => !candidates.includes(id)), ...candidates];
    const shown = filter ? all.filter((id) => id.toLowerCase().includes(filter)) : all;
    list.replaceChildren(...shown.map((id) => el("button", { type: "button", class: "portal-candidate", "aria-pressed": String(models.includes(id)), text: id,
      onclick: () => models.includes(id) ? change(models.filter((model) => model !== id)) : add(id) })));
    if (!shown.length) list.append(el("span", { class: "portal-empty", text: all.length ? "没有匹配的模型" : loaded ? "没有可选的模型" : "尚未获取模型" }));
    search.hidden = all.length <= 12;
    count.textContent = `已选择 ${models.length} 个模型`;
    clearButton.hidden = !models.length;
    refreshButton.textContent = loaded ? "重新获取" : "获取模型";
  }
  async function refresh() {
    const current = ++sequence;
    const accepted = () => isOpen() && current === sequence;
    refreshButton.disabled = true;
    say("正在获取模型…");
    try {
      const ids = sortCandidates(await load(accepted));
      if (!accepted()) return;
      candidates = ids;
      loaded = true;
      let picked = 0;
      if (preselect && !models.length) {
        // A starting point from the names only; the user confirms or changes it.
        const image = ids.filter((id) => IMAGE_LIKE.test(id)).slice(0, MAX_MODELS);
        picked = image.length;
        if (picked) { models = image; onChange(); }
      }
      if (!ids.length) {
        manual.open = true;
        say("没有获取到模型，可以在下方手动填写模型名称。", "error");
      } else say(picked ? `获取到 ${ids.length} 个模型，已按名称预选 ${picked} 个图片模型，可以调整。` : `获取到 ${ids.length} 个模型，点选要使用的。`);
    } catch (error) {
      if (!accepted()) return;
      // A value the user has to correct first is not a failed fetch; the
      // field that holds it says what is wrong.
      if (error.fixInput) say("");
      else {
        manual.open = true;
        say(`${error.message || "获取模型失败。"}可以在下方手动填写模型名称。`, "error");
      }
    } finally {
      if (accepted()) {
        refreshButton.disabled = false;
        render();
      }
    }
  }
  search.addEventListener("input", () => { filter = search.value.trim().toLowerCase(); render(); });
  custom.addEventListener("keydown", (event) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    addTyped();
  });
  render();
  return {
    node: el("div", { class: "portal-models" }, el("div", { class: "portal-models-head" }, count, refreshButton, clearButton), search, list, note, manual),
    get value() { return [...models]; },
    get loaded() { return loaded; },
    set(next) { models = [...next]; render(); },
    // Another key or interface is another list: an answer still on its way is dropped.
    reset() { sequence++; models = []; candidates = []; loaded = false; filter = ""; search.value = ""; refreshButton.disabled = false; say(""); render(); },
    refresh,
  };
}

/** Administrator: which groups the workbench and the canvas offer. */
export function openGroupSettings({ captureConnection = () => () => true, onSaved = () => {} } = {}) {
  const view = openDialog("工作台分组设置", "勾选的分组会出现在图片工作台和无限画布里，仍然只有对该分组有权限的用户能看到它。保存后立即生效，不需要重启。");
  const messages = {
    source_changed_refresh_page: "其他管理员刚修改过设置，已载入最新内容，请重新调整后保存。",
    studio_group_is_server_managed: "媒体引擎分组只能在服务器的登记文件里调整。",
    invalid_studio_registry: "有分组、模型或 API 子域名不符合要求，请检查后重试。",
    persistence_uncertain_restart_required: "设置已写入但未确认落盘，请联系运维核对后重启 Portal。",
    admin_required: "只有管理员可以修改分组设置。",
  };
  const allowCustom = el("input", { type: "checkbox" });
  const list = el("ul", { class: "portal-rows" });
  let revision = 0, rows = [], addressRows = [];
  const addressList = el("div");
  const addressSource = el("p", { class: "portal-note" });
  const addressDeployment = el("p", { class: "portal-note" });
  let apiHost = "";
  function addAddress(label = "", isDefault = false) {
    if (addressRows.length >= 16) { say(view, "最多设置 16 个 API 子域名。", "error"); return; }
    const input = el("input", { type: "text", value: label, autocomplete: "off", spellcheck: false, "aria-label": "API 子域名" });
    const radio = el("input", { type: "radio", name: "custom-api-default", checked: isDefault });
    const entry = { input, radio };
    entry.node = el("div", { class: "portal-address" }, input, el("code", { text: "." + apiHost }),
      el("label", {}, radio, "默认"), button("移除", () => {
        addressRows = addressRows.filter((item) => item !== entry);
        entry.node.remove();
        if (!addressRows.some((item) => item.radio.checked) && addressRows.length) addressRows[0].radio.checked = true;
      }, "portal-link", "移除 API 子域名"));
    addressRows.push(entry);
    addressList.append(entry.node);
  }
  const save = () => {
    const { groups, error } = managedGroups(rows.map((row) => row.entry()));
    if (error) { say(view, error, "error"); return; }
    const addresses = customAPILabelRequest(addressRows.map((row) => row.input.value.trim()), addressRows.find((row) => row.radio.checked)?.input.value.trim() || "");
    if (addresses.error) { say(view, addresses.error, "error"); return; }
    // A save answers the document and connection it started in.
    const stillConnected = captureConnection();
    void busy(view, "正在保存…", async (current) => {
      const result = await authorizedJSON("/api/studio/admin/registry", {
        method: "PUT", body: { revision, custom_disabled: !allowCustom.checked, groups, ...addresses }, isCurrent: current,
      });
      if (!current()) return;
      if (result.ok) {
        show(result.data);
        say(view, "已保存，已经生效。");
        onSaved();
        return;
      }
      if (result.data?.error === "source_changed_refresh_page") {
        const data = await request("/api/studio/admin/registry", { isCurrent: current }, messages);
        if (!current()) return;
        show(data);
      }
      throw Error(messages[result.data?.error] || "暂时无法保存，请稍后重试。");
    }, stillConnected);
  };
  const saveButton = button("保存", save, "portal-primary");
  function row(group) {
    let dirty = false, reset = false;
    const touched = () => { dirty = true; reset = false; note.textContent = ""; };
    const check = el("input", { type: "checkbox", checked: group.enabled, disabled: group.locked });
    const picker = group.locked ? null : modelPicker({ selected: group.models, isOpen: view.isOpen, onChange: touched, preselect: true,
      load: async (isCurrent) => {
        const stillConnected = captureConnection();
        return (await request(`/api/studio/admin/groups/${group.group_id}/models`,
          { isCurrent: () => stillConnected() && isCurrent() }, messages)).models;
      } });
    const note = el("span", { class: "portal-note" });
    const tags = [
      group.locked ? "媒体引擎 · 服务器登记，只读" : { file: "服务器登记", override: "已在此修改", managed: "已在此加入" }[group.source],
      ...group.blockers.map((blocker) => GROUP_BLOCKERS[blocker] || blocker),
    ].filter(Boolean);
    const editor = el("div", { class: "portal-row-models" },
      picker ? picker.node : el("p", { class: "portal-fixed-models", text: group.models.join("、") }));
    const sync = () => { editor.hidden = !check.checked && !group.locked; };
    check.addEventListener("change", () => {
      touched();
      sync();
      // Ticking a group fetches its models at once, so they are picked, not typed.
      if (check.checked && picker && !picker.loaded) void picker.refresh();
    });
    const undo = group.source === "override" || group.source === "managed" ? button(group.source === "override" ? "恢复服务器登记" : "移除", () => {
      reset = true;
      dirty = false;
      check.checked = group.source === "override";
      picker.set(group.source === "override" ? group.file_models || [] : []);
      note.textContent = "保存后生效";
      sync();
    }, "portal-link") : null;
    sync();
    return {
      node: el("li", { class: "portal-row" },
        el("label", { class: "portal-row-head" }, check,
          el("span", { class: "portal-row-name", text: group.name || `分组 #${group.group_id}` }),
          el("span", { class: "portal-row-meta", text: `${group.platform || "未知平台"} · #${group.group_id}` })),
        el("div", { class: "portal-tags" }, tags.map((text) => el("span", { class: "portal-tag", text })), undo, note),
        editor),
      entry: () => ({ group, enabled: check.checked, models: picker ? picker.value : [], dirty, reset }),
    };
  }
  function show(data) {
    revision = data.revision;
    apiHost = data.api_host;
    addressRows = [];
    addressList.replaceChildren();
    for (const label of data.custom_api_labels || []) addAddress(label, label === data.custom_api_default);
    addressSource.textContent = `当前生效来源：${{ online: "在线设置", deployment: "部署配置", none: "未设置，使用主站地址" }[data.custom_api_source]}`;
    const deployment = data.deployment_api_labels || [];
    addressDeployment.textContent = deployment.length ? `部署初始值：${deployment.map((label) => label + "." + apiHost + (label === data.deployment_api_default ? "（默认）" : "")).join("、")}` : "部署初始值：未设置";
    allowCustom.checked = data.custom_disabled !== true;
    rows = data.groups.map(row);
    list.replaceChildren(...rows.map((item) => item.node));
    if (!rows.length) list.append(el("li", { class: "portal-empty", text: "主站还没有启用的分组。" }));
    saveButton.hidden = data.writable !== true;
    if (data.writable !== true) say(view, messages.persistence_uncertain_restart_required, "error");
  }
  view.body.append(el("label", { class: "portal-toggle" }, allowCustom, "允许用户用自己的 API 密钥连接（自定义连接）"),
    el("section", { class: "portal-api-settings" }, el("h3", { text: "自定义连接可选的 API 子域名" }), addressList,
      button("添加", () => addAddress("", addressRows.length === 0), "portal-link", "添加 API 子域名"), addressSource, addressDeployment,
      el("p", { class: "portal-note", text: "留空则使用部署配置的初始值；都没有时使用主站地址。" })), list);
  view.actions.append(button("关闭", view.close), saveButton);
  const openedFor = captureConnection();
  void busy(view, "正在读取分组…", async (current) => {
    const data = await request("/api/studio/admin/registry", { isCurrent: current }, messages);
    if (!current()) return;
    say(view, "");
    show(data);
  }, openedFor);
}

/** Any user: connect with one of their own API keys. */
export function openCustomSettings({ captureConnection = () => () => true, onSaved = () => {} } = {}) {
  const view = openDialog("自定义连接", "用你在本站已有的 API 密钥连接图片工作台和无限画布，费用照常从余额扣除。域名固定为本站，可以选择 API 子域名；路径由系统自动拼接。");
  const messages = {
    invalid_custom_connection: "API 地址、接口或模型不符合要求，请检查后重试。",
    custom_connection_unavailable: "这把密钥暂时不能使用：可能已撤销、已过期、无权访问，或者属于需要从分组列表进入的分组。",
    custom_connection_disabled: "管理员已关闭自定义连接。",
  };
  view.actions.append(button("关闭", view.close));
  function persistBinding(configured, options) {
    const currentConnection = captureConnection();
    return busy(view, configured ? "正在保存…" : "正在删除…", async (current) => {
      await request("/api/studio/custom", { ...options, isCurrent: current }, messages);
      if (!current()) return;
      view.close();
      onSaved(configured);
    }, currentConnection);
  }
  function show(data) {
    if (data.enabled !== true) { view.body.append(el("p", { text: messages.custom_connection_disabled })); return; }
    const keys = Array.isArray(data.keys) ? data.keys : [];
    if (!keys.some((key) => !key.blocker)) {
      view.body.append(el("p", {}, "当前账号还没有可用的 API 密钥。请先到 ",
        el("a", { href: "/keys", target: "_top", text: "API 密钥" }), " 页面，在要使用的分组下创建一把。"));
      return;
    }
    const saved = data.binding && keys.some((key) => key.id === data.binding.key_id && !key.blocker) ? data.binding : null;
    const keySelect = el("select", { "aria-label": "API 密钥" }, keys.map((key) => el("option", { value: String(key.id), disabled: Boolean(key.blocker),
      text: [key.name || "未命名", key.group_name || `分组 #${key.group_id}`, key.hint ? `…${key.hint}` : "", key.blocker ? `（${KEY_BLOCKERS[key.blocker] || "不可用"}）` : ""].filter(Boolean).join(" · ") })));
    const protocolSelect = el("select", { "aria-label": "接口类型" }, PROTOCOLS.map((item) => el("option", { value: item.value, text: item.label })));
    const labelSelect = el("select", { "aria-label": "API 子域名" }, (data.api_labels || []).map((label) => el("option", { value: label, text: label })));
    const address = el("div", { class: "portal-address" });
    const preview = el("p", { class: "portal-note" });
    const notice = el("p", { class: "portal-note" });
    const antiHint = el("p", { class: "portal-note", text: "Antigravity 分组暂时只能使用主站地址。" });
    const selectedKey = () => keys.find((key) => String(key.id) === keySelect.value);
    const choice = customAddressChoice(data.api_labels || [], data.api_default || "", saved?.api_label);
    labelSelect.value = choice.label;
    // A connection saved before subdomains existed still uses the main address until it is saved again.
    const legacy = Boolean(saved && !saved.api_label && (data.api_labels || []).length);
    notice.textContent = choice.removed ? "之前选的地址已停用，已改用默认地址。" : legacy ? "现在的连接仍使用主站地址，保存后改用上面选择的地址。" : "";
    const picker = modelPicker({ selected: saved ? saved.models : [], isOpen: view.isOpen, preselect: true, load: async (isCurrent) => {
      const stillConnected = captureConnection();
      return (await request("/api/studio/custom/models", {
        method: "POST", body: { key_id: Number(keySelect.value), protocol: protocolSelect.value },
        isCurrent: () => stillConnected() && isCurrent(),
      }, messages)).models;
    } });
    const describe = () => {
      const anti = selectedKey().platform === "antigravity";
      const selectable = !anti && (data.api_labels || []).length > 0;
      address.replaceChildren(...(selectable ? [el("code", { text: "https://" }), labelSelect, el("code", { text: "." + data.api_host })]
        : [el("code", { class: "portal-fixed", text: data.api_origin })]));
      antiHint.hidden = !anti;
      notice.hidden = anti || !notice.textContent;
      preview.textContent = customPreview(selectable ? `https://${labelSelect.value}.${data.api_host}` : data.api_origin, selectedKey().platform, protocolSelect.value);
    };
    const setProtocols = (value) => {
      const options = selectedKey().platform === "antigravity" ? PROTOCOLS.filter((item) => item.value === "gemini_native") : PROTOCOLS;
      protocolSelect.replaceChildren(...options.map((item) => el("option", { value: item.value, text: item.label })));
      protocolSelect.value = options.some((item) => item.value === value) ? value : selectedKey().protocol;
    };
    keySelect.value = String(saved ? saved.key_id : keys.find((key) => !key.blocker).id);
    setProtocols(saved ? saved.protocol : selectedKey().protocol);
    keySelect.addEventListener("change", () => {
      setProtocols(selectedKey().protocol);
      describe();
      picker.reset();
      void picker.refresh();
    });
    protocolSelect.addEventListener("change", () => {
      describe();
      picker.reset();
      void picker.refresh();
    });
    labelSelect.addEventListener("change", describe);
    describe();
    view.body.append(
      el("label", { class: "portal-field" }, "API 密钥", keySelect),
      el("label", { class: "portal-field" }, "接口类型", protocolSelect),
      el("div", { class: "portal-field" }, "请求地址",
        address, notice, antiHint, preview),
      el("div", { class: "portal-field" }, "模型", picker.node));
    const save = () => {
      const { body, error } = customRequest({ keyId: Number(keySelect.value), protocol: protocolSelect.value, apiLabel: labelSelect.value, platform: selectedKey().platform, models: picker.value });
      if (error) { say(view, error, "error"); return; }
      void persistBinding(true, { method: "PUT", body });
    };
    if (data.binding) view.actions.append(button("删除自定义连接", () => void persistBinding(false, { method: "DELETE" })));
    view.actions.append(button("保存并连接", save, "portal-primary"));
    // The list is fetched as soon as the dialog opens.
    void picker.refresh();
  }
  const openedFor = captureConnection();
  void busy(view, "正在读取你的密钥…", async (current) => {
    const data = await request("/api/studio/custom", { isCurrent: current }, messages);
    if (!current()) return;
    show(data);
    say(view, "");
  }, openedFor);
}
