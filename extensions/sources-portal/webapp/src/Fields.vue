<script setup lang="ts">
import { ref } from "vue";
import { authorizedJSON } from "../../internal/portal/web/session.js";
const uploadError = ref("");
async function upload(
  event: Event,
  key: string,
  model: Record<string, unknown>,
) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  if (!file) return;
  uploadError.value = "";
  try {
    if (
      file.size > 2 * 1024 * 1024 ||
      !["image/png", "image/jpeg"].includes(file.type)
    )
      throw Error("请选择2MB以内的PNG/JPEG图片");
    const result = await authorizedJSON("/api/home/assets", {
      method: "POST",
      rawBody: file,
      contentType: file.type,
    });
    if (!result.ok) throw Error("图片格式错误或超过2048像素");
    model[key] = result.data.path;
  } catch (e) {
    uploadError.value = (e as Error).message;
  } finally {
    input.value = "";
  }
}

defineProps<{ modelValue: Record<string, unknown>; prefix?: string }>();
const labels: Record<string, string> = {
  site_name: "站点名称",
  logo_light: "浅色 Logo",
  logo_dark: "深色 Logo",
  doc_url: "文档地址",
  welfare_path: "站内福利中心路径",
  brand: "首页内容",
  sections: "显示区域",
  contacts: "联系方式",
  pricing: "价格表",
  features: "特点",
  stats: "展示数据（编辑内容，非实时监控）",
  hero_badge: "首屏提示",
  hero_title: "主标题",
  hero_subtitle: "副标题",
  primary_cta_label: "主按钮文案",
  primary_cta_url: "主按钮链接",
  secondary_cta_label: "次按钮文案",
  secondary_cta_url: "次按钮链接",
  experience_title: "介绍标题",
  announcement: "公告",
  enabled: "启用",
  text: "正文",
  highlights: "亮点",
  label: "文案",
  name: "名称",
  account: "联系账号",
  logo: "图标",
  qr_image: "二维码图片",
  url: "链接",
  icon: "图标",
  title: "标题",
  description: "说明",
  value: "展示值",
  footer_groups: "页脚",
  links: "链接列表",
  pricing_table: "价格表",
  headings: "表头",
  rows: "价格行",
  model: "模型",
  spec: "规格",
  group: "分组",
  multiplier: "倍率",
  input: "输入价格",
  output: "输出价格",
  official: "对比价格",
  discount: "折扣",
};
const templates: Record<string, unknown> = {
  highlights: { label: "" },
  contacts: { name: "", account: "", logo: "", qr_image: "", url: "" },
  features: { icon: "zap", title: "", description: "" },
  stats: { value: "", label: "" },
  footer_groups: { title: "", links: [] },
  links: { label: "", url: "" },
  headings: "",
  rows: {
    model: "",
    spec: "",
    group: "",
    multiplier: "",
    input: "",
    output: "",
    official: "",
    discount: "",
  },
};
function newItem(key: string) {
  return structuredClone(templates[key] ?? "");
}
</script>
<template>
  <div class="space-y-3">
    <p v-if="uploadError" role="alert" class="text-red-600">
      {{ uploadError }}
    </p>
    <div v-for="(value, key) in modelValue" :key="key">
      <label v-if="typeof value === 'string'" class="block text-sm"
        >{{ labels[key] || key
        }}<textarea
          :aria-label="`${prefix || ''}${labels[key] || key}`"
          class="mt-1 block w-full rounded border bg-transparent p-2 dark:border-slate-600"
          rows="2"
          :value="value"
          @input="
            modelValue[key] = ($event.target as HTMLTextAreaElement).value
          "
        />
        <input
          v-if="['logo_light', 'logo_dark', 'logo', 'qr_image'].includes(key)"
          type="file"
          accept="image/png,image/jpeg"
          :aria-label="`${prefix || ''}上传${labels[key] || key}`"
          class="mt-2 block text-xs"
          @change="upload($event, key, modelValue)"
        />
      </label>
      <label v-else-if="typeof value === 'boolean'" class="flex gap-2"
        ><input
          type="checkbox"
          :checked="value"
          @change="
            modelValue[key] = ($event.target as HTMLInputElement).checked
          "
        />{{ labels[key] || key }}</label
      >
      <details
        v-else-if="Array.isArray(value)"
        class="rounded border p-3 dark:border-slate-700"
      >
        <summary class="cursor-pointer font-semibold">
          {{ labels[key] || key }} ({{ value.length }})
        </summary>
        <div
          v-for="(item, i) in value"
          :key="i"
          class="my-3 rounded border p-3 dark:border-slate-700"
        >
          <Fields
            v-if="item && typeof item === 'object'"
            :model-value="item"
            :prefix="`${labels[key] || key}${i + 1} / `"
          />
          <input
            v-else
            class="w-full border bg-transparent p-2"
            :aria-label="`${labels[key] || key}${i + 1}`"
            :value="item"
            @input="value[i] = ($event.target as HTMLInputElement).value"
          />
          <button
            type="button"
            class="mt-2 text-sm text-red-600"
            @click="value.splice(i, 1)"
          >
            移除第 {{ i + 1 }} 项
          </button>
        </div>
        <button
          v-if="value.length < 32"
          type="button"
          class="mt-3 text-primary-800 dark:text-primary-300"
          @click="value.push(newItem(key))"
        >
          添加一项
        </button>
      </details>
      <details
        v-else-if="value && typeof value === 'object'"
        class="rounded border p-3 dark:border-slate-700"
      >
        <summary class="cursor-pointer font-semibold">
          {{ labels[key] || key }}
        </summary>
        <Fields
          class="mt-3"
          :model-value="value as Record<string, unknown>"
          :prefix="prefix"
        />
      </details>
    </div>
  </div>
</template>
