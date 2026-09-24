import { defineConfig } from "@playwright/test";

// Set only to an isolated test deployment; never point this at the live service.
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? "http://127.0.0.1:15173";

export default defineConfig({
  testDir: "./tests",
  fullyParallel: true,
  workers: 4,
  use: { baseURL, browserName: "chromium", viewport: { width: 1440, height: 1000 }, trace: "retain-on-failure" },
  webServer: process.env.PLAYWRIGHT_BASE_URL ? undefined : {
    command: "npm run dev -- --port 15173 --strictPort",
    url: baseURL,
    reuseExistingServer: false,
  },
});
