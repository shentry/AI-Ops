import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// `npm run dev` proxies backend paths to a locally running oncall-agent so the
// console keeps same-origin cookies. Browser tests intercept these requests
// before they reach the proxy.
const backend = process.env.ONCALL_BACKEND ?? "http://127.0.0.1:18080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // Markdown 解析器体积最大且很少变，单独成块便于缓存。
        manualChunks: {
          react: ["react", "react-dom", "react-dom/client", "react-router"],
          markdown: ["react-markdown", "remark-gfm"],
        },
      },
    },
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    proxy: {
      "/api": { target: backend, changeOrigin: false },
    },
  },
});
