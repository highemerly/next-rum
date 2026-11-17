import { test, expect, type Page } from '@playwright/test';

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

// 計測対象のページ（beacon.js を1行入れただけのもの）を PAGE で返す
async function routePage(page: Page) {
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
}

// これまでに送られた /collect のイベント（配列で来たものは展開する）
function collectedEvents(page: Page): any[] {
  const events: any[] = [];
  page.on('request', (r) => {
    if (r.url() !== `${COLLECTOR}/collect` || r.method() !== 'POST') return;
    const body = JSON.parse(r.postData() ?? 'null');
    events.push(...(Array.isArray(body) ? body : [body]));
  });
  return events;
}

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

  await routePage(page);

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

// SPA のルーターと同じく history.pushState / replaceState / 戻る（popstate）で URL だけを変え、
// path が変わるたびに soft_navigation が1件ずつ送られることを見る。
test('beacon.js reports SPA route changes as soft_navigation', async ({ page }) => {
  const events = collectedEvents(page);
  const softNavs = () => events.filter((e) => e.kind === 'soft_navigation').map((e) => e.path);
  await routePage(page);
  await page.goto(PAGE);
  // 読み込み時のまとめ送りが済んでから遷移する（済んだ後は1件ずつすぐ送る）
  await expect.poll(() => events.some((e) => e.kind === 'navigation'), { timeout: 10_000 }).toBe(true);

  // クエリだけの変更はページビューではない。pushState の直後の replaceState（クライアント側の
  // リダイレクト）は、行き着いた先の1件にまとまる。
  await page.evaluate(() => {
    history.replaceState(null, '', location.pathname + '?tab=1');
    history.pushState(null, '', '/events/e2e-redirect');
    history.replaceState(null, '', '/events/e2e-landed');
  });
  await expect.poll(softNavs).toEqual(['/events/e2e-landed']);

  // 戻る（popstate）。最初の path に戻る
  await page.goBack();
  await expect.poll(softNavs).toEqual(['/events/e2e-landed', '/events/e2e-browser']);

  // localhost は rum-config.json に無いので、unknown の既定の path_group（other）になる
  expect(events.find((e) => e.kind === 'soft_navigation')).toMatchObject({
    service: 'unknown',
    path_group: 'other',
    nav_type: 'soft',
  });
});

// features（サービスごとに有効にする信号）を確かめるための設定。localhost を features を持つサービスにする。
// コレクタが配る rum-config.json をこのページの読み込みでだけ差し替える（送られた中身はコレクタではなく
// ここで見るので、コレクタ側の設定は変えなくてよい）。
async function routeConfigWithFeatures(page: Page, features: string[]) {
  await page.route(`${COLLECTOR}/rum-config.json`, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      headers: { 'Access-Control-Allow-Origin': '*' },
      body: JSON.stringify({
        services: { localhost: 'e2e' },
        default_service: 'unknown',
        default_path_group: 'other',
        path_groups: { e2e: [{ pattern: '/events/*', group: 'event' }] },
        features: { e2e: features },
      }),
    }),
  );
}

// ログイン状態を <html data-rum-auth> で教えるページ
async function routePageWithAuth(page: Page, auth: string) {
  await page.route(PAGE, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/html; charset=utf-8',
      headers: { 'Content-Security-Policy': CSP },
      body: `<!doctype html><html data-rum-auth="${auth}"><head><title>e2e</title>
<script async src="${COLLECTOR}/beacon.js"></script>
</head><body><h1>e2e browser smoke</h1></body></html>`,
    }),
  );
}

test('beacon.js sends the referrer host and login state when the service opts in', async ({ page }) => {
  const events = collectedEvents(page);
  await routeConfigWithFeatures(page, ['referrer', 'auth']);
  await routePageWithAuth(page, 'out');
  // 前のページのパスとクエリは送らず、ホスト名だけを送る
  await page.goto(PAGE, { referer: 'https://mastodon.social/@alice/123?x=1' });
  await expect.poll(() => events.some((e) => e.kind === 'navigation'), { timeout: 10_000 }).toBe(true);

  const nav = events.find((e) => e.kind === 'navigation');
  expect(nav).toMatchObject({ service: 'e2e', path_group: 'event', referrer: 'mastodon.social', auth: 'out' });

  // ページ遷移のたびに、その時点のログイン状態を読む（ログインしたらページが属性を書き換える）
  await page.evaluate(() => {
    document.documentElement.setAttribute('data-rum-auth', 'in');
    history.pushState(null, '', '/events/e2e-after-login');
  });
  await expect.poll(() => events.find((e) => e.kind === 'soft_navigation')).toMatchObject({ auth: 'in' });
  // 画面内の移動には外からの参照元が無い
  expect(events.find((e) => e.kind === 'soft_navigation')).not.toHaveProperty('referrer');
});

test('beacon.js sends neither signal when the service has not opted in', async ({ page }) => {
  const events = collectedEvents(page);
  await routeConfigWithFeatures(page, []);
  await routePageWithAuth(page, 'in');
  await page.goto(PAGE, { referer: 'https://mastodon.social/@alice/123' });
  await expect.poll(() => events.some((e) => e.kind === 'navigation'), { timeout: 10_000 }).toBe(true);

  await page.evaluate(() => history.pushState(null, '', '/events/e2e-next'));
  await expect.poll(() => events.some((e) => e.kind === 'soft_navigation')).toBe(true);
  for (const e of events) {
    expect(e).not.toHaveProperty('referrer');
    expect(e).not.toHaveProperty('auth');
  }
});
