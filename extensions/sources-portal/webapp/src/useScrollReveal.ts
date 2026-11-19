import { onBeforeUnmount, onMounted, ref, type Ref } from "vue";

interface ScrollRevealOptions {
  threshold?: number;
  rootMargin?: string;
}

export function useScrollReveal(
  target: Ref<HTMLElement | null>,
  options: ScrollRevealOptions = {},
) {
  const isVisible = ref(false);
  const threshold = options.threshold ?? 0.18;
  const rootMargin = options.rootMargin ?? "0px 0px -12% 0px";
  let observer: IntersectionObserver | null = null;

  onMounted(() => {
    if (
      typeof window === "undefined" ||
      typeof IntersectionObserver === "undefined"
    ) {
      isVisible.value = true;
      return;
    }

    const element = target.value;
    if (!element) return;

    observer = new IntersectionObserver(
      (entries) => {
        const [entry] = entries;
        if (!entry) return;
        if (entry.isIntersecting) {
          isVisible.value = true;
        }
      },
      {
        threshold,
        rootMargin,
      },
    );

    observer.observe(element);
  });

  onBeforeUnmount(() => {
    observer?.disconnect();
    observer = null;
  });

  return {
    isVisible,
  };
}
