# CLAUDE.md

- 応答は日本語で行う。
- main へは直接コミットしない。作業を始めるときにブランチを切り、PR（squash マージ）で取り込む（[docs/development.md](docs/development.md)「ブランチと PR」）。
- CI の構成・差分での間引きは [docs/development.md](docs/development.md)「CI」を参照する。新しい種類のファイルを足したら `.github/scripts/ci-targets.sh` の判定も見直す。
- 依存は Renovate の PR で上げる。手元で `go get -u` しない（[docs/development.md](docs/development.md)「依存更新（Renovate）」）。
- 設計・エンドポイント・リリースの流れは [README.md](README.md)、k8s へのデプロイは [deploy/k8s/README.md](deploy/k8s/README.md) を参照する。
