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

Targets: `pic.handon.club` (SHAMEZOU/Next.js), `anypost.dev`, `hosteka.poyo.me`,
and `dev.handon.club` when it runs a self-hosted app (otherwise use the Traefik
middleware in `../traefik/`).
