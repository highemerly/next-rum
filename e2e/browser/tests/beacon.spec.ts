import { test, expect } from '@playwright/test';

// 計測対象のサイトに <script async src=".../beacon.js"> を1行入れた状態を再現し、beacon.js が
// ブラウザで例外なく動いてビーコンを送るかを見る。中身（ラベル・メトリクス・Loki）は e2e/smoke.sh が見ている。
const COLLECTOR = `http://127.0.0.1:${process.env.E2E_PORT ?? '18480'}`;
// 計測対象のページ。page.route で返すのでサーバーは要らない。コレクタとは別オリジンにして、本番と同じく
// クロスオリジンで読ませる。
const PAGE = 'http://localhost:18999/events/e2e-browser';

// route で返したページは実際のアドレスを持たないので、Chrome の Local Network Access が公開サイト扱いにし、
// 127.0.0.1（コレクタ）への通信を許可なしでは止める。本番は公開サイトから公開の rum.piyo.me を読むので
// この制限に当たらない。テストの構成でだけ起きるので、許可を与えて本番と同じ条件にする。
test.use({ permissions: ['local-network-access'] });

// サイトが CSP を持っていても、コレクタのオリジンを許せば動くこと（unsafe-inline・unsafe-eval が要らないこと）
const CSP = `default-src 'none'; script-src ${COLLECTOR}; connect-src ${COLLECTOR}`;

test('beacon.js loads under a strict CSP and sends the load beacon', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  await page.addInitScript(() => {
    (window as any).__cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => {
      (window as any).__cspViolations.push(`${e.violatedDirective} ${e.blockedURI}`);
    });
  });

  const assetStatus: Record<string, number> = {};
  page.on('response', (r) => {
    const path = new URL(r.url()).pathname;
    if (['/beacon.js', '/rum-config.json', '/web-vitals.js'].includes(path)) assetStatus[path] = r.status();
  });

  await page.route(PAGE, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/html; charset=utf-8',
      headers: { 'Content-Security-Policy': CSP },
      body: `<!doctype html><html><head><title>e2e</title>
<script async src="${COLLECTOR}/beacon.js"></script>
</head><body><h1>e2e browser smoke</h1></body></html>`,
    }),
  );

  // 読み込み時のまとめ送り（beacon.js の WINDOW_MS＝3秒後）を待つ。画面操作はしない。
  const beacon = page.waitForRequest((r) => r.url() === `${COLLECTOR}/collect` && r.method() === 'POST', {
    timeout: 10_000,
  });
  await page.goto(PAGE);
  // 来なかったときは、原因（読み込み・例外・CSP）が分かるよう下の確認を先に通してから落とす
  const req = await beacon.catch(() => null);

  expect(await page.evaluate(() => (window as any).__cspViolations)).toEqual([]);
  expect(pageErrors).toEqual([]);
  expect(assetStatus).toEqual({ '/beacon.js': 200, '/rum-config.json': 200, '/web-vitals.js': 200 });
  expect(req, 'no POST /collect within 10s').not.toBeNull();

  // 1件ならオブジェクト、複数なら配列で来る
  const body = JSON.parse(req!.postData() ?? 'null');
  const events = Array.isArray(body) ? body : [body];
  const nav = events.find((e) => e.kind === 'navigation');
  // localhost は rum-config.json に無いので default_service（unknown）になる
  expect(nav).toMatchObject({ kind: 'navigation', service: 'unknown', path: '/events/e2e-browser', nav_type: 'navigate' });
});
