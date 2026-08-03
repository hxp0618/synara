// FILE: vitest.browser.config.ts
// Purpose: Run real Chromium checks for the independent Admin surface.
// Layer: Admin browser-test configuration

import { playwright } from "@vitest/browser-playwright";
import { defineConfig, mergeConfig } from "vitest/config";

import viteConfig from "./vite.config";

export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      include: ["src/**/*.browser.tsx"],
      browser: {
        enabled: true,
        provider: playwright(),
        instances: [{ browser: "chromium" }],
        headless: true,
        api: {
          host: process.env.VITEST_BROWSER_API_HOST ?? "127.0.0.1",
          port: Number(process.env.VITEST_BROWSER_API_PORT ?? 51_110),
        },
      },
      testTimeout: 90_000,
      hookTimeout: 90_000,
    },
  }),
);
