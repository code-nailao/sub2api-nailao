import { defineComponent, h } from "vue";
import { flushPromises, mount } from "@vue/test-utils";
import { createPinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { AdminGroup } from "@/types";
import GroupsView from "@/views/admin/GroupsView.vue";

const {
  showError,
  listGroups,
  getModelAllowlistCandidates,
  getUsageSummary,
  getCapacitySummary,
  getLiveCapability,
  createGroup,
  updateGroup,
} = vi.hoisted(() => ({
  showError: vi.fn(),
  listGroups: vi.fn(),
  getModelAllowlistCandidates: vi.fn(),
  getUsageSummary: vi.fn(),
  getCapacitySummary: vi.fn(),
  getLiveCapability: vi.fn(),
  createGroup: vi.fn(),
  updateGroup: vi.fn(),
}));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      getAll: vi.fn(),
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      create: createGroup,
      update: updateGroup,
      delete: vi.fn(),
      duplicate: vi.fn(),
      updateSortOrder: vi.fn(),
    },
    accounts: {
      list: vi.fn(),
      getById: vi.fn(),
    },
  },
}));

vi.mock("@/stores/app", () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
  }),
}));

vi.mock("@/stores/onboarding", () => ({
  useOnboardingStore: () => ({
    isCurrentStep: vi.fn(() => false),
    nextStep: vi.fn(),
  }),
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

const sourceGroup = {
  id: 42,
  name: "OpenAI",
  description: null,
  platform: "openai",
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: "active",
  subscription_type: "standard",
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  long_context_pricing_enabled: true,
  force_openai_fast: false,
  openai_service_tier_policy: "",
  request_speed_selection_enabled: false,
  default_request_speed_tier: "ultrafast",
  force_openai_ultrafast: false,
  free_openai_fast: false,
  model_pricing: [],
  profit_control_enabled: false,
  profit_min_margin: 0,
  profit_safety_buffer: 0,
  allow_image_generation: false,
  allow_batch_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  batch_image_discount_multiplier: 0.5,
  batch_image_hold_multiplier: 0.6,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  video_rate_independent: false,
  video_rate_multiplier: 1,
  video_price_480p: null,
  video_price_720p: null,
  video_price_1080p: null,
  web_search_price_per_call: null,
  search_price_per_1k: null,
  audio_realtime_price_per_min: null,
  audio_tts_price_per_million_chars: null,
  audio_stt_price_per_hour: null,
  peak_rate_enabled: false,
  peak_start: "",
  peak_end: "",
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  allow_live: false,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: "2026-09-05T00:00:00Z",
  updated_at: "2026-09-05T00:00:00Z",
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: true,
  supported_model_scopes: [],
  account_count: 1,
  active_account_count: 1,
  rate_limited_account_count: 0,
  models_list_config: undefined,
  codex_models_manifest_config: {
    enabled: false,
    account_ids: [],
    fallback_to_scheduler: false,
  },
  sort_order: 10,
} satisfies AdminGroup;

const AppLayoutStub = defineComponent({
  template: "<main><slot /></main>",
});

const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>',
});

const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
  },
  template: '<div><div v-if="data.length"><slot name="cell-actions" :row="data[0]" /></div></div>',
});

const BaseDialogStub = defineComponent({
  props: {
    show: { type: Boolean, default: false },
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
});

// 表单提交前会调用这些子组件的 validate()，自动桩不提供该方法。
const ValidatingFieldStub = defineComponent({
  setup(_, { expose }) {
    expose({ validate: () => true, resetValidation: () => undefined });
    return () => h("div");
  },
});

const mountView = () =>
  mount(GroupsView, {
    global: {
      plugins: [createPinia()],
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        Select: defineComponent({
          props: ["modelValue", "options"],
          emits: ["update:modelValue", "change"],
          template: `<select :value="modelValue" @change="$emit('update:modelValue', $event.target.value); $emit('change', $event.target.value)"><option v-for="option in options" :value="option.value">{{ option.label }}</option></select>`,
        }),
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        ReasoningEffortPolicyFields: ValidatingFieldStub,
        CodexManifestAccountsField: ValidatingFieldStub,
        PricingEntryCard: true,
        VueDraggable: true,
      },
    },
  });

const openEditDialog = async (group: AdminGroup) => {
  listGroups.mockResolvedValue({
    items: [group],
    total: 1,
    page: 1,
    page_size: 20,
    pages: 1,
  });
  const wrapper = mountView();
  await flushPromises();
  const editButton = wrapper
    .findAll("button")
    .find((button) => button.text().includes("common.edit"));
  expect(editButton).toBeTruthy();
  await editButton!.trigger("click");
  await flushPromises();
  return wrapper;
};

describe("GroupsView native request speed", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    getModelAllowlistCandidates.mockResolvedValue([]);
    getUsageSummary.mockResolvedValue([]);
    getCapacitySummary.mockResolvedValue([]);
    getLiveCapability.mockResolvedValue({ supported: false });
    listGroups.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 });
    createGroup.mockResolvedValue({ id: 99 });
    updateGroup.mockResolvedValue({});
  });

  it("keeps fixed tier controls for disabled OpenAI groups", async () => {
    const wrapper = await openEditDialog(sourceGroup);
    expect(wrapper.get('[data-testid="edit-request-speed-enabled"]').attributes('aria-checked')).toBe('false');
    expect(wrapper.find('[data-testid="edit-service-tier-policy"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="edit-force-openai-fast"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="edit-force-openai-ultrafast"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="edit-request-speed-default"]').exists()).toBe(false);
    wrapper.unmount();
  });

  it("hides legacy force flags when an explicit private policy is selected", async () => {
    const wrapper = await openEditDialog({ ...sourceGroup,
      openai_service_tier_policy: 'force_fast', force_openai_ultrafast: true });
    expect(wrapper.find('[data-testid="edit-force-openai-fast"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="edit-force-openai-ultrafast"]').exists()).toBe(false);
    await wrapper.get('[data-testid="edit-service-tier-policy"]').setValue('');
    expect(wrapper.find('[data-testid="edit-force-openai-fast"]').exists()).toBe(true);
    expect(wrapper.get('[data-testid="edit-force-openai-ultrafast"]').attributes('aria-checked')).toBe('true');
    wrapper.unmount();
  });

  it("switches enabled groups to Key mode while preserving pricing and hidden fixed settings", async () => {
    const wrapper = await openEditDialog({ ...sourceGroup, rate_multiplier: 0.15,
      force_openai_fast: true, force_openai_ultrafast: true,
      openai_service_tier_policy: 'force_ultrafast', free_openai_fast: true });
    await wrapper.get('[data-testid="edit-request-speed-enabled"]').trigger('click');
    expect(wrapper.find('[data-testid="edit-service-tier-policy"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="edit-force-openai-fast"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="edit-force-openai-ultrafast"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="edit-free-openai-fast"]').exists()).toBe(true);
    expect(wrapper.text()).toContain('requestSpeed.modeByKey');
    await wrapper.get('[data-testid="edit-request-speed-default"]').setValue('fast');
    await wrapper.get('#edit-group-form').trigger('submit');
    await flushPromises();
    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(updateGroup.mock.calls[0][1]).toMatchObject({
      request_speed_selection_enabled: true, default_request_speed_tier: 'fast',
      force_openai_fast: true, force_openai_ultrafast: true,
      openai_service_tier_policy: 'force_ultrafast', free_openai_fast: true,
      rate_multiplier: 0.15, model_pricing: [],
    });
    wrapper.unmount();
  });

  it("resaves loaded native tier values with a single update", async () => {
    const wrapper = await openEditDialog({ ...sourceGroup,
      request_speed_selection_enabled: true, default_request_speed_tier: 'fast' });
    expect(wrapper.get('[data-testid="edit-request-speed-default"]').element).toHaveProperty('value', 'fast');
    await wrapper.get('#edit-group-form').trigger('submit');
    await flushPromises();
    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(updateGroup.mock.calls[0][1]).toMatchObject({ request_speed_selection_enabled: true, default_request_speed_tier: 'fast' });
    wrapper.unmount();
  });

  it("restores suspended fixed settings when Key mode is disabled without rewriting their values", async () => {
    const wrapper = await openEditDialog({ ...sourceGroup,
      request_speed_selection_enabled: true, default_request_speed_tier: 'ultrafast',
      openai_service_tier_policy: 'force_ultrafast', force_openai_fast: true,
      force_openai_ultrafast: true, free_openai_fast: true });
    await wrapper.get('[data-testid="edit-request-speed-enabled"]').trigger('click');
    expect(wrapper.find('[data-testid="edit-service-tier-policy"]').exists()).toBe(true);
    await wrapper.get('#edit-group-form').trigger('submit');
    await flushPromises();
    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(updateGroup.mock.calls[0][1]).toMatchObject({ request_speed_selection_enabled: false,
      openai_service_tier_policy: 'force_ultrafast', force_openai_fast: true,
      force_openai_ultrafast: true, free_openai_fast: true });
    wrapper.unmount();
  });

  it("creates the native group and its speed setting atomically", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click');
    await wrapper.get('[data-tour="group-form-name"]').setValue('Speed group');
    await wrapper.get('[data-tour="group-form-platform"]').setValue('openai');
    await wrapper.get('[data-testid="create-request-speed-enabled"]').trigger('click');
    await wrapper.get('[data-testid="create-request-speed-default"]').setValue('fast');
    expect(wrapper.find('[data-testid="create-service-tier-policy"]').exists()).toBe(false);
    await wrapper.get('#create-group-form').trigger('submit');
    await flushPromises();
    expect(createGroup).toHaveBeenCalledTimes(1);
    expect(createGroup.mock.calls[0][0]).toMatchObject({ name: 'Speed group', platform: 'openai',
      request_speed_selection_enabled: true, default_request_speed_tier: 'fast', rate_multiplier: 1 });
    expect(updateGroup).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it.each(['composite', 'anthropic'] as const)("does not add Key speed configuration to %s groups", async (platform) => {
    const wrapper = await openEditDialog({ ...sourceGroup, platform });
    expect(wrapper.find('[data-testid="edit-request-speed-enabled"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="edit-force-openai-ultrafast"]').exists()).toBe(platform === 'composite');
    await wrapper.get('#edit-group-form').trigger('submit');
    await flushPromises();
    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(updateGroup.mock.calls[0][1]).not.toHaveProperty('request_speed_selection_enabled');
    expect(updateGroup.mock.calls[0][1]).not.toHaveProperty('default_request_speed_tier');
    wrapper.unmount();
  });

  it("preserves the form and explains why a group with Fast Keys cannot be disabled", async () => {
    updateGroup.mockRejectedValueOnce({ reason: 'REQUEST_SPEED_FAST_KEYS_EXIST', message: '仍有 Key 选择 Fast，请先调整这些 Key 后再关闭' });
    const wrapper = await openEditDialog({ ...sourceGroup, request_speed_selection_enabled: true });
    await wrapper.get('[data-testid="edit-request-speed-enabled"]').trigger('click');
    await wrapper.get('#edit-group-form').trigger('submit');
    await flushPromises();
    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(wrapper.find('#edit-group-form').exists()).toBe(true);
    expect(wrapper.get('[data-testid="edit-request-speed-error"]').text()).toContain('仍有 Key 选择 Fast');
    expect(showError).toHaveBeenCalledWith('仍有 Key 选择 Fast，请先调整这些 Key 后再关闭');
    wrapper.unmount();
  });
});
