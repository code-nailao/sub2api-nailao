<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, toRaw, computed } from "vue";
import { authorizedJSON } from "../../internal/portal/web/session.js";
import Home from "./Home.vue";
import Fields from "./Fields.vue";
import type { HomeConfig } from "./types";
const admin = location.pathname === "/admin/home";
const config = ref<HomeConfig>();
const saved=ref('')
const dirty=computed(()=>Boolean(config.value)&&JSON.stringify(config.value)!==saved.value)

const origin = ref("");
const error = ref("");
const notice = ref("");
const preview = ref(false);
const previewConfig = ref<HomeConfig>();
let previewAssets: string[] = [];
function clearPreview() {
  for (const url of previewAssets) URL.revokeObjectURL(url);
  previewAssets = [];
  previewConfig.value = undefined;
  preview.value = false;
}
async function togglePreview() {
  if (preview.value) {
    clearPreview();
    return;
  }
  if (!config.value) return;
  try {
    const value = structuredClone(toRaw(config.value));
    const urls = new Map<string, string>();
    const slots = [
      { item: value, key: "logo_light" },
      { item: value, key: "logo_dark" },
      ...value.brand.contacts.flatMap((item) => [
        { item, key: "logo" },
        { item, key: "qr_image" },
      ]),
    ];
    for (const slot of slots) {
      const item = slot.item as unknown as Record<string, string>;
      const path = item[slot.key];
      if (path.startsWith("/content-assets/")) {
        if (!urls.has(path)) {
          const result = await authorizedJSON(
            "/api/home/assets/" + path.split("/").pop(),
            { responseType: "blob" },
          );
          if (!result.ok) throw Error("预览图片暂不可用");
          const url = URL.createObjectURL(result.data);
          urls.set(path, url);
          previewAssets.push(url);
        }
        item[slot.key] = urls.get(path)!;
      }
    }
    previewConfig.value = value;
    preview.value = true;
  } catch (e) {
    clearPreview();
    error.value = (e as Error).message;
  }
}

const busy = ref(false);
const revision = ref(0);
const versions = ref<
  { id: number; published_at: string; published_by: number }[]
>([]);
async function request(path: string, method = "GET", body?: unknown) {
  const result = admin
    ? await authorizedJSON(path, { method, body })
    : await fetch(path, { credentials: "omit" }).then(async (response) => ({
        ok: response.ok,
        status: response.status,
        data: await response.json(),
      }));
  if (!result.ok) {
    if (result.status === 401) throw Error("登录已过期，请从主站重新打开。");
    if (result.status === 403) throw Error("此编辑器仅限管理员。");
    if (result.status === 409)
      throw Error("内容已被另一位管理员修改，请重新载入后再编辑。");
    throw Error("操作失败，请检查内容格式或稍后重试。");
  }
  return result.data;
}

async function load() {
  error.value = "";
  try {
    const data = await request(admin ? "/api/home" : "/public/home");
    config.value = data.config;
    saved.value=JSON.stringify(data.config);
    origin.value = data.main_origin;
    revision.value = data.revision;
    versions.value = data.versions || [];
  } catch (e) {
    error.value = (e as Error).message;
  }
}
async function change(action: string, id?: number) {
  busy.value = true;
  notice.value = "";
  error.value = "";
  try {
    const data = await request(`/api/home/${action}`, "POST", {
      revision: revision.value,
      config: action === "draft" ? config.value : undefined,
      version: id,
    });
    config.value = data.config;
    saved.value=JSON.stringify(data.config);
    revision.value = data.revision;
    versions.value = data.versions;
    notice.value =
      action === "publish"
        ? "已发布，公开首页已更新。"
        : action === "restore"
          ? "历史版本已恢复为草稿；预览确认后发布。"
          : "草稿已保存，公开首页未改变。";
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
function locked() {
  if (admin) {
    clearPreview();
    config.value = undefined;
    error.value = "主站已退出，请重新登录后打开。";
  }
}
onMounted(() => {
  window.addEventListener("sources:locked", locked);
  void load();
});
onBeforeUnmount(() => {
  clearPreview();
  window.removeEventListener("sources:locked", locked);
});
</script>
<template>
  <div
    v-if="admin"
    class="min-h-screen bg-slate-50 text-slate-900 dark:bg-slate-950 dark:text-slate-100"
  >
    <header
      class="sticky top-0 z-[60] border-b bg-white/95 p-4 backdrop-blur dark:border-slate-800 dark:bg-slate-900/95"
    >
      <div class="mx-auto flex max-w-7xl flex-wrap items-center gap-3">
        <h1 class="mr-auto text-xl font-bold">首页内容管理</h1>
        <button class="rounded border px-3 py-2" @click="togglePreview">
          {{ preview ? "返回编辑" : "预览草稿" }}</button
        ><button
          :disabled="busy || !config"
          class="rounded border px-3 py-2 disabled:opacity-50"
          @click="change('draft')"
        >
          保存草稿</button
        ><button
          :disabled="busy || !config || dirty"
          class="rounded bg-primary-600 px-3 py-2 font-semibold text-slate-950 disabled:opacity-50"
          @click="change('publish')"
        >
          发布已保存草稿
        </button>
      </div>
    </header>
    <p v-if="error" role="alert" class="mx-auto max-w-5xl p-4 text-red-600">
      {{ error }}
    </p>
    <p v-if="dirty" role="status" class="mx-auto max-w-5xl p-4 text-amber-700">有未保存修改，请先保存草稿再发布。</p>
    <p v-if="notice" role="status" class="mx-auto max-w-5xl p-4 text-green-600">
      {{ notice }}
    </p>
    <div v-if="config && !preview" class="mx-auto max-w-5xl space-y-6 p-5">
      <p>
        配置只保存在独立扩展服务。草稿与公开版本分离；历史恢复先生成草稿。首页价格/统计为编辑内容，不改变实际计费。
      </p>
      <Fields :model-value="config as unknown as Record<string, unknown>" />
      <details class="rounded border p-4 dark:border-slate-700">
        <summary>发布历史</summary>
        <div
          v-for="v in versions"
          :key="v.id"
          class="flex items-center justify-between border-b py-3 dark:border-slate-700"
        >
          <span
            >#{{ v.id }} · {{v.published_by ? v.published_at+' · 管理员 '+v.published_by : '初始导入内容'}}</span
          ><button :disabled="busy" @click="change('restore', v.id)">
            恢复为草稿
          </button>
        </div>
      </details>
    </div>
    <div v-if="preview && previewConfig" class="relative mt-16">
      <Home :key="revision" :config="previewConfig" :main-origin="origin" />
    </div>
  </div>
  <Home v-else-if="config" :config="config" :main-origin="origin" />
  <p v-else role="status" class="p-10 text-center">
    {{ error || "正在加载…" }}
  </p>
</template>
