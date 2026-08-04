# Deploying to VKE (k8sg1-repo / Flux CD)

These manifests are the **source of truth for `manifests/rum/prd/` in
[`highemerly/k8sg1-repo`](https://github.com/highemerly/k8sg1-repo)**. Flux syncs
that repo; this repo only builds the image. The release workflow
(`.github/workflows/docker-build.yml`) opens a PR against k8sg1-repo that bumps
the `image:` line in `manifests/rum/prd/deployment.yaml`.

```
this repo            k8sg1-repo                          cluster
deploy/k8s/*.yaml -> manifests/rum/prd/*.yaml  --Flux--> namespace rum-prd
tag vX.Y.Z        -> (Actions) PR: image bump
```

## First-time setup

### 1. Copy the manifests

```bash
mkdir -p ../k8sg1/manifests/rum/prd
cp deploy/k8s/{configmap,deployment,ingressroute,vmservicescrape}.yaml \
   ../k8sg1/manifests/rum/prd/
```

### 2. Add the namespace — `manifests/namespaces/namespaces.yaml`

```yaml
---
apiVersion: v1
kind: Namespace
metadata:
  name: rum-prd
  labels:
    app.kubernetes.io/name: rum-collector
    environment: production
```

### 3. Add the certificate — `manifests/cert-manager/certificates/rum.yaml`

`rum.piyo.me` needs its own cert in the `rum-prd` namespace (the existing
`*.piyo.me` cert lives in `hosteka-prd` and secrets are namespace-scoped).
Remember to add the file to that directory's `kustomization.yaml`.

```yaml
# rum-prd 用証明書（piyo.me）
# CloudFlare DNS 管理のため letsencrypt-prod を使用
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: rum-tls
  namespace: rum-prd
spec:
  secretName: rum-tls
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer
  dnsNames:
    - "rum.piyo.me"
```

### 4. Register with Flux — `clusters/production/apps.yaml`

```yaml
---
# RUM collector 本番環境
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: rum-prd
  namespace: flux-system
spec:
  interval: 1m
  retryInterval: 30s
  path: ./manifests/rum/prd
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  wait: true
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: rum-collector
      namespace: rum-prd
  dependsOn:
    - name: namespaces
    - name: cert-manager
    - name: traefik
```

### 5. DNS + Cloudflare

- `rum.piyo.me` → VKE Traefik の LB IP、**Proxy ON**（オレンジ雲）。
  CF 経由でないと `CF-IPCountry` / `HAN-CF-ASN` が付かない。
- Transform Rules は `../cloudflare/transform-rules.md` を参照
  （ASN ヘッダ注入と、各サービス zone の Server-Timing）。

### 6. GitHub secret

`K8SG1_PAT` … k8sg1-repo に PR を作れる PAT（movapic-neo と同じもの）。
リポジトリの Settings → Secrets and variables → Actions に登録。

## Release

```bash
git tag v0.1.0
git push origin v0.1.0
```

タグ push で `docker-build.yml` が走る:
`checks`（gofmt/vet/test）と `build-and-push`（ghcr へ linux/amd64）が並走 →
両方緑なら `create-k8s-pr` が k8sg1-repo に image 更新 PR を作成 →
PR をマージすると Flux が rollout。

## Verify after rollout

```bash
kubectl -n rum-prd rollout status deploy/rum-collector
kubectl -n rum-prd get pod,svc,ingressroute

# エッジ経由の疎通（注入前でも確認できる）
curl -s https://rum.piyo.me/healthz
curl -sI https://rum.piyo.me/beacon.js | grep -i 'access-control\|cache-control'
curl -s https://rum.piyo.me/rum-config.json | head

# 合成ビーコン（CF 経由なので country/asn も付く）
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://rum.piyo.me/collect \
  -H 'Content-Type: text/plain' \
  -d '{"kind":"navigation","service":"movapic-neo","path_group":"home","device":"desktop","nav_type":"navigate","protocol":"h2","cf_cache":"DYNAMIC"}'

# メトリクスに入ったか（/metrics は外部公開していないので port-forward）
kubectl -n rum-prd port-forward svc/rum-collector 8080:8080 &
curl -s localhost:8080/metrics | grep -E 'rum_country_total|rum_collector_received_total'
```

VM 側は VMAgent の targets に `rum-collector` が UP で出ること、Grafana に
`deploy/grafana/rum-dashboard.json` を import して値が出ることを確認する。

## Rollout order for the script tag

コレクタが緑になってから、1 サービスずつ注入する（設計 §13）:

1. `pic.handon.club`（movapic-neo / Next.js）に直書き — 自作で統制が効く
2. `anypost.dev`, `hosteka.piyo.me`
3. `handon.club`（Traefik rewriteBody）, `mi.hiyoko.club`（CF Worker）

## Rollback

```bash
kubectl -n rum-prd rollout undo deploy/rum-collector   # 即時
```
恒久的には k8sg1-repo で image 行を前のバージョンに戻す PR を出す（Flux が戻す）。
スクリプト注入側は 1 行消すだけで RUM 送信は止まる。
