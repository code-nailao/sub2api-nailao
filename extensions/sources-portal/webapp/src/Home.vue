<template>
  <div
    class="sources-home min-h-screen overflow-x-hidden bg-white text-slate-950 dark:bg-dark-950 dark:text-white"
  >
    <header
      data-testid="home-navbar"
      class="fixed inset-x-0 top-0 z-50 border-b bg-white/90 backdrop-blur-md transition-all duration-300 dark:bg-dark-950/95"
      :class="
        navbarScrolled
          ? 'border-slate-200/70 shadow-sm dark:border-dark-800 dark:shadow-dark-950/40'
          : 'border-slate-100 dark:border-dark-800/70'
      "
    >
      <nav
        class="mx-auto flex h-16 max-w-7xl items-center justify-between px-5 sm:px-6 lg:px-8"
      >
        <a
          :href="mainURL('/home')"
          target="_top"
          class="flex min-w-0 items-center gap-2.5"
        >
          <span
            class="flex h-8 w-8 flex-shrink-0 items-center justify-center overflow-hidden rounded-lg bg-slate-950 shadow-sm dark:bg-white"
          >
            <img
              data-testid="home-logo"
              :src="siteLogo"
              :alt="siteName"
              class="h-full w-full object-cover"
            />
          </span>
          <span
            class="truncate text-lg font-bold tracking-tight text-slate-950 dark:text-white"
            >{{ siteName }}</span
          >
        </a>

        <div class="hidden items-center gap-8 md:flex">
          <a
            :href="mainURL('/home')"
            target="_top"
            class="home-nav-link text-slate-950 dark:text-white"
            >{{ t("home.nav.home") }}</a
          >
          <a
            v-if="sectionVisible.pricing"
            href="#pricing"
            class="home-nav-link"
            >{{ t("home.nav.pricing") }}</a
          >
          <a href="#status" class="home-nav-link">{{ t("home.nav.status") }}</a>
          <a
            v-if="docUrl"
            :href="docUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="home-nav-link"
            >{{ t("home.docs") }}</a
          >
        </div>

        <div class="flex items-center gap-2 sm:gap-3">
          <button
            type="button"
            class="rounded-full p-2 text-slate-500 transition-colors hover:bg-slate-100 hover:text-slate-900 dark:text-dark-300 dark:hover:bg-dark-800 dark:hover:text-white"
            :title="isDark ? t('home.switchToLight') : t('home.switchToDark')"
            @click="toggleTheme"
          >
            <Icon v-if="isDark" name="sun" size="sm" />
            <Icon v-else name="moon" size="sm" />
          </button>
          <a
            :href="dashboardTarget"
            target="_top"
            class="home-console-button"
            data-testid="home-dashboard-link"
          >
            {{ dashboardLabel }}
          </a>
        </div>
      </nav>

      <div
        v-if="announcement.enabled && announcement.text"
        class="border-t border-primary-100 bg-primary-50 px-4 py-2.5 text-primary-700 dark:border-primary-500/20 dark:bg-dark-900/95 dark:text-primary-200"
      >
        <div
          class="mx-auto flex max-w-7xl items-center justify-center gap-2 text-[13px] font-medium"
        >
          <Icon
            name="bell"
            size="sm"
            class="text-primary-500 dark:text-primary-400"
          />
          <span>{{ announcement.text }}</span>
        </div>
      </div>
    </header>

    <main>
      <section
        data-testid="home-hero"
        class="hero-bg relative flex min-h-screen min-h-[100svh] items-center overflow-hidden px-5 pb-20 pt-32 sm:px-6 lg:px-8"
      >
        <div
          class="mx-auto grid w-full max-w-7xl grid-cols-1 items-center gap-14 lg:grid-cols-[minmax(0,1.05fr)_minmax(360px,0.95fr)] lg:gap-16"
        >
          <div class="flex flex-col items-start pt-8 lg:pt-0">
            <p
              class="animate-fade-in-up mb-6 inline-flex items-center rounded-full border border-primary-100 bg-primary-50 px-4 py-1.5 text-sm font-medium text-primary-700 dark:border-primary-800/60 dark:bg-primary-950/40 dark:text-primary-200"
            >
              {{ brand.hero_badge }}
            </p>

            <h1
              class="animate-fade-in-up delay-100 mb-6 text-5xl font-extrabold leading-[1.12] tracking-normal text-slate-950 dark:text-white sm:text-[56px] xl:text-[64px]"
            >
              {{ heroTitlePrefix
              }}<span class="home-title-accent">{{
                heroTitleAccent
              }}</span>
            </h1>

            <p
              class="animate-fade-in-up delay-200 mb-8 text-lg font-medium text-slate-600 dark:text-dark-200 sm:text-xl"
            >
              {{ brand.hero_subtitle }}
            </p>

            <div
              class="animate-fade-in-up delay-200 mb-10 flex flex-wrap items-center gap-3"
            >
              <div
                v-for="highlight in highlights"
                :key="highlight.label"
                class="inline-flex items-center gap-1.5 rounded-full border border-primary-100/70 bg-primary-50 px-3 py-1.5 text-primary-700 dark:border-primary-800/60 dark:bg-primary-950/40 dark:text-primary-200"
              >
                <span
                  class="flex h-4 w-4 items-center justify-center rounded-full bg-primary-600 text-slate-950 dark:bg-primary-500"
                >
                  <Icon name="check" size="xs" :stroke-width="3" />
                </span>
                <span class="text-sm font-medium">{{ highlight.label }}</span>
              </div>
            </div>

            <div
              class="animate-fade-in-up delay-300 mb-12 flex w-full flex-col items-center gap-4 sm:w-auto sm:flex-row"
            >
              <component
                :is="'a'"
                v-bind="primaryCtaBinding"
                class="home-hero-primary"
              >
                {{ brand.primary_cta_label }}
              </component>
              <component
                :is="'a'"
                v-bind="secondaryCtaBinding"
                class="home-hero-secondary"
              >
                {{ brand.secondary_cta_label }}
              </component>
            </div>

            <div
              v-if="sectionVisible.contacts"
              class="animate-fade-in-up delay-300 flex w-full flex-col gap-4 sm:w-auto sm:flex-row"
            >
              <component
                v-for="contact in contacts"
                :key="`${contact.name}-${contact.account}`"
                :is="contact.url ? 'a' : 'div'"
                v-bind="
                  contact.url
                    ? {
                        href: contact.url,
                        target: '_blank',
                        rel: 'noopener noreferrer',
                      }
                    : {}
                "
                class="contact-card group"
              >
                <span
                  class="flex h-10 w-10 items-center justify-center overflow-hidden rounded-full bg-slate-50 text-slate-500 ring-1 ring-slate-100 dark:bg-dark-800 dark:ring-dark-700"
                >
                  <img
                    v-if="contact.logo"
                    :src="contact.logo"
                    :alt="contact.name"
                    class="h-full w-full object-contain"
                  />
                  <Icon v-else name="chat" size="md" />
                </span>
                <span class="min-w-0 flex-1 pr-1">
                  <span
                    class="block truncate text-sm font-bold text-slate-900 dark:text-white"
                    >{{ contact.name }}</span
                  >
                  <span
                    class="block truncate text-xs text-slate-500 dark:text-dark-300"
                    >{{ contact.account }}</span
                  >
                </span>
                <span
                  class="qr-trigger relative flex h-10 w-10 items-center justify-center rounded-lg border border-slate-100 bg-white dark:border-dark-700 dark:bg-dark-900"
                  tabindex="0"
                >
                  <img
                    v-if="contact.qr_image"
                    :src="contact.qr_image"
                    :alt="`${contact.name} QR`"
                    class="h-6 w-6 object-contain opacity-70"
                  />
                  <Icon v-else name="grid" size="lg" class="text-slate-400" />
                  <span
                    v-if="contact.qr_image"
                    class="qr-preview"
                    aria-hidden="true"
                  >
                    <img
                      :src="contact.qr_image"
                      :alt="`${contact.name} QR preview`"
                      class="h-full w-full rounded-xl object-contain"
                    />
                  </span>
                </span>
              </component>
            </div>
          </div>

          <div
            class="animate-fade-in-up delay-200 relative flex w-full justify-center lg:justify-end"
          >
            <div
              class="absolute left-1/2 top-1/2 h-[110%] w-[110%] -translate-x-1/2 -translate-y-1/2 rounded-full bg-primary-400/15 blur-[60px] dark:bg-primary-500/10"
              aria-hidden="true"
            ></div>
            <div
              class="relative z-10 flex w-full max-w-[400px] flex-col items-center rounded-[32px] border border-slate-100/80 bg-white p-8 shadow-[0_20px_60px_-15px_rgba(0,0,0,0.12)] dark:border-dark-700 dark:bg-dark-900 sm:p-10"
            >
              <button
                type="button"
                class="scene relative z-10 mb-10 h-48 w-48 cursor-pointer overflow-visible"
                aria-label="打开福利中心"
                @mousemove="handleCalendarMousemove"
                @mouseleave="resetCalendarTilt"
                @click="openWelfare"
              >
                <span
                  class="calendar-shadow absolute -bottom-6 left-1/2 h-8 w-40 -translate-x-1/2 scale-y-50 rounded-[100%] bg-primary-500/20 blur-xl"
                ></span>
                <span
                  ref="calendarPressWrapRef"
                  class="calendar-press-wrap block h-full w-full"
                >
                  <span
                    ref="calendarRef"
                    class="calendar-3d block h-full w-full"
                  >
                    <span
                      class="absolute inset-0 rounded-[24px]"
                      style="
                        transform: translateZ(-5px);
                        background: linear-gradient(
                          to bottom,
                          var(--color-primary-700) 35%,
                          #f1f5f9 35%
                        );
                      "
                    ></span>
                    <span
                      class="absolute inset-0 rounded-[24px] shadow-md"
                      style="
                        transform: translateZ(-10px);
                        background: linear-gradient(
                          to bottom,
                          var(--color-primary-800) 35%,
                          #e2e8f0 35%
                        );
                      "
                    ></span>
                    <span
                      class="absolute inset-0 flex rotate-y-back flex-col overflow-hidden rounded-[24px] border border-slate-200 bg-white shadow-2xl dark:border-dark-700 dark:bg-dark-900"
                    >
                      <span
                        class="flex h-[35%] items-start justify-around bg-gradient-to-b from-primary-700 to-primary-600 px-6 pt-3 shadow-inner"
                      >
                        <span
                          class="-mt-5 h-8 w-3 rounded-full border-2 border-white/60 bg-gradient-to-b from-slate-200 to-slate-300"
                        ></span>
                        <span
                          class="-mt-5 h-8 w-3 rounded-full border-2 border-white/60 bg-gradient-to-b from-slate-200 to-slate-300"
                        ></span>
                      </span>
                      <span
                        class="flex h-[65%] flex-col items-center justify-center bg-primary-50/70 dark:bg-primary-950/30"
                      >
                        <span
                          class="mb-3 flex h-12 w-12 items-center justify-center rounded-xl border border-primary-100 bg-white shadow-sm dark:border-primary-800 dark:bg-dark-800"
                        >
                          <span
                            class="text-2xl font-black italic text-primary-500"
                            >S</span
                          >
                        </span>
                        <span
                          class="mb-1.5 h-1.5 w-16 rounded-full bg-primary-200/70"
                        ></span>
                        <span
                          class="h-1.5 w-10 rounded-full bg-primary-100/80"
                        ></span>
                      </span>
                    </span>
                    <span
                      class="absolute inset-0 flex flex-col overflow-hidden rounded-[24px] border border-slate-100 bg-white shadow-lg dark:border-dark-700 dark:bg-dark-900"
                      style="transform: translateZ(0px)"
                    >
                      <span
                        class="flex h-[35%] items-start justify-around bg-gradient-to-b from-primary-500 to-primary-700 px-6 pt-3 shadow-inner"
                      >
                        <span
                          class="-mt-4 h-8 w-3.5 rounded-full border-2 border-white bg-gradient-to-b from-slate-100 to-slate-300 shadow-[0_4px_4px_rgba(0,0,0,0.2)]"
                        ></span>
                        <span
                          class="-mt-4 h-8 w-3.5 rounded-full border-2 border-white bg-gradient-to-b from-slate-100 to-slate-300 shadow-[0_4px_4px_rgba(0,0,0,0.2)]"
                        ></span>
                      </span>
                      <span
                        class="grid h-[65%] grid-cols-4 gap-2 bg-white p-4 dark:bg-dark-900"
                      >
                        <span
                          v-for="index in 8"
                          :key="index"
                          class="rounded bg-slate-100/80 dark:bg-dark-800"
                          :class="
                            index === 6
                              ? 'relative flex items-center justify-center border border-primary-200 bg-primary-50 dark:border-primary-700 dark:bg-primary-950/50'
                              : ''
                          "
                        >
                          <span
                            v-if="index === 6"
                            class="h-2.5 w-2.5 rounded-full bg-primary-500 shadow-sm"
                          ></span>
                        </span>
                      </span>
                    </span>
                    <span
                      ref="badgeRef"
                      class="check-badge absolute -bottom-4 -right-4 flex h-16 w-16 items-center justify-center rounded-full border-[4px] border-white bg-gradient-to-tr from-primary-600 to-primary-400 shadow-[0_10px_25px_rgba(0,0,0,0.18)] dark:border-dark-900"
                      style="transform: translateZ(35px)"
                    >
                      <Icon
                        name="check"
                        size="xl"
                        :stroke-width="4"
                        class="text-white"
                      />
                    </span>
                    <Icon
                      name="sparkles"
                      size="lg"
                      class="absolute -left-6 top-4 text-primary-300 drop-shadow-sm"
                      style="transform: translateZ(20px)"
                    />
                    <Icon
                      name="sparkles"
                      size="sm"
                      class="absolute -right-2 top-14 text-primary-200 drop-shadow-sm"
                      style="transform: translateZ(-10px)"
                    />
                  </span>
                </span>
              </button>

              <h3
                class="mb-2 text-2xl font-bold text-slate-900 dark:text-white"
              >
                每日签到
              </h3>
              <p
                class="mb-8 text-center text-[15px] text-slate-500 dark:text-dark-300"
              >
                签到与活动统一在福利中心查看
              </p>
              <a
                id="sign-in-btn"
                :href="mainURL(config.welfare_path)"
                target="_top"
                class="sign-in-button group text-center"
              >
                <span class="relative z-10">前往福利中心</span>
                <span
                  class="button-shimmer absolute top-0 -left-[100%] h-full w-1/2 skew-x-[-20deg] bg-gradient-to-r from-transparent via-white/20 to-transparent"
                ></span>
              </a>
              <p class="mt-4 text-[13px] text-slate-400 dark:text-dark-400">
                活动详情以福利中心为准
              </p>
            </div>
          </div>
        </div>
      </section>

      <section
        v-if="showExperienceSection"
        id="experience"
        ref="experienceSectionRef"
        class="home-reveal-section border-t border-slate-100 bg-[#FAFAFA] py-24 dark:border-dark-800 dark:bg-dark-900/40"
        :class="sectionRevealClass(experienceVisible)"
      >
        <div class="mx-auto max-w-7xl px-5 sm:px-6 lg:px-8">
          <div
            v-if="sectionVisible.features"
            class="home-reveal-item mb-16 text-center"
            :class="itemRevealClass(experienceVisible, 0)"
          >
            <h2
              class="text-3xl font-bold tracking-normal text-slate-950 dark:text-white sm:text-4xl"
            >
              {{ brand.experience_title }}
            </h2>
          </div>

          <div
            v-if="sectionVisible.features"
            class="grid grid-cols-1 gap-6 md:grid-cols-2 lg:grid-cols-4"
          >
            <article
              v-for="(feature, index) in brand.features"
              :key="`${feature.icon}-${feature.title}`"
              class="home-reveal-item group rounded-2xl border border-slate-200/70 bg-white p-8 shadow-sm transition-all duration-300 hover:border-primary-200 hover:shadow-lg hover:shadow-primary-500/5 dark:border-dark-700 dark:bg-dark-800/70 dark:hover:border-primary-800"
              :class="
                itemRevealClass(experienceVisible, Math.min(index + 1, 3))
              "
            >
              <div
                class="mb-6 flex h-12 w-12 items-center justify-center rounded-xl border border-slate-100 bg-slate-50 transition-all duration-300 group-hover:border-primary-100 group-hover:bg-primary-50 dark:border-dark-700 dark:bg-dark-900 dark:group-hover:border-primary-800 dark:group-hover:bg-primary-950/40"
              >
                <Icon
                  :name="feature.icon"
                  size="lg"
                  class="text-slate-800 transition-colors group-hover:text-primary-600 dark:text-white dark:group-hover:text-primary-300"
                />
              </div>
              <h3
                class="mb-3 text-lg font-semibold text-slate-950 dark:text-white"
              >
                {{ feature.title }}
              </h3>
              <p
                class="text-sm leading-relaxed text-slate-500 dark:text-dark-300"
              >
                {{ feature.description }}
              </p>
            </article>
          </div>

          <div
            v-if="sectionVisible.stats"
            class="grid grid-cols-1 gap-12 border-t border-slate-200/70 pt-16 text-center dark:border-dark-700 md:grid-cols-3"
            :class="sectionVisible.features ? 'mt-20' : ''"
          >
            <div
              v-for="stat in compactStats"
              :key="`${stat.value}-${stat.label}`"
              class="home-reveal-item"
              :class="itemRevealClass(experienceVisible, 3)"
            >
              <div
                class="mb-4 text-[36px] font-bold tracking-normal text-slate-900 dark:text-white sm:text-[40px]"
              >
                {{ stat.value }}
              </div>
              <div
                class="text-[14px] font-medium leading-relaxed text-slate-500 dark:text-dark-300 sm:text-[15px]"
              >
                {{ stat.label }}
              </div>
            </div>
          </div>
        </div>
      </section>

      <section
        v-if="sectionVisible.pricing"
        id="pricing"
        ref="pricingSectionRef"
        class="home-reveal-section mx-auto max-w-7xl px-5 py-20 sm:px-6 lg:px-8"
        :class="sectionRevealClass(pricingVisible)"
      >
        <div
          class="home-reveal-item mb-12 text-center"
          :class="itemRevealClass(pricingVisible, 0)"
        >
          <h2
            class="mb-4 text-3xl font-bold tracking-normal text-slate-950 dark:text-white sm:text-4xl"
          >
            {{ t("home.pricing.title") }}
          </h2>
          <p
            class="inline-block rounded-full border border-slate-100 bg-slate-50 px-4 py-2 text-xs tracking-wide text-slate-500 dark:border-dark-700 dark:bg-dark-900 dark:text-dark-300"
          >
            {{ t("home.pricing.note") }}
          </p>
        </div>

        <div
          class="home-reveal-item overflow-hidden rounded-xl border border-slate-200/70 bg-[#F8F9FA] p-4 shadow-sm dark:border-dark-700 dark:bg-dark-900 sm:p-8"
          :class="itemRevealClass(pricingVisible, 1)"
        >
          <div class="overflow-x-auto">
            <table class="w-full min-w-[900px] border-collapse text-left">
              <thead>
                <tr class="border-b border-slate-200/80 dark:border-dark-700">
                  <th
                    v-for="heading in pricingHeadings"
                    :key="heading"
                    class="px-4 py-4 text-[15px] font-semibold text-slate-900 dark:text-white"
                  >
                    {{ heading }}
                  </th>
                </tr>
              </thead>
              <tbody class="text-[15px] text-slate-700 dark:text-dark-200">
                <tr
                  v-for="row in pricingRows"
                  :key="`${row.model}-${row.spec}-${row.input}-${row.output}`"
                  class="border-b border-slate-200/60 transition-colors last:border-0 hover:bg-black/[0.02] dark:border-dark-700 dark:hover:bg-white/[0.03]"
                >
                  <td
                    class="px-4 py-5 font-medium text-slate-900 dark:text-white"
                  >
                    {{ row.model }}
                    <span
                      v-if="row.spec"
                      class="ml-1 inline-flex rounded border border-slate-200 bg-slate-100 px-1.5 py-0.5 text-[11px] font-mono font-medium text-slate-500 dark:border-dark-700 dark:bg-dark-800 dark:text-dark-300"
                      >{{ row.spec }}</span
                    >
                  </td>
                  <td class="px-4 py-5">{{ row.group }}</td>
                  <td class="px-4 py-5">{{ row.multiplier }}</td>
                  <td class="px-4 py-5">{{ row.input }}</td>
                  <td class="px-4 py-5">{{ row.output }}</td>
                  <td class="px-4 py-5">{{ row.official }}</td>
                  <td class="px-4 py-5">
                    <span
                      class="rounded-md border border-primary-200 bg-primary-50 px-2.5 py-1 text-xs font-medium tracking-wide text-primary-700 dark:border-primary-800 dark:bg-primary-950/40 dark:text-primary-200"
                      >{{ row.discount }}</span
                    >
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </section>
    </main>

    <footer
      id="status"
      ref="statusSectionRef"
      class="home-reveal-section border-t border-slate-200 bg-[#FAFAFA] dark:border-dark-800 dark:bg-dark-900/40"
      :class="sectionRevealClass(statusVisible)"
    >
      <div class="mx-auto max-w-[1200px] px-5 pb-12 pt-16 sm:px-6">
        <div
          class="home-reveal-item grid grid-cols-2 gap-x-4 gap-y-12 md:grid-cols-3 lg:grid-cols-6"
          :class="itemRevealClass(statusVisible, 0)"
        >
          <div v-for="group in visibleFooterGroups" :key="group.title">
            <h4 class="mb-6 text-sm font-bold text-slate-900 dark:text-white">
              {{ group.title }}
            </h4>
            <ul class="space-y-4 text-[14px] text-slate-500 dark:text-dark-300">
              <li
                v-for="link in group.links"
                :key="`${group.title}-${link.label}`"
              >
                <component
                  :is="'a'"
                  v-bind="footerLinkBinding(link.url)"
                  class="transition-colors hover:text-primary-600 dark:hover:text-primary-300"
                >
                  {{ link.label }}
                </component>
              </li>
            </ul>
          </div>
        </div>

        <div
          class="home-reveal-item mt-20 flex flex-col gap-4 border-t border-slate-200 pt-8 text-xs text-slate-400 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between"
          :class="itemRevealClass(statusVisible, 1)"
        >
          <p>
            © {{ currentYear }} {{ siteName }}.
            {{ t("home.footer.allRightsReserved") }}
          </p>
          <div class="flex gap-4">
            <a
              :href="defaultTelegramUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="hover:text-primary-600 dark:hover:text-primary-300"
              >Telegram</a
            >
            <a
              v-if="docUrl"
              :href="docUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="hover:text-primary-600 dark:hover:text-primary-300"
              >{{ t("home.docs") }}</a
            >
          </div>
        </div>
      </div>
    </footer>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import Icon from "./Icon.vue";
import { appearance } from "../../../sources-brand/appearance";
import { useScrollReveal } from "./useScrollReveal";
import type { HomeConfig } from "./types";
const props = defineProps<{ config: HomeConfig; mainOrigin: string }>();
const brand = computed(() => props.config.brand);
const config = computed(() => props.config);
const theme = appearance();
const isDark = ref(theme.state.isDark);
const unsubscribeTheme = theme.subscribe(state => { isDark.value = state.isDark; });
onBeforeUnmount(unsubscribeTheme);
const navbarScrolled = ref(false);
const calendarPressWrapRef = ref<HTMLElement | null>(null);
const calendarRef = ref<HTMLElement | null>(null);
const badgeRef = ref<HTMLElement | null>(null);
const experienceSectionRef = ref<HTMLElement | null>(null);
const pricingSectionRef = ref<HTMLElement | null>(null);
const statusSectionRef = ref<HTMLElement | null>(null);
const { isVisible: experienceVisible } = useScrollReveal(experienceSectionRef);
const { isVisible: pricingVisible } = useScrollReveal(pricingSectionRef);
const { isVisible: statusVisible } = useScrollReveal(statusSectionRef);
const siteName = computed(() => props.config.site_name);
const siteLogo = computed(() =>
  isDark.value ? props.config.logo_dark : props.config.logo_light,
);
const docUrl = computed(() => props.config.doc_url);
const announcement = computed(() => brand.value.announcement);
const highlights = computed(() => brand.value.highlights);
const contacts = computed(() => brand.value.contacts);
const sectionVisible = computed(() => brand.value.sections);
const showExperienceSection = computed(
  () => sectionVisible.value.features || sectionVisible.value.stats,
);
const compactStats = computed(() => brand.value.stats.slice(0, 3));
const visibleFooterGroups = computed(() => brand.value.footer_groups);
const pricingHeadings = computed(() => brand.value.pricing_table.headings);
const pricingRows = computed(() => brand.value.pricing_table.rows);
const defaultTelegramUrl = computed(
  () => contacts.value.find((c) => c.name.includes("Telegram"))?.url || "",
);
const heroTitleAccent = computed(() =>
  brand.value.hero_title.endsWith("一键接入") ? "一键接入" : "",
);
const heroTitlePrefix = computed(() =>
  heroTitleAccent.value
    ? brand.value.hero_title.slice(0, -4)
    : brand.value.hero_title,
);
const currentYear = new Date().getFullYear();
const dashboardTarget = computed(() => mainURL("/dashboard"));
const dashboardLabel = "控制台";
const text: Record<string, string> = {
  "home.nav.home": "首页",
  "home.nav.pricing": "价格",
  "home.nav.status": "服务状态",
  "home.docs": "文档",
  "home.switchToLight": "切换浅色",
  "home.switchToDark": "切换深色",
  "home.footer.allRightsReserved": "保留所有权利。",
  "home.pricing.title": "透明定价",
  "home.pricing.note": "实际价格以控制台分组与模型价格为准",
};
const t = (key: string) => text[key] || key;
function mainURL(path: string) {
  return new URL(path || "/dashboard", props.mainOrigin).href;
}
function linkBinding(path: string) {
  return path.startsWith("#")
    ? { href: path }
    : { href: mainURL(path), target: "_top", rel: "noopener" };
}
const primaryCtaBinding = computed(() =>
  linkBinding(brand.value.primary_cta_url || "/dashboard"),
);
const secondaryCtaBinding = computed(() =>
  linkBinding(brand.value.secondary_cta_url || "#pricing"),
);
const footerLinkBinding = linkBinding;
function openWelfare() {
  window.open(mainURL(props.config.welfare_path), "_top");
}
function toggleTheme() { theme.toggleDark(); }
const sectionRevealClass = (visible: boolean) =>
  visible ? "home-reveal-visible" : "home-reveal-hidden";
const itemRevealClass = (visible: boolean, index: number) => [
  sectionRevealClass(visible),
  `home-reveal-delay-${index}`,
];
function handleCalendarMousemove(event: MouseEvent) {
  const el = calendarPressWrapRef.value;
  if (!el) return;
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
  el.style.setProperty(
    "--tilt-x",
    `${(-(event.clientY - rect.top - rect.height / 2) / (rect.height / 2)) * 18}deg`,
  );
  el.style.setProperty(
    "--tilt-y",
    `${((event.clientX - rect.left - rect.width / 2) / (rect.width / 2)) * 18}deg`,
  );
  el.classList.add("is-tracking");
}
function resetCalendarTilt() {
  const el = calendarPressWrapRef.value;
  if (!el) return;
  el.style.setProperty("--tilt-x", "0deg");
  el.style.setProperty("--tilt-y", "0deg");
  el.classList.remove("is-tracking");
}
function updateNavbarState() {
  navbarScrolled.value = window.scrollY > 20;
}
onMounted(() => {
  updateNavbarState();
  window.addEventListener("scroll", updateNavbarState, { passive: true });
});
onBeforeUnmount(() => window.removeEventListener("scroll", updateNavbarState));
</script>
<style scoped>
.home-title-accent {
  color: var(--color-primary-600);
}

.dark .home-title-accent {
  color: var(--color-primary-400);
}

.home-nav-link {
  @apply text-sm font-medium text-slate-500 transition-colors hover:text-primary-600 dark:text-dark-300 dark:hover:text-primary-300;
}

.home-console-button {
  @apply inline-flex items-center rounded-full border border-slate-200 bg-white px-5 py-2 text-sm font-medium text-slate-700 transition-all duration-300 hover:border-slate-300 hover:bg-slate-50 active:scale-95 dark:border-dark-700 dark:bg-dark-900 dark:text-dark-100 dark:hover:bg-dark-800;
}

.hero-bg {
  background:
    radial-gradient(
      circle at 80% 20%,
      color-mix(in srgb, var(--color-primary-500) 10%, transparent) 0%,
      transparent 40%
    ),
    radial-gradient(
      circle at 20% 80%,
      color-mix(in srgb, var(--color-primary-500) 4%, transparent) 0%,
      transparent 40%
    );
}

.home-hero-primary,
.home-hero-secondary {
  @apply flex w-full items-center justify-center rounded-xl px-10 py-3.5 text-base font-medium transition-all duration-300 sm:w-auto;
}

.home-hero-primary {
  color: var(--color-primary-600-contrast);
  @apply bg-primary-600 hover:-translate-y-0.5 hover:bg-primary-700 hover:shadow-lg hover:shadow-primary-500/25 dark:bg-primary-500 dark:hover:bg-primary-400;
}

.home-hero-primary:hover, .sign-in-button:hover { color: var(--color-primary-700-contrast); }
.dark .home-hero-primary, .dark .sign-in-button { color: var(--color-primary-500-contrast); }
.dark .home-hero-primary:hover, .dark .sign-in-button:hover { color: var(--color-primary-400-contrast); }

.home-hero-secondary {
  @apply border border-slate-200 bg-white text-primary-700 hover:border-primary-300 hover:bg-primary-50 dark:border-dark-700 dark:bg-dark-900 dark:text-primary-300 dark:hover:bg-primary-950/30;
}

.contact-card {
  @apply relative flex items-center gap-4 rounded-2xl border border-slate-100 bg-white p-4 shadow-sm transition-all duration-300 hover:-translate-y-0.5 hover:shadow-md dark:border-dark-700 dark:bg-dark-900;
}

.qr-preview {
  position: absolute;
  right: 0;
  bottom: calc(100% + 10px);
  z-index: 60;
  width: 132px;
  height: 132px;
  padding: 8px;
  border-radius: 18px;
  border: 1px solid rgb(226 232 240);
  background: white;
  box-shadow: 0 24px 60px rgba(15, 23, 42, 0.18);
  opacity: 0;
  pointer-events: none;
  transform: translateY(6px) scale(0.92);
  transform-origin: bottom right;
  transition:
    opacity 180ms ease,
    transform 220ms cubic-bezier(0.22, 1, 0.36, 1);
}

.contact-card:hover .qr-preview,
.contact-card:focus-within .qr-preview {
  opacity: 1;
  transform: translateY(0) scale(1);
}

.scene {
  perspective: 1000px;
}

.calendar-press-wrap {
  --tilt-x: 0deg;
  --tilt-y: 0deg;
  --push-y: 0px;
  --scale: 1;
  transform: translateY(var(--push-y)) scale(var(--scale))
    rotateX(var(--tilt-x)) rotateY(var(--tilt-y));
  transition: transform 0.6s cubic-bezier(0.34, 1.56, 0.64, 1);
  transform-style: preserve-3d;
  will-change: transform;
}

.calendar-press-wrap.is-tracking {
  transition: transform 0.1s ease-out;
}

.scene:hover .calendar-press-wrap {
  --push-y: 4px;
  --scale: 0.98;
}

.scene:active .calendar-press-wrap {
  --push-y: 12px;
  --scale: 0.94;
  transition: transform 0.1s ease-out;
}

.calendar-shadow {
  transition: all 0.6s cubic-bezier(0.34, 1.56, 0.64, 1);
}

.scene:hover .calendar-shadow {
  width: 7.5rem;
  background-color: color-mix(
    in srgb,
    var(--color-primary-500) 35%,
    transparent
  );
  transition: all 0.4s cubic-bezier(0.2, 0.8, 0.2, 1);
}

.scene:active .calendar-shadow {
  width: 6.5rem;
  background-color: color-mix(
    in srgb,
    var(--color-primary-500) 45%,
    transparent
  );
  transition: all 0.15s ease-out;
}

.calendar-3d {
  position: relative;
  transform-style: preserve-3d;
  animation: float-3d-rotate 6s ease-in-out infinite;
}

.rotate-y-back {
  transform: translateZ(-15px) rotateY(180deg);
}

.sign-in-button {
  color: var(--color-primary-600-contrast);
  @apply relative w-full overflow-hidden rounded-xl bg-primary-600 py-3.5 text-[17px] font-bold shadow-[0_8px_20px_-6px_rgba(0,0,0,0.28)] transition-all duration-300 hover:-translate-y-1 hover:bg-primary-700 hover:shadow-[0_12px_24px_-8px_rgba(0,0,0,0.32)] active:scale-[0.98] disabled:cursor-not-allowed disabled:opacity-70 disabled:hover:translate-y-0 dark:bg-primary-500 dark:hover:bg-primary-400;
}

.sign-in-button:hover .button-shimmer {
  animation: shimmer 1.5s infinite;
}

.home-reveal-section,
.home-reveal-item {
  will-change: opacity, transform;
}

.home-reveal-hidden {
  opacity: 0;
  transform: translateY(28px);
}

.home-reveal-visible {
  opacity: 1;
  transform: translateY(0);
}

.home-reveal-section {
  opacity: 1;
  transform: none;
}

.home-reveal-item {
  transition:
    opacity 620ms cubic-bezier(0.22, 1, 0.36, 1),
    transform 620ms cubic-bezier(0.22, 1, 0.36, 1),
    box-shadow 300ms ease,
    border-color 300ms ease;
}

.home-reveal-delay-0 {
  transition-delay: 0ms;
}
.home-reveal-delay-1 {
  transition-delay: 90ms;
}
.home-reveal-delay-2 {
  transition-delay: 160ms;
}
.home-reveal-delay-3 {
  transition-delay: 220ms;
}

.animate-fade-in-up {
  opacity: 0;
  animation: fade-in-up 0.8s cubic-bezier(0.16, 1, 0.3, 1) forwards;
}

.delay-100 {
  animation-delay: 100ms;
}
.delay-200 {
  animation-delay: 200ms;
}
.delay-300 {
  animation-delay: 300ms;
}

@keyframes fade-in-up {
  from {
    opacity: 0;
    transform: translateY(20px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}

@keyframes float-3d-rotate {
  0%,
  100% {
    transform: translateY(0) rotateX(10deg) rotateY(-15deg);
  }
  50% {
    transform: translateY(-20px) rotateX(20deg) rotateY(-5deg);
  }
}

@keyframes shimmer {
  100% {
    left: 200%;
  }
}

@media (prefers-reduced-motion: reduce) {
  .home-reveal-section,
  .home-reveal-item,
  .animate-fade-in-up {
    opacity: 1;
    transform: none;
    transition-duration: 1ms;
    transition-delay: 0ms;
    animation-duration: 1ms;
    animation-iteration-count: 1;
  }

  .calendar-3d {
    animation: none;
  }
}
</style>
