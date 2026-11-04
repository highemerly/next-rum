#!/usr/bin/env bash
# ブランチ push の CI（ci.yml）で、どのジョブ（checks / e2e）を回すかを決める。
# main との差分ファイルを見て、変更が影響しうるジョブだけ true にする。
#
# 方針（docs/development.md「CI」）:
# - アプリ本体（Go のソース・go.mod / go.sum・assets）と CI の配線に触れたら両方回す。
# - 下の一覧にないファイルは両方回す（知らない種類の変更を素通しさせない）。
#
# 使い方: ci-targets.sh。GITHUB_OUTPUT があればそこへ、無ければ標準出力へ書く（手元で試せる）。
set -euo pipefail

checks=false
e2e=false

base="$(git merge-base origin/main HEAD)"
while IFS= read -r file; do
  case "$file" in
    # config_sync_test が assets/rum-config.json との一致を見ている
    deploy/k8s/configmap.yaml) checks=true ;;
    # 動作に影響しないもの（ドキュメント・Claude Code の設定・ローカル hook・GitHub の設定・手元用の設定）
    *.md | docs/* | .claude/* | .githooks/* | .github/rulesets/* | .github/renovate.json5 | .gitignore | .tool-versions) ;;
    # 手で反映している設定とマニフェスト（Grafana・vmalert・Traefik・Cloudflare・k8sg1-repo へのコピー元）と、
    # 手元の動作確認用（README「Local end-to-end」）。どれもイメージにもテストにも入らない
    deploy/* | testdata/*) ;;
    # タグ push でしか動かない（ブランチでは確かめようがない）
    .github/workflows/docker-build.yml) ;;
    # ワークフロー単体の変更（Actions の更新など）は、それを使うジョブだけ
    .github/workflows/checks.yml) checks=true ;;
    # 本番イメージの中身と、それを起動して確かめるスモーク（compose・スクリプト）
    .github/workflows/e2e.yml | Dockerfile | .dockerignore | compose.yaml | e2e/*) e2e=true ;;
    *) checks=true; e2e=true ;;
  esac
done < <(git diff --name-only "$base" HEAD)

out="${GITHUB_OUTPUT:-/dev/stdout}"
{
  echo "checks=$checks"
  echo "e2e=$e2e"
} >> "$out"
