#!/usr/bin/env bash
# 本番イメージに対する API スモーク。compose.yaml で起動したコレクタに、本物のクライアント（assets/beacon.js）が
# 送るのと同じリクエストを送り、配信物・CORS・/metrics のテキスト出力（VictoriaMetrics が scrape するもの）と、
# Loki への push（偽の Loki が受けたもの）を見る。unit テストはレジストリの値（Gather）までしか見ておらず、
# Loki への送り出しはどこもテストしていないので、client_golang / prometheus/common の更新や送り出しの変更はここで気づく。
#
# 使い方: bash e2e/smoke.sh（手順は compose.yaml の先頭。手元でポートが塞がっていれば E2E_PORT で変える）
# 最後に collector を止める（止めるときに溜まったログを Loki へ送り切るので、それを待たずに確かめられる）。
# もう一度流すときは docker compose up -d から。カウンタは前後の差、Loki はこの実行の path で見る。
set -euo pipefail

BASE_URL="http://127.0.0.1:${E2E_PORT:-18480}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
compose() { docker compose -f "$ROOT/compose.yaml" "$@"; }
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# assets/rum-config.json に載っているサービス。CORS の許可はこのホストの https オリジンから作られる。
SERVICE=hosteka
ORIGIN=https://hosteka.piyo.me
PATH_GROUP=event # /events/* に当たる

failures=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; failures=$((failures + 1)); }

# check <名前> <期待値> <実際の値>
check() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: want [$2], got [$3]"; fi
}

# get <path> <出力先>: ステータスコードを返し、ヘッダを <出力先>.h、本文を <出力先> に書く
get() {
  curl -sS -o "$2" -D "$2.h" -w '%{http_code}' "$BASE_URL$1"
}

# header <ヘッダファイル> <名前>: 値（前後の空白と CR を除く）。無ければ空
header() {
  awk -v n="$(echo "$2" | tr '[:upper:]' '[:lower:]')" -F': ' \
    'tolower($1) == n { sub(/\r$/, "", $2); print $2 }' "$1"
}

# metric <series>: /metrics の該当行の値。行が無ければ 0（まだ一度も記録されていない系列）
metric() {
  curl -sS "$BASE_URL/metrics" | awk -v s="$1" '$1 == s { v = $2 } END { print (v == "" ? 0 : v) }'
}

# ---- 配信物（beacon.js が読み込むもの） ----

check "GET /healthz" 200 "$(get /healthz "$TMP/healthz")"

for asset in beacon.js web-vitals.js; do
  check "GET /$asset status" 200 "$(get "/$asset" "$TMP/$asset")"
  check "GET /$asset Content-Type" "application/javascript; charset=utf-8" "$(header "$TMP/$asset.h" Content-Type)"
  # beacon.js はページから、web-vitals.js は beacon.js から動的 import されるので、どちらもクロスオリジン
  check "GET /$asset ACAO" "*" "$(header "$TMP/$asset.h" Access-Control-Allow-Origin)"
  check "GET /$asset Cache-Control" "public, max-age=300" "$(header "$TMP/$asset.h" Cache-Control)"
  if cmp -s "$ROOT/assets/$asset" "$TMP/$asset"; then pass "GET /$asset body = assets/$asset"; else fail "GET /$asset body differs from assets/$asset"; fi
done

check "GET /rum-config.json status" 200 "$(get /rum-config.json "$TMP/config")"
check "GET /rum-config.json ACAO" "*" "$(header "$TMP/config.h" Access-Control-Allow-Origin)"
# マウントした設定（本番の ConfigMap と同じ中身）をそのまま返す
if cmp -s "$ROOT/assets/rum-config.json" "$TMP/config"; then pass "GET /rum-config.json body = mounted config"; else fail "GET /rum-config.json body differs from mounted config"; fi

# ---- CORS（/collect は設定のホストからだけ受ける） ----

code=$(curl -sS -o /dev/null -D "$TMP/pre.h" -w '%{http_code}' -X OPTIONS \
  -H "Origin: $ORIGIN" -H 'Access-Control-Request-Method: POST' "$BASE_URL/collect")
check "OPTIONS /collect status" 204 "$code"
check "OPTIONS /collect ACAO (configured origin)" "$ORIGIN" "$(header "$TMP/pre.h" Access-Control-Allow-Origin)"

curl -sS -o /dev/null -D "$TMP/pre-x.h" -X OPTIONS \
  -H 'Origin: https://evil.example' -H 'Access-Control-Request-Method: POST' "$BASE_URL/collect"
check "OPTIONS /collect ACAO (other origin)" "" "$(header "$TMP/pre-x.h" Access-Control-Allow-Origin)"

# ---- /collect → /metrics ----
# beacon.js と同じ形で送る: navigator.sendBeacon に type text/plain の Blob を渡す（プリフライトを避けるため）。
# 1回目は読み込み時のまとめ送り（navigation と、その時点で確定した TTFB・FCP の配列）、
# 2回目はページを離れるときの送り（1件だけなら配列でなくオブジェクト）。
# CF-IPCountry と HAN-CF-ASN は本番では Cloudflare が付ける。

wv_labels="device=\"desktop\",nav_type=\"navigate\",path_group=\"$PATH_GROUP\",rating=\"good\",service=\"$SERVICE\""
series=(
  "rum_collector_received_total{kind=\"navigation\",status=\"ok\"}"
  "rum_collector_received_total{kind=\"web_vitals\",status=\"ok\"}"
  "rum_pageview_total{browser=\"chrome\",device=\"desktop\",nav_type=\"navigate\",os=\"macos\",path_group=\"$PATH_GROUP\",service=\"$SERVICE\"}"
  "rum_country_total{country=\"JP\",service=\"$SERVICE\"}"
  "rum_asn_total{asn=\"2516\",service=\"$SERVICE\"}"
  "rum_cf_cache_total{device=\"desktop\",path_group=\"$PATH_GROUP\",protocol=\"h2\",service=\"$SERVICE\",status=\"HIT\"}"
  "rum_nav_response_milliseconds_count{cf_cache=\"HIT\",device=\"desktop\",path_group=\"$PATH_GROUP\",protocol=\"h2\",service=\"$SERVICE\"}"
  "web_vitals_ttfb_milliseconds_count{$wv_labels}"
  "web_vitals_fcp_milliseconds_count{$wv_labels}"
  "web_vitals_lcp_milliseconds_count{$wv_labels}"
  "web_vitals_lcp_milliseconds_bucket{$wv_labels,le=\"2000\"}"
)
want_delta=(1 3 1 1 1 1 1 1 1 1 1)

before=()
for s in "${series[@]}"; do before+=("$(metric "$s")"); done

ts=$(($(date +%s) * 1000))
# Loki で、この実行で送ったものを見分けるための path（/events/* なので path_group は event のまま）
PAGE_PATH="/events/e2e-smoke-$ts"
common="\"service\":\"$SERVICE\",\"path_group\":\"$PATH_GROUP\",\"path\":\"$PAGE_PATH\",\"device\":\"desktop\",\"os\":\"macos\",\"browser\":\"chrome\",\"conn\":\"4g\",\"ts\":$ts"
load_batch="[
  {$common,\"kind\":\"navigation\",\"dns_ms\":0,\"tcp_ms\":12,\"tls_ms\":8,\"request_ms\":95,\"response_ms\":30,\"transfer_size\":15360,\"encoded_size\":15060,\"decoded_size\":61440,\"protocol\":\"h2\",\"nav_type\":\"navigate\",\"cf_cache\":\"HIT\",\"cf_colo\":\"NRT\"},
  {$common,\"kind\":\"web_vitals\",\"name\":\"TTFB\",\"value\":120.5,\"rating\":\"good\",\"metric_id\":\"v4-e2e-ttfb\",\"nav_type\":\"navigate\"},
  {$common,\"kind\":\"web_vitals\",\"name\":\"FCP\",\"value\":800,\"rating\":\"good\",\"metric_id\":\"v4-e2e-fcp\",\"nav_type\":\"navigate\"}
]"
tail_one="{$common,\"kind\":\"web_vitals\",\"name\":\"LCP\",\"value\":1800,\"rating\":\"good\",\"metric_id\":\"v4-e2e-lcp\",\"nav_type\":\"navigate\"}"

for body in "$load_batch" "$tail_one"; do
  code=$(curl -sS -o /dev/null -D "$TMP/collect.h" -w '%{http_code}' -X POST \
    -H "Origin: $ORIGIN" -H 'Content-Type: text/plain;charset=UTF-8' \
    -H 'CF-IPCountry: JP' -H 'HAN-CF-ASN: 2516' \
    --data-binary "$body" "$BASE_URL/collect")
  check "POST /collect status" 204 "$code"
  check "POST /collect ACAO" "$ORIGIN" "$(header "$TMP/collect.h" Access-Control-Allow-Origin)"
done

check "GET /metrics status" 200 "$(get /metrics "$TMP/metrics")"
# VictoriaMetrics が読むテキスト形式（0.0.4）で返っていること。escaping などの付加パラメータは問わない
ctype="$(header "$TMP/metrics.h" Content-Type)"
case "$ctype" in
  "text/plain; version=0.0.4"*) pass "GET /metrics Content-Type ($ctype)" ;;
  *) fail "GET /metrics Content-Type: want text/plain; version=0.0.4, got [$ctype]" ;;
esac

for i in "${!series[@]}"; do
  after="$(metric "${series[$i]}")"
  delta=$(awk -v a="$after" -v b="${before[$i]}" 'BEGIN { print a - b }')
  check "${series[$i]} +${want_delta[$i]}" "${want_delta[$i]}" "$delta"
done

# ---- Loki への push ----
# 本番の rollout と同じく SIGTERM で止める。コレクタは止まる前に溜まったログを送り切る（loki.Client.Close）。
compose stop collector >/dev/null 2>&1
compose exec -T loki wget -qO- http://127.0.0.1:3100/pushes > "$TMP/pushes.json"

check "Loki push Content-Type" "application/json" "$(jq -r '[.[].content_type] | unique | join(",")' "$TMP/pushes.json")"
# 受けた push を「ストリームのラベル・タイムスタンプ・本文（JSON）」の並びにし、この実行の path のものだけ残す
jq --arg p "$PAGE_PATH" '[.[].body.streams[] | .stream as $s | .values[]
  | {stream: $s, ts: .[0], line: (.[1] | fromjson)} | select(.line.path == $p)]' "$TMP/pushes.json" > "$TMP/lines.json"

nav_stream='{"job":"rum-collector","kind":"navigation","service":"'"$SERVICE"'"}'
wv_stream='{"job":"rum-collector","kind":"web_vitals","service":"'"$SERVICE"'"}'
check "Loki navigation lines" 1 "$(jq "[.[] | select(.line.kind == \"navigation\")] | length" "$TMP/lines.json")"
check "Loki web_vitals lines" 3 "$(jq "[.[] | select(.line.kind == \"web_vitals\")] | length" "$TMP/lines.json")"
check "Loki navigation stream labels" "$nav_stream" "$(jq -cS '[.[] | select(.line.kind == "navigation") | .stream] | unique | .[]' "$TMP/lines.json")"
check "Loki web_vitals stream labels" "$wv_stream" "$(jq -cS '[.[] | select(.line.kind == "web_vitals") | .stream] | unique | .[]' "$TMP/lines.json")"
check "Loki web_vitals metrics" "FCP,LCP,TTFB" "$(jq -r '[.[] | select(.line.kind == "web_vitals") | .line.metric] | sort | join(",")' "$TMP/lines.json")"
# タイムスタンプはビーコンの ts（ミリ秒）から。Loki のは文字列のナノ秒、本文の ts は RFC3339
check "Loki timestamps (ns)" "${ts}000000" "$(jq -r '[.[].ts] | unique | join(",")' "$TMP/lines.json")"
check "Loki line ts" "$(jq -nr --argjson t "$ts" '$t / 1000 | todate')" "$(jq -r '[.[].line.ts] | unique | join(",")' "$TMP/lines.json")"
# 本文には、ラベルに出さない詳細（path・geo・Cloudflare の colo など）が入る。サニタイズ後の値で出ること
check "Loki navigation line" \
  '{"asn":"2516","browser":"chrome","cf_cache":"HIT","cf_colo":"NRT","conn":"4g","country":"JP","decoded_size":61440,"device":"desktop","dns_ms":0,"encoded_size":15060,"os":"macos","path_group":"event","protocol":"h2","request_ms":95,"response_ms":30,"tcp_ms":12,"tls_ms":8,"transfer_size":15360}' \
  "$(jq -cS '.[] | select(.line.kind == "navigation") | .line | del(.kind, .service, .path, .ts)' "$TMP/lines.json")"
check "Loki LCP line" \
  '{"browser":"chrome","conn":"4g","device":"desktop","metric":"LCP","metric_id":"v4-e2e-lcp","nav_type":"navigate","os":"macos","path_group":"event","rating":"good","value":1800}' \
  "$(jq -cS '.[] | select(.line.metric == "LCP") | .line | del(.kind, .service, .path, .ts)' "$TMP/lines.json")"

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
