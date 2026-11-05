# CLAUDE.md

- 応答は日本語で行う。
- main へは直接コミットしない。作業を始めるときにブランチを切り、PR（squash マージ）で取り込む（[docs/development.md](docs/development.md)「ブランチと PR」）。PR は自分に assign する（`gh pr create --assignee @me`）。
- CI の構成・差分での間引きは [docs/development.md](docs/development.md)「CI」を参照する。新しい種類のファイルを足したら `.github/scripts/ci-targets.sh` の判定も見直す。
- 依存は Renovate の PR で上げる。手元で `go get -u` しない（[docs/development.md](docs/development.md)「依存更新（Renovate）」）。
- 設計・エンドポイント・リリースの流れは [README.md](README.md) を参照する。k8s のマニフェストとデプロイの手順は、非公開のマニフェストのリポジトリにある。

## 公開リポジトリ

- インフラの非公開の情報（マニフェストのリポジトリ名・パス、クラスタの内部の名前や IP）をコード・docs・コミットメッセージ・PR に書かない。ワークフローで要るものはシークレットに置く（Actions のログも公開される）。
- コミットの作者のメールは GitHub の noreply（`3039555+highemerly@users.noreply.github.com`）にする（設定は docs/development.md「ブランチと PR」）。
