// rum.piyo.me/beacon.js
// Loader for Real User Monitoring. Single line to deploy on every service:
//   <script async src="https://rum.piyo.me/beacon.js"></script>
// Service identification and URL grouping come from rum-config.json (single source).
(function () {
  // GPC (Global Privacy Control) preference: skip measurement entirely.
  if (navigator.globalPrivacyControl === true) return;

  // document.currentScript is only valid during initial synchronous execution.
  var SELF = document.currentScript;

  // Derive the collector base from this script's own URL so the same file works
  // in production (https://rum.piyo.me/beacon.js), locally
  // (http://localhost:8080/beacon.js), and behind a path prefix on the measured
  // site itself (https://hosteka.piyo.me/_n-rum/beacon.js, routed to the collector
  // by the site's Ingress with the prefix stripped). The base is the script's
  // directory, not its origin: resolving against the origin would send the
  // prefixed case's config fetch and beacons to the site instead of the collector.
  // Falls back to the production collector.
  var BASE = (function () {
    try { return new URL('./', SELF.src).href; } catch (e) { return 'https://rum.piyo.me/'; }
  })();
  var CONFIG_URL  = BASE + 'rum-config.json';
  var VITALS_URL  = BASE + 'web-vitals.js';
  var COLLECT_URL = BASE + 'collect';

  // ---- glob (for config path_groups) ----
  // * matches a single path segment ([^/]*); ** matches across segments (.*).
  function globToRegExp(glob) {
    var re = glob.replace(/[.+?^${}()|[\]\\]/g, '\\$&'); // escape regex specials (keep *)
    re = re.replace(/\*\*/g, '\x01DBLSTAR\x01');         // ** -> marker (survives * pass)
    re = re.replace(/\*/g, '[^/]*');                     // *  -> one segment
    re = re.replace(/\x01DBLSTAR\x01/g, '.*');           // ** -> any segments
    return new RegExp('^' + re + '$');
  }

  function resolveService(cfg) {
    var host = location.hostname;
    if (cfg.services && cfg.services[host]) return cfg.services[host];
    var attr = SELF && SELF.dataset ? SELF.dataset.service : null; // optional fallback
    return attr || cfg.default_service || 'unknown';
  }

  function resolvePathGroup(cfg, service) {
    var rules = (cfg.path_groups && (cfg.path_groups[service] || cfg.path_groups['*'])) || [];
    var p = location.pathname;
    for (var i = 0; i < rules.length; i++) {
      if (globToRegExp(rules[i].pattern).test(p)) return rules[i].group;
    }
    return cfg.default_path_group || 'other';
  }

  // ---- Navigation Timing ----
  function getNavTimings() {
    var nav = performance.getEntriesByType('navigation')[0];
    if (!nav) return {};
    return {
      dns_ms:      Math.round(nav.domainLookupEnd - nav.domainLookupStart),
      tcp_ms:      Math.round(nav.connectEnd - nav.connectStart),
      tls_ms:      nav.secureConnectionStart > 0
                     ? Math.round(nav.connectEnd - nav.secureConnectionStart) : 0,
      request_ms:  Math.round(nav.responseStart - nav.requestStart),
      response_ms: Math.round(nav.responseEnd - nav.responseStart), // transfer time
      transfer_size: nav.transferSize || 0,
      encoded_size:  nav.encodedBodySize || 0,
      decoded_size:  nav.decodedBodySize || 0,
      protocol:      nav.nextHopProtocol || '',
      nav_type:      nav.type || 'navigate'
    };
  }

  // ---- Cloudflare cache info (via Server-Timing, same-origin) ----
  function getCFInfo() {
    var nav = performance.getEntriesByType('navigation')[0];
    if (!nav || !nav.serverTiming) return {};
    var out = {};
    for (var i = 0; i < nav.serverTiming.length; i++) {
      var st = nav.serverTiming[i];
      if (st.name === 'cf-cache') out.cf_cache = st.description || '';
      if (st.name === 'cf-colo')  out.cf_colo  = st.description || '';
    }
    return out;
  }

  function getDevice() {
    if (navigator.userAgentData) return navigator.userAgentData.mobile ? 'mobile' : 'desktop';
    return /Mobile|Android|iPhone/.test(navigator.userAgent) ? 'mobile' : 'desktop';
  }
  function getConnection() {
    return (navigator.connection && navigator.connection.effectiveType) || '';
  }

  // Coarse OS name (no version). Prefers the low-entropy UA-CH platform hint,
  // falls back to UA parsing for browsers without userAgentData (Firefox/Safari).
  function getOS() {
    var uaData = navigator.userAgentData;
    var p = (uaData && uaData.platform) ? uaData.platform.toLowerCase() : '';
    if (p.indexOf('win') >= 0) return 'windows';
    if (p.indexOf('mac') >= 0) return 'macos';
    if (p.indexOf('android') >= 0) return 'android';
    if (p.indexOf('chrome os') >= 0 || p.indexOf('cros') >= 0) return 'chromeos';
    if (p.indexOf('linux') >= 0) return 'linux';
    var ua = navigator.userAgent;
    if (/Windows/.test(ua)) return 'windows';
    if (/Android/.test(ua)) return 'android';
    if (/iPhone|iPad|iPod/.test(ua)) return 'ios';
    if (/Mac OS X|Macintosh/.test(ua)) return 'macos';
    if (/CrOS/.test(ua)) return 'chromeos';
    if (/Linux/.test(ua)) return 'linux';
    return 'other';
  }

  // Coarse browser name (no version). Prefers UA-CH brands (low entropy), falls
  // back to UA parsing. Order matters: check derivatives before their base.
  function getBrowser() {
    var uaData = navigator.userAgentData;
    if (uaData && uaData.brands) {
      var brands = uaData.brands.map(function (b) { return b.brand; });
      if (brands.indexOf('Microsoft Edge') >= 0) return 'edge';
      if (brands.indexOf('Opera') >= 0 || brands.indexOf('Opera GX') >= 0) return 'opera';
      if (brands.indexOf('Brave') >= 0) return 'brave';
      if (brands.indexOf('Samsung Internet') >= 0) return 'samsung';
      if (brands.indexOf('Google Chrome') >= 0) return 'chrome';
      if (brands.indexOf('Chromium') >= 0) return 'chromium';
    }
    var ua = navigator.userAgent;
    if (/Edg\//.test(ua)) return 'edge';
    if (/OPR\/|Opera/.test(ua)) return 'opera';
    if (/Firefox\//.test(ua)) return 'firefox';
    if (/Chrome\//.test(ua)) return 'chrome';
    if (/Safari\//.test(ua) && /Version\//.test(ua)) return 'safari';
    return 'other';
  }

  function post(obj) {
    try {
      var blob = new Blob([JSON.stringify(obj)], { type: 'text/plain' }); // avoid CORS preflight
      navigator.sendBeacon(COLLECT_URL, blob);
    } catch (e) { /* ignore */ }
  }

  // Load config and web-vitals in parallel (both cross-origin -> CORS required).
  // pitfall: do not use cache:'force-cache' here. force-cache serves a cached
  // response even when stale, so a browser that fetched rum-config.json once
  // keeps using it until the entry is evicted — a renamed service id then makes
  // that client send a value the collector no longer accepts, and every beacon
  // from it is clamped to default_service and counted as label_rejected. The
  // default cache mode honours the Cache-Control max-age the collector sets
  // (ASSET_MAX_AGE_SECONDS), so config changes reach returning visitors.
  Promise.all([
    fetch(CONFIG_URL).then(function (r) { return r.json(); }),
    import(VITALS_URL) // dynamic cross-origin import requires CORS
  ]).then(function (res) {
    var cfg = res[0], m = res[1];
    var service    = resolveService(cfg);
    var path_group = resolvePathGroup(cfg, service);
    // Captured once, next to path_group, because the two must describe the same
    // page. The tail batch flushes at page hide, and in a client-side-routed app
    // (Next.js, Misskey, Mastodon's /web) the visitor may have soft-navigated
    // several times by then; reading location.pathname at that point would pair a
    // later path with the path_group resolved at load, so one Loki line would name
    // two different pages. Route changes are reported on their own as
    // soft_navigation events (below), but Web Vitals stay attributed to the
    // document that was actually loaded: LCP/INP/CLS are defined per document,
    // and splitting them per route needs the Soft Navigations API, which is not
    // available across browsers.
    var path       = location.pathname;
    var navInfo    = getNavTimings();
    var cfInfo     = getCFInfo();
    var device     = getDevice();
    var os         = getOS();
    var browser    = getBrowser();

    function base() {
      return {
        service: service,
        path_group: path_group,
        path: path,                // Loki only (never a VM label)
        device: device,
        os: os,
        browser: browser,
        conn: getConnection(),
        ts: Date.now()
      };
    }

    // ---- batching ----
    // Metrics finalize at different lifecycle points (FCP/TTFB early; LCP on the
    // first input; INP/CLS at page hide). We coalesce into at most two beacons:
    //   1) a "load" batch flushed at WINDOW_MS  -> navigation + TTFB + FCP, ...
    //   2) a "tail" batch flushed at page hide   -> LCP + INP + CLS, ...
    // Buffering the tail until hide minimizes requests. visibilitychange(hidden)/
    // pagehide is the last reliable moment to send; a metric that never reaches
    // the server (e.g. the tab crashes before hide) is acceptable loss for RUM.
    var WINDOW_MS = 3000;
    var queue = [];

    // flush drains whatever has finalized so far into a single beacon. It is
    // called at the load window and again at hide; splice empties the queue so
    // the two flushes never resend the same event.
    function flush() {
      if (!queue.length) return;
      var batch = queue.splice(0, queue.length);
      post(batch.length === 1 ? batch[0] : batch); // server accepts object or array
    }

    function send(metric) {
      var b = base();
      b.kind = 'web_vitals';
      b.name = metric.name;          // LCP/INP/CLS/FCP/TTFB
      b.value = metric.value;
      b.rating = metric.rating;      // good / needs-improvement / poor
      b.metric_id = metric.id;       // Loki only
      b.nav_type = navInfo.nav_type;
      queue.push(b);
    }

    m.onLCP(send); m.onINP(send); m.onCLS(send); m.onFCP(send); m.onTTFB(send);

    // Navigation timing + CF cache, queued once.
    var navEvent = base();
    navEvent.kind = 'navigation';
    for (var k in navInfo) navEvent[k] = navInfo[k];
    for (var c in cfInfo) navEvent[c] = cfInfo[c];
    queue.push(navEvent);

    var loadFlushed = false;
    setTimeout(function () { loadFlushed = true; flush(); }, WINDOW_MS); // 1) load batch
    addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'hidden') flush(); // 2) tail batch
    });
    addEventListener('pagehide', flush);

    // ---- SPA route changes (soft navigations) ----
    // A client-side router changes the URL with history.pushState/replaceState
    // (and the back button fires popstate) without loading a document, so none of
    // the above sees it. Each change of pathname is reported as a soft_navigation
    // event — a page view with the new path/path_group and no timings. Query- or
    // hash-only changes and same-path replaceState are not page views and are
    // ignored. The check is deferred to a task so it runs outside the router's
    // call stack, and so a pushState immediately followed by a replaceState (a
    // client-side redirect) counts once, for where the visitor landed.
    var lastPath = path;
    var routeCheckPending = false;
    function checkRoute() {
      routeCheckPending = false;
      var p = location.pathname;
      if (p === lastPath) return;
      lastPath = p;
      var ev = base();
      ev.kind = 'soft_navigation';
      ev.path = p;
      ev.path_group = resolvePathGroup(cfg, service);
      ev.nav_type = 'soft';
      queue.push(ev);
      // Before the load window closes, ride along with the load batch; after it,
      // send now rather than holding the event until the page is hidden, which
      // in a long-lived SPA tab can be hours later.
      if (loadFlushed) flush();
    }
    function onRouteChange() {
      if (routeCheckPending) return;
      routeCheckPending = true;
      setTimeout(checkRoute, 0);
    }
    ['pushState', 'replaceState'].forEach(function (fn) {
      var orig = history[fn];
      if (typeof orig !== 'function') return;
      history[fn] = function () {
        var ret = orig.apply(this, arguments);
        onRouteChange();
        return ret;
      };
    });
    addEventListener('popstate', onRouteChange);
  }).catch(function () { /* measurement failure is silent: never affect the page */ });
})();
