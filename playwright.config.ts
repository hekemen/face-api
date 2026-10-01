import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './test/playwright',
  timeout: 30_000,
  expect: {
    timeout: 10_000,
  },
  use: {
    baseURL: 'http://localhost:8081',
    actionTimeout: 5_000,
  },
  retries: 0,
  reporter: 'list',
});
