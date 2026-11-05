# Direct-inject snippets (self-hosted apps)

For apps you control, add the single loader line to the `<head>` as early as
possible (web-vitals needs its `PerformanceObserver` registered before LCP/FCP
paint). `async` keeps it non-blocking. No `data-service` needed — the service is
resolved from `location.hostname` via rum-config.json.

Plain HTML:

```html
<script async src="https://rum.piyo.me/beacon.js"></script>
```

## Next.js — App Router (`app/layout.tsx`)

```tsx
export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <head>
        {/* RUM loader — keep early in <head> */}
        <script async src="https://rum.piyo.me/beacon.js" />
      </head>
      <body>{children}</body>
    </html>
  );
}
```

## Next.js — Pages Router (`pages/_document.tsx`)

```tsx
import { Html, Head, Main, NextScript } from "next/document";

export default function Document() {
  return (
    <Html lang="ja">
      <Head>
        <script async src="https://rum.piyo.me/beacon.js" />
      </Head>
      <body>
        <Main />
        <NextScript />
      </body>
    </Html>
  );
}
```

Targets: `pic.handon.club` (SHAMEZOU/Next.js), `anypost.dev`, `hosteka.piyo.me`,
and `dev.handon.club` when it runs a self-hosted app (otherwise use the Traefik
middleware in `../traefik/`).

## Same-origin, behind a path prefix (`hosteka.piyo.me`)

A site can serve the collector under its own host instead of `rum.piyo.me`:

```html
<script async src="/_n-rum/beacon.js"></script>
```

The site's Ingress routes `/_n-rum/{beacon.js,web-vitals.js,rum-config.json,collect}`
to the collector and strips `/_n-rum` (hosteka does this in its IngressRoute).
`beacon.js` resolves the other three URLs against its own
directory, so no attribute is needed. Match the four paths exactly rather than
`PathPrefix`, or `/_n-rum/metrics` becomes public.
