// Injects the RUM loader into Misskey (mi.hiyoko.club) HTML responses.
// HTML-only routes (see wrangler.toml) keep billing to page loads; a defensive
// content-type / status check still avoids touching ActivityPub JSON.
const TAG = '<script async src="https://rum.piyo.me/beacon.js"></script>';

export default {
  async fetch(request) {
    const response = await fetch(request);
    const ct = response.headers.get('content-type') || '';
    // Pass through non-HTML and error/redirect responses untouched.
    if (!ct.includes('text/html') || response.status >= 300) {
      return response;
    }
    return new HTMLRewriter()
      .on('head', { element(el) { el.append(TAG, { html: true }); } })
      .transform(response);
  },
};
