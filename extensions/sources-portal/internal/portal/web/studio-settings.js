import { authorizedJSON } from "./session.js";

// Settings dialogs of the Portal page that hosts Image Studio and Canvas. They
// open above the client frame and call only this origin's /api/studio/ routes.
// Portal checks every value again; the checks here only save a round trip.

const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$/;
const PREFIX = /^(\/[A-Za-z0-9][A-Za-z0-9._~-]{0,63}){1,4}$/;
const MAX_MODELS = 32;
const IMAGE_LIKE = /image|imagen|imagine|dall|flux|banana|seedream/i;
// The prefix is where Main serves the interface; the paths below it are the
// interface's own and are never typed by the user.
const PROTOCOLS = [
  { value: "openai_images", label: "OpenAI 图片接口", prefix: "/v1", paths: "/images/…（生成与编辑）" },
  { value: "grok_image", label: "Grok 图片接口", prefix: "/v1", paths: "/images/…（生成与编辑）" },
  { value: "gemini_native", label: "Gemini 原生接口", prefix: "/v1beta", paths: "/models/<模型>:generateContent" },
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

/** The prefix to use for an interface, or why the typed one cannot be used. */
export function customPrefix(protocol, typed) {
  const item = PROTOCOLS.find((entry) => entry.value === protocol);
  if (!item) return { error: "请选择接口类型。" };
  const prefix = typed.trim() || item.prefix;
  if (prefix.length > 128 || !PREFIX.test(prefix)) return { error: "前缀需以 / 开头，由 1 到 4 段字母、数字和 . _ ~ - 组成，例如 /v1。" };
  if (protocol !== "gemini_native" && !prefix.endsWith("/v1")) return { error: "OpenAI 和 Grok 接口的前缀需要以 /v1 结尾，例如 /v1。" };
  return { prefix };
}

/** The body of a custom connection save, or the reason it cannot be saved. */
export function customRequest({ keyId, protocol, prefix, models }) {
  if (!Number.isSafeInteger(keyId) || keyId < 1) return { error: "请选择一把密钥。" };
  const checked = customPrefix(protocol, prefix);
  if (checked.error) return checked;
  if (!models.length) return { error: "请至少选择一个模型。" };
  return { body: { key_id: keyId, protocol, prefix: checked.prefix, models } };
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

let current = null;
function openDialog(title, hint) {
  current?.close();
  const body = el("div", { class: "portal-dialog-body" });
  const status = el("p", { class: "portal-dialog-status", role: "status", "aria-live": "polite" });
  const actions = el("div", { class: "portal-dialog-actions" });
  const node = el("dialog", { class: "portal-dialog", "aria-label": title },
    el("h2", { text: title }), el("p", { class: "portal-dialog-hint", text: hint }), body, status, actions);
  // The close event arrives later, and in a background tab much later, so a
  // dialog closed from here is removed at once; the event covers the Escape key.
  const cleanup = () => {
    if (view.closed) return;
    view.closed = true;
    node.remove();
    window.removeEventListener("sources:locked", view.close);
    if (current === view) current = null;
  };
  const view = { node, body, status, actions, closed: false, close: () => { if (node.open) node.close(); cleanup(); } };
  node.addEventListener("close", cleanup);
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
async function busy(view, text, task) {
  if (view.closed) return;
  const controls = [...view.node.querySelectorAll("button,input,select")].filter((control) => !control.disabled);
  for (const control of controls) control.disabled = true;
  say(view, text);
  try {
    await task();
  } catch (error) {
    if (!view.closed) say(view, error.message || "操作未完成，请重试。", "error");
  } finally {
    for (const control of controls) control.disabled = false;
  }
}
async function request(path, options, messages = {}) {
  const result = await authorizedJSON(path, options);
  if (result.ok) return result.data;
  throw Error(messages[result.data?.error] || (result.status === 401 ? "登录已失效，请重新登录主站后再试。" : "暂时无法完成，请稍后重试。"));
}

// Models are fetched and then picked from the list. Typing a name is the
// fallback for a list that cannot be fetched or lacks a model.
function modelPicker({ selected, load, onChange = () => {}, preselect = false }) {
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
    refreshButton.disabled = true;
    say("正在获取模型…");
    try {
      const ids = sortCandidates(await load());
      if (current !== sequence) return;
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
      if (current !== sequence) return;
      // A value the user has to correct first is not a failed fetch; the
      // field that holds it says what is wrong.
      if (error.fixInput) say("");
      else {
        manual.open = true;
        say(`${error.message || "获取模型失败。"}可以在下方手动填写模型名称。`, "error");
      }
    } finally {
      if (current === sequence) {
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
export function openGroupSettings({ onSaved = () => {} } = {}) {
  const view = openDialog("工作台分组设置", "勾选的分组会出现在图片工作台和无限画布里，仍然只有对该分组有权限的用户能看到它。保存后立即生效，不需要重启。");
  const messages = {
    source_changed_refresh_page: "其他管理员刚修改过设置，已载入最新内容，请重新调整后保存。",
    studio_group_is_server_managed: "媒体引擎分组只能在服务器的登记文件里调整。",
    invalid_studio_registry: "有分组或模型 ID 不符合要求，请检查后重试。",
    persistence_uncertain_restart_required: "设置已写入但未确认落盘，请联系运维核对后重启 Portal。",
    admin_required: "只有管理员可以修改分组设置。",
  };
  const allowCustom = el("input", { type: "checkbox" });
  const list = el("ul", { class: "portal-rows" });
  let revision = 0, rows = [];
  const save = () => {
    const { groups, error } = managedGroups(rows.map((row) => row.entry()));
    if (error) { say(view, error, "error"); return; }
    void busy(view, "正在保存…", async () => {
      const result = await authorizedJSON("/api/studio/admin/registry", { method: "PUT", body: { revision, custom_disabled: !allowCustom.checked, groups } });
      if (result.ok) {
        show(result.data);
        say(view, "已保存，已经生效。");
        onSaved();
        return;
      }
      if (result.data?.error === "source_changed_refresh_page") show(await request("/api/studio/admin/registry", undefined, messages));
      throw Error(messages[result.data?.error] || "暂时无法保存，请稍后重试。");
    });
  };
  const saveButton = button("保存", save, "portal-primary");
  function row(group) {
    let dirty = false, reset = false;
    const touched = () => { dirty = true; reset = false; note.textContent = ""; };
    const check = el("input", { type: "checkbox", checked: group.enabled, disabled: group.locked });
    const picker = group.locked ? null : modelPicker({ selected: group.models, onChange: touched, preselect: true,
      load: async () => (await request(`/api/studio/admin/groups/${group.group_id}/models`, undefined, messages)).models });
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
    allowCustom.checked = data.custom_disabled !== true;
    rows = data.groups.map(row);
    list.replaceChildren(...rows.map((item) => item.node));
    if (!rows.length) list.append(el("li", { class: "portal-empty", text: "主站还没有启用的分组。" }));
    saveButton.hidden = data.writable !== true;
    if (data.writable !== true) say(view, messages.persistence_uncertain_restart_required, "error");
  }
  view.body.append(el("label", { class: "portal-toggle" }, allowCustom, "允许用户用自己的 API 密钥连接（自定义连接）"), list);
  view.actions.append(button("关闭", view.close), saveButton);
  void busy(view, "正在读取分组…", async () => {
    const data = await request("/api/studio/admin/registry", undefined, messages);
    say(view, "");
    show(data);
  });
}

/** Any user: connect with one of their own API keys. */
export function openCustomSettings({ onSaved = () => {} } = {}) {
  const view = openDialog("自定义连接", "用你在本站已有的 API 密钥连接图片工作台和无限画布，费用照常从余额扣除。域名固定为本站；前缀可以修改，其后的路径由系统自动拼接。");
  const messages = {
    invalid_custom_connection: "前缀或模型不符合要求，请检查后重试。",
    custom_connection_unavailable: "这把密钥在该前缀下暂时不能使用：可能已撤销、已过期、无权访问，或者属于需要从分组列表进入的分组。",
    custom_connection_disabled: "管理员已关闭自定义连接。",
  };
  view.actions.append(button("关闭", view.close));
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
    const prefixInput = el("input", { type: "text", autocomplete: "off", spellcheck: false, "aria-label": "地址前缀" });
    const preview = el("p", { class: "portal-note" });
    const selectedKey = () => keys.find((key) => String(key.id) === keySelect.value);
    const protocol = () => PROTOCOLS.find((entry) => entry.value === protocolSelect.value) || PROTOCOLS[0];
    const picker = modelPicker({ selected: saved ? saved.models : [], preselect: true, load: async () => {
      const { prefix, error } = customPrefix(protocolSelect.value, prefixInput.value);
      if (error) throw Object.assign(Error(error), { fixInput: true });
      return (await request("/api/studio/custom/models", { method: "POST", body: { key_id: Number(keySelect.value), prefix } }, messages)).models;
    } });
    const describe = () => {
      const { prefix, error } = customPrefix(protocolSelect.value, prefixInput.value);
      preview.textContent = error || `前缀后面的路径由系统自动拼接，不需要填写：${prefix}${protocol().paths}`;
      preview.dataset.kind = error ? "error" : "";
    };
    // The key's group decides where to start; the user may change either field.
    const defaults = (key) => key.protocol === protocolSelect.value && key.prefix ? key.prefix : protocol().prefix;
    keySelect.value = String(saved ? saved.key_id : keys.find((key) => !key.blocker).id);
    protocolSelect.value = saved ? saved.protocol : selectedKey().protocol;
    prefixInput.value = saved ? saved.prefix : defaults(selectedKey());
    keySelect.addEventListener("change", () => {
      // Another key is another group: its interface, prefix and models start over.
      protocolSelect.value = selectedKey().protocol;
      prefixInput.value = defaults(selectedKey());
      describe();
      picker.reset();
      void picker.refresh();
    });
    protocolSelect.addEventListener("change", () => {
      prefixInput.value = defaults(selectedKey());
      describe();
      picker.reset();
      void picker.refresh();
    });
    prefixInput.addEventListener("input", describe);
    prefixInput.addEventListener("change", () => void picker.refresh());
    describe();
    view.body.append(
      el("label", { class: "portal-field" }, "API 密钥", keySelect),
      el("label", { class: "portal-field" }, "接口类型", protocolSelect),
      el("div", { class: "portal-field" }, "请求地址",
        el("div", { class: "portal-address" }, el("code", { class: "portal-fixed", text: data.api_origin || location.origin, title: "域名固定，不能修改" }), prefixInput), preview),
      el("div", { class: "portal-field" }, "模型", picker.node));
    const save = () => {
      const { body, error } = customRequest({ keyId: Number(keySelect.value), protocol: protocolSelect.value, prefix: prefixInput.value, models: picker.value });
      if (error) { say(view, error, "error"); return; }
      void busy(view, "正在保存…", async () => {
        await request("/api/studio/custom", { method: "PUT", body }, messages);
        view.close();
        onSaved(true);
      });
    };
    if (data.binding) view.actions.append(button("删除自定义连接", () => void busy(view, "正在删除…", async () => {
      await request("/api/studio/custom", { method: "DELETE" }, messages);
      view.close();
      onSaved(false);
    })));
    view.actions.append(button("保存并连接", save, "portal-primary"));
    // The list is fetched as soon as the dialog opens.
    void picker.refresh();
  }
  void busy(view, "正在读取你的密钥…", async () => {
    show(await request("/api/studio/custom", undefined, messages));
    say(view, "");
  });
}
