import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
export default defineConfig({
  plugins: [vue()],
  base: "/home-assets/",
  build: { outDir: "../internal/portal/homeweb", emptyOutDir: true, rollupOptions: { input: ["index.html", "intelligence.html"] } },
  server: {
    proxy: {
      "/public": "http://127.0.0.1:18091",
      "/api": "http://127.0.0.1:18091",
    },
  },
});
