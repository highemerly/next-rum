import { defineConfig, devices } from '@playwright/test';

// ブラウザでの確認は beacon.js が実際に動くかだけを見る（docs/development.md「E2E スモーク」）。
// コレクタは compose.yaml で起動しておく。リトライはしない（落ちたら原因を見る）。
export default defineConfig({
  testDir: './tests',
  // 既定（test-results/）だと実行した場所に散らばるので、ここに固定する
  outputDir: './test-results',
  retries: 0,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  use: {
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
