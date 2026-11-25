// rum.piyo.me/beacon.js
// Loader for Real User Monitoring. Single line to deploy on every service:
//   <script async src="https://rum.piyo.me/beacon.js"></script>
// Service identification and URL grouping come from rum-config.json (single source).
(function () {
  // GPC (Global Privacy Control): a browser with it sends nothing unless the
  // service opted into "gpc_counts", and then only page-view counts and the
  // performance histograms' inputs — no visitor attribute, no timestamp, no id.
  // Decided after reading rum-config.json, since the switch lives there.
  var GPC = navigator.globalPrivacyControl === true;

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
  var CONFIG_URL      = BASE + 'rum-config.json';
  var VITALS_URL      = BASE + 'web-vitals.js';
  var VITALS_ATTR_URL = BASE + 'web-vitals.attribution.js'; // same release, with attribution
  var COLLECT_URL     = BASE + 'collect';

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
  function getNavEntry() {
    return performance.getEntriesByType('navigation')[0];
  }

  function getNavTimings() {
    var nav = getNavEntry();
    if (!nav) return {};
    // A prerendered page counts its load from when it was shown, as web-vitals
    // does for TTFB/FCP/LCP.
    var act = nav.activationStart || 0;
    function sinceStart(t) { return t > 0 ? Math.max(0, Math.round(t - act)) : 0; }
    return {
      dns_ms:      Math.round(nav.domainLookupEnd - nav.domainLookupStart),
      tcp_ms:      Math.round(nav.connectEnd - nav.connectStart),
      tls_ms:      nav.secureConnectionStart > 0
                     ? Math.round(nav.connectEnd - nav.secureConnectionStart) : 0,
      request_ms:  Math.round(nav.responseStart - nav.requestStart),
      response_ms: Math.round(nav.responseEnd - nav.responseStart), // transfer time
      // Navigation start to the end of DOMContentLoaded / of the load event
      // (page load). 0 when the page was hidden before getting there.
      dcl_ms:      sinceStart(nav.domContentLoadedEventEnd),
      load_ms:     sinceStart(nav.loadEventEnd),
      transfer_size: nav.transferSize || 0,
      encoded_size:  nav.encodedBodySize || 0,
      decoded_size:  nav.decodedBodySize || 0,
      protocol:      nav.nextHopProtocol || ''
    };
  }

  // The navigation-timing fields a GPC browser sends: the inputs of the
  // rum_nav_* histograms, nothing else.
  var GPC_NAV_FIELDS = ['dns_ms', 'tcp_ms', 'tls_ms', 'request_ms', 'response_ms', 'transfer_size', 'protocol'];

  // ---- Cloudflare cache info (via Server-Timing, same-origin) ----
  function getCFInfo() {
    var nav = getNavEntry();
    if (!nav || !nav.serverTiming) return {};
    var out = {};
    for (var i = 0; i < nav.serverTiming.length; i++) {
      var st = nav.serverTiming[i];
      if (st.name === 'cf-cache') out.cf_cache = st.description || '';
      if (st.name === 'cf-colo')  out.cf_colo  = st.description || '';
    }
    return out;
  }

  // iPadOS Safari presents itself as a Mac (desktop-class browsing); a Mac has
  // no touch screen, so touch points give the iPad away.
  function isIPadOS() {
    return /Macintosh/.test(navigator.userAgent) && navigator.maxTouchPoints > 1;
  }

  function getDevice() {
    if (isIPadOS() || /iPad/.test(navigator.userAgent)) return 'tablet';
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
    if (isIPadOS()) return 'ios';
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
  // On iOS every browser is WebKit and names itself with its own token
  // (CriOS / FxiOS / EdgiOS) instead of Chrome/Firefox/Edg.
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
    if (/Edg\/|EdgiOS\/|EdgA\//.test(ua)) return 'edge';
    if (/OPR\/|Opera/.test(ua)) return 'opera';
    if (/Firefox\/|FxiOS\//.test(ua)) return 'firefox';
    if (/SamsungBrowser\//.test(ua)) return 'samsung';
    if (/Chrome\/|CriOS\//.test(ua)) return 'chrome';
    if (/Safari\//.test(ua) && /Version\//.test(ua)) return 'safari';
    return 'other';
  }

  // ---- opt-in signals (rum-config.json "features"; off unless the service lists them) ----
  // Host only: the path and query of the previous page never leave the browser.
  function getReferrerHost() {
    try { return document.referrer ? new URL(document.referrer).hostname : ''; } catch (e) { return ''; }
  }
  // The page declares its login state as <html data-rum-auth="in|out">. Read at
  // each page view, so a page that updates it on login/logout is followed.
  function getAuth() {
    return document.documentElement.getAttribute('data-rum-auth') || '';
  }

  // Viewport width (CSS px) as one of four buckets; the raw width is not sent.
  function getViewport() {
    var w = window.innerWidth || document.documentElement.clientWidth || 0;
    if (!w) return '';
    return w < 640 ? '0-639' : w < 1024 ? '640-1023' : w < 1440 ? '1024-1439' : '1440+';
  }

  // Campaign parameters of the landing URL. Only these three leave the
  // browser, reduced to [a-z0-9._-] and 64 characters (the collector repeats it).
  function getUTM() {
    var out = {};
    try {
      var q = new URLSearchParams(location.search);
      ['utm_source', 'utm_medium', 'utm_campaign'].forEach(function (k) {
        var v = (q.get(k) || '').toLowerCase().replace(/[^a-z0-9._-]+/g, '').slice(0, 64);
        if (v) out[k] = v;
      });
    } catch (e) { /* ignore */ }
    return out;
  }

  // ---- versions ----
  // The reduced UA string (Chromium) keeps the browser's major version but
  // freezes the OS version (Windows NT 10.0, Mac OS X 10_15_7, Android 10; K),
  // so the OS version comes from the high-entropy UA-CH hint platformVersion.
  // Firefox and Safari have no UA-CH; what their UA string cannot tell is ''.
  var BRAND = {
    edge: ['Microsoft Edge'], opera: ['Opera', 'Opera GX'], brave: ['Brave'],
    samsung: ['Samsung Internet'], chrome: ['Google Chrome'], chromium: ['Chromium']
  };
  var UA_VERSION = {
    edge: /(?:Edg|EdgiOS|EdgA)\/(\d+)/, opera: /OPR\/(\d+)/, firefox: /(?:Firefox|FxiOS)\/(\d+)/,
    samsung: /SamsungBrowser\/(\d+)/, chrome: /(?:Chrome|CriOS)\/(\d+)/, chromium: /Chrome\/(\d+)/,
    safari: /Version\/(\d+)/
  };

  function getBrowserMajor(browser) {
    var uaData = navigator.userAgentData;
    var names = BRAND[browser] || [];
    if (uaData && uaData.brands) {
      for (var i = 0; i < uaData.brands.length; i++) {
        if (names.indexOf(uaData.brands[i].brand) >= 0) return String(parseInt(uaData.brands[i].version, 10) || '');
      }
    }
    var m = UA_VERSION[browser] && UA_VERSION[browser].exec(navigator.userAgent);
    return m ? m[1] : '';
  }

  function getOSMajor(os, platformVersion) {
    var ua = navigator.userAgent, m;
    var pv = parseInt(platformVersion, 10); // NaN without UA-CH
    if (os === 'windows') {
      // platformVersion 13+ is Windows 11, 1-10 is Windows 10 (0 is 7/8.x).
      // Without it there is no telling: every Windows says NT 10.0.
      if (isNaN(pv) || pv < 1) return '';
      return pv >= 13 ? '11' : '10';
    }
    if (os === 'macos') return isNaN(pv) || pv < 1 ? '' : String(pv);
    if (os === 'android') {
      if (!isNaN(pv) && pv >= 1) return String(pv);
      m = /Android (\d+)/.exec(ua);
      return m && !/Android 10; K\b/.test(ua) ? m[1] : ''; // "10; K" is the frozen value
    }
    if (os === 'ios') {
      // Safari's major version is the iOS major version (Safari 26 ships with
      // iOS 26). The OS version in the UA string is frozen at 18_6 from iOS 26
      // on, so without Version/ it is trusted only below 18.
      m = /Version\/(\d+)/.exec(ua);
      if (m) return m[1];
      m = /OS (\d+)_/.exec(ua);
      return m && +m[1] < 18 ? m[1] : '';
    }
    return '';
  }

  function getPlatformVersion() {
    var uaData = navigator.userAgentData;
    if (!uaData || !uaData.getHighEntropyValues) return Promise.resolve('');
    return uaData.getHighEntropyValues(['platformVersion']).then(
      function (v) { return v.platformVersion || ''; },
      function () { return ''; }
    );
  }

  // ---- Web Vitals attribution ----
  // web-vitals names the element behind LCP/INP/CLS with a CSS selector built
  // from ids and classes. An id often holds a post or user id, so it becomes a
  // bare "#"; runs of 4+ digits become "N"; anything outside a plain selector
  // is dropped; 100 characters at most. The collector applies the same rules.
  function reduceSelector(sel) {
    return String(sel || '')
      .replace(/#[^>]*/g, '#')
      .replace(/[0-9]{4,}/g, 'N')
      .replace(/[^A-Za-z0-9_.>:#-]+/g, '')
      .slice(0, 100) || undefined; // no element (e.g. CLS 0): leave the field out
  }
  function ms(v) { return typeof v === 'number' && isFinite(v) ? Math.round(v * 10) / 10 : undefined; }

  // The breakdown of one metric. Never the LCP resource URL (it can name a
  // user's media), only whether the element was a resource or text.
  function getAttribution(metric) {
    var a = metric.attribution;
    if (!a) return null;
    switch (metric.name) {
      case 'LCP': return {
        target: reduceSelector(a.element), lcp_type: a.url ? 'resource' : 'text',
        ttfb: ms(a.timeToFirstByte), load_delay: ms(a.resourceLoadDelay),
        load_duration: ms(a.resourceLoadDuration), render_delay: ms(a.elementRenderDelay)
      };
      case 'INP': return {
        target: reduceSelector(a.interactionTarget), interaction: a.interactionType,
        input_delay: ms(a.inputDelay), processing: ms(a.processingDuration),
        presentation: ms(a.presentationDelay), load_state: a.loadState
      };
      case 'CLS': return {
        target: reduceSelector(a.largestShiftTarget),
        shift_value: typeof a.largestShiftValue === 'number' ? Math.round(a.largestShiftValue * 10000) / 10000 : undefined,
        load_state: a.loadState
      };
      case 'FCP': return {
        ttfb: ms(a.timeToFirstByte), first_byte_to_fcp: ms(a.firstByteToFCP), load_state: a.loadState
      };
      case 'TTFB': return {
        waiting: ms(a.waitingDuration), cache: ms(a.cacheDuration), dns: ms(a.dnsDuration),
        connection: ms(a.connectionDuration), request: ms(a.requestDuration)
      };
    }
    return null;
  }

  function post(obj) {
    try {
      var blob = new Blob([JSON.stringify(obj)], { type: 'text/plain' }); // avoid CORS preflight
      navigator.sendBeacon(COLLECT_URL, blob);
    } catch (e) { /* ignore */ }
  }

  // Load the config first: whether to measure at all (GPC), which web-vitals
  // build to import and whether to ask for platformVersion all depend on the
  // service's features. web-vitals observes with buffered entries, so loading
  // it after the config loses nothing.
  // pitfall: do not use cache:'force-cache' here. force-cache serves a cached
  // response even when stale, so a browser that fetched rum-config.json once
  // keeps using it until the entry is evicted — a renamed service id then makes
  // that client send a value the collector no longer accepts, and every beacon
  // from it is clamped to default_service and counted as label_rejected. The
  // default cache mode honours the Cache-Control max-age the collector sets
  // (ASSET_MAX_AGE_SECONDS), so config changes reach returning visitors.
  fetch(CONFIG_URL).then(function (r) { return r.json(); }).then(function (cfg) {
    var service  = resolveService(cfg);
    var features = (cfg.features && cfg.features[service]) || [];
    if (GPC && features.indexOf('gpc_counts') < 0) return;
    // Every opt-in signal is off for a GPC browser, whatever the service lists.
    function on(name) { return !GPC && features.indexOf(name) >= 0; }
    return Promise.all([
      import(on('attribution') ? VITALS_ATTR_URL : VITALS_URL), // dynamic cross-origin import requires CORS
      on('versions') ? getPlatformVersion() : ''
    ]).then(function (res) { measure(cfg, service, on, res[0], res[1]); });
  }).catch(function () { /* measurement failure is silent: never affect the page */ });

  function measure(cfg, service, on, m, platformVersion) {
    // Captured once, at load, because the events of this document must describe
    // the same page. The tail batch flushes at page hide, and in a
    // client-side-routed app (Next.js, Misskey, Mastodon's /web) the visitor may
    // have soft-navigated several times by then. Route changes are reported on
    // their own as soft_navigation events (below), but Web Vitals stay
    // attributed to the document that was actually loaded: LCP/INP/CLS are
    // defined per document, and splitting them per route needs the Soft
    // Navigations API, which is not available across browsers.
    // The path itself is never sent — only its path_group. It is kept here to
    // tell a route change from a query- or hash-only change.
    var path_group = resolvePathGroup(cfg, service);
    var path       = location.pathname;
    var nav        = getNavEntry();
    var navType    = (nav && nav.type) || 'navigate';
    var device     = getDevice();
    var os         = getOS();
    var browser    = getBrowser();
    var viewport   = on('viewport') ? getViewport() : '';

    // Fields common to every event. A GPC browser sends the service, the
    // path_group and the device (all histogram labels) and the gpc flag, so the
    // collector takes the GPC path even without the Sec-GPC header.
    function base(pg) {
      if (GPC) return { gpc: true, service: service, path_group: pg, device: device };
      return {
        service: service,
        path_group: pg,
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
    //   1) a "load" batch -> navigation + TTFB + FCP, ...
    //   2) a "tail" batch flushed at page hide -> LCP + INP + CLS, ...
    // Buffering the tail until hide minimizes requests. visibilitychange(hidden)/
    // pagehide is the last reliable moment to send; a metric that never reaches
    // the server (e.g. the tab crashes before hide) is acceptable loss for RUM.
    // The load batch goes at WINDOW_MS, or when the load event has finished if
    // that is later (so the page-load time is known), but no later than
    // MAX_WAIT_MS after navigation start.
    var WINDOW_MS = 3000;
    var MAX_WAIT_MS = 10000;
    var queue = [];

    // flush drains whatever has finalized so far into a single beacon. It is
    // called at the load batch and again at hide; splice empties the queue so
    // the two flushes never resend the same event.
    function flush() {
      if (!queue.length) return;
      var batch = queue.splice(0, queue.length);
      post(batch.length === 1 ? batch[0] : batch); // server accepts object or array
    }

    function send(metric) {
      var b = base(path_group);
      b.kind = 'web_vitals';
      b.name = metric.name;          // LCP/INP/CLS/FCP/TTFB
      b.value = metric.value;
      b.rating = metric.rating;      // good / needs-improvement / poor
      b.nav_type = navType;
      if (!GPC) b.metric_id = metric.id; // Loki only; one page load, not a person
      if (on('attribution')) {
        var at = getAttribution(metric);
        if (at) b.attribution = at;
      }
      queue.push(b);
    }

    m.onLCP(send); m.onINP(send); m.onCLS(send); m.onFCP(send); m.onTTFB(send);

    // The navigation event is built when it is sent, so the DOMContentLoaded and
    // load times are filled in if the page got there; once per document.
    var navQueued = false;
    function queueNavigation() {
      if (navQueued) return;
      navQueued = true;
      var t = getNavTimings(), cf = getCFInfo();
      var ev = base(path_group);
      ev.kind = 'navigation';
      ev.nav_type = navType;
      var keys = GPC ? GPC_NAV_FIELDS : Object.keys(t);
      for (var i = 0; i < keys.length; i++) if (keys[i] in t) ev[keys[i]] = t[keys[i]];
      if (cf.cf_cache) ev.cf_cache = cf.cf_cache;
      if (!GPC && cf.cf_colo) ev.cf_colo = cf.cf_colo;
      if (on('referrer')) ev.referrer = getReferrerHost();
      if (on('auth')) ev.auth = getAuth();
      if (on('viewport')) ev.viewport = viewport;
      if (on('versions')) {
        ev.browser_major = getBrowserMajor(browser);
        ev.os_major = getOSMajor(os, platformVersion);
      }
      // Campaign parameters describe the arrival, so only a fresh navigation
      // carries them (a reload keeps the URL and would count it again).
      if (on('utm') && navType === 'navigate') {
        var utm = getUTM();
        for (var k in utm) ev[k] = utm[k];
      }
      queue.push(ev);
    }

    var loadFlushed = false;
    function loadFlush() {
      if (loadFlushed) return;
      loadFlushed = true;
      queueNavigation();
      flush();
    }
    setTimeout(function () { // 1) load batch
      var n = getNavEntry();
      if (!n || n.loadEventEnd > 0) return loadFlush();
      var timer = setTimeout(loadFlush, Math.max(0, MAX_WAIT_MS - performance.now()));
      // loadEventEnd is set once the load handlers have returned.
      addEventListener('load', function () {
        setTimeout(function () { clearTimeout(timer); loadFlush(); }, 0);
      });
    }, WINDOW_MS);

    // ---- engagement (visible time of each page view) ----
    // A page view is the document load, then each SPA route change. Its visible
    // time is reported at every page hide (the time since the previous report
    // and the total so far) and once more when a route change ends it; the
    // collector adds up the seconds and counts each threshold once. A tab can
    // be hidden and shown many times, and any hide may be the last chance to
    // send, so it is reported in pieces rather than once at the end.
    var engage = on('engagement') ? (function () {
      var since = null; // performance.now() when it became visible; null while hidden
      var unreported = 0, total = 0, pg = path_group;
      if (document.visibilityState === 'visible') since = (nav && nav.activationStart) || 0;
      function stop(now) {
        if (since === null) return;
        var d = Math.max(0, now - since);
        unreported += d; total += d; since = null;
      }
      function report() {
        if (unreported < 1) return;
        var ev = base(pg);
        ev.kind = 'engagement';
        ev.visible_ms = Math.round(unreported);
        ev.total_ms = Math.round(total);
        queue.push(ev);
        unreported = 0;
      }
      return {
        hide: function () { stop(performance.now()); report(); },
        show: function () { if (since === null) since = performance.now(); },
        // A route change ends the current page view and starts the next.
        route: function (nextGroup) {
          var now = performance.now(), visible = since !== null;
          stop(now); report();
          total = 0; pg = nextGroup;
          if (visible) since = now;
        }
      };
    })() : null;

    function onHide() { // 2) tail batch
      if (engage) engage.hide();
      queueNavigation();
      flush();
    }
    addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'hidden') onHide();
      else if (engage) engage.show();
    });
    addEventListener('pagehide', onHide);
    addEventListener('pageshow', function (e) { if (e.persisted && engage) engage.show(); }); // back/forward cache

    // ---- SPA route changes (soft navigations) ----
    // A client-side router changes the URL with history.pushState/replaceState
    // (and the back button fires popstate) without loading a document, so none of
    // the above sees it. Each change of pathname is reported as a soft_navigation
    // event — a page view with the new path_group and no timings. Query- or
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
      var pg = resolvePathGroup(cfg, service);
      if (engage) engage.route(pg);
      var ev = base(pg);
      ev.kind = 'soft_navigation';
      ev.nav_type = 'soft';
      if (on('auth')) ev.auth = getAuth();
      queue.push(ev);
      // Before the load batch, ride along with it; after it, send now rather
      // than holding the event until the page is hidden, which in a long-lived
      // SPA tab can be hours later.
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
  }
})();
