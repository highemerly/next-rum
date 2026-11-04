# 開発の進め方

## ブランチと PR

- **main へは直接コミット・push しない**。変更は必ずブランチを切り、PR → CI 緑 → squash マージで取り込む（依存更新を Renovate の PR で回すため、人の変更も同じ経路に揃える）。
- PR のタイトルは英語で、中身が分かるように付ける。squash のコミットメッセージにそのまま使われる。
- 手元：[.githooks/pre-commit](../.githooks/pre-commit) が main 上のコミットを拒否する。clone ごとに1回有効化する：
  ```bash
  git config core.hooksPath .githooks
  ```
- サーバー側：GitHub の ruleset（定義は [.github/rulesets/main.json](../.github/rulesets/main.json)）が main への push を拒否する。タグは ruleset の対象外なので、リリースのタグは手元から push できる。

### ruleset の中身と理由

| ルール | 設定 | 理由 |
|---|---|---|
| PR 必須 | 承認数 0 | 一人運用で、自分の PR は自分で承認できないため |
| アカウントに紐づかない作者のコミット | 追加の承認を求めない（`require_extra_approval_for_unattributed_changes: false`） | 省略すると true が入り、そうしたコミットを含む PR を自分では承認できず、マージできなくなるため |
| マージ方式 | squash のみ＋linear history | 「1 PR＝1 コミット」の履歴にする |
| 必須チェック | まだ無い | CI の集約ジョブ（`ci-result`）を作ってから、それ1つだけを必須にする |
| force push・削除 | 禁止 | |
| バイパス | なし（管理者も対象） | |

ruleset を変えたら JSON を直して反映する（作成時は POST、以降は `gh api repos/highemerly/next-rum/rulesets` で id を引いて PUT）：
```bash
gh api -X POST repos/highemerly/next-rum/rulesets --input .github/rulesets/main.json
gh api -X PUT repos/highemerly/next-rum/rulesets/<id> --input .github/rulesets/main.json
```

### リポジトリの設定

| 設定 | 値 | 理由 |
|---|---|---|
| マージ方式 | squash だけ有効 | ruleset と揃える（ほかの方式のボタンを出さない） |
| squash のコミット | タイトル＝PR タイトル、本文は空 | ブランチ上の途中のコミットメッセージを main に持ち込まない |
| マージ後のブランチ | 自動で削除 | |
| auto-merge | 有効 | Renovate の PR などを、CI が緑になったら取り込めるように |

```bash
gh api -X PATCH repos/highemerly/next-rum \
  -F allow_merge_commit=false -F allow_rebase_merge=false -F allow_squash_merge=true \
  -F allow_auto_merge=true -F delete_branch_on_merge=true \
  -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=BLANK
```

## CI

### ワークフロー

| ファイル | いつ | 何を |
|---|---|---|
| [ci.yml](../.github/workflows/ci.yml) | main 以外のブランチへの push | 差分を見て、下の2つのうち必要なものだけ呼ぶ。結果は `ci-result` にまとめる |
| [checks.yml](../.github/workflows/checks.yml) | ci.yml とタグから呼ばれる | gofmt / go vet / `go test -race` |
| [image.yml](../.github/workflows/image.yml) | ci.yml から呼ばれる | 本番イメージのビルド（push しない）。ベースイメージや Dockerfile の変更を、タグを打つ前に確かめる |
| [docker-build.yml](../.github/workflows/docker-build.yml) | `v*` タグ | checks とイメージのビルド・push を並走させ、両方緑なら k8sg1-repo へ PR（README「Release / deploy」） |

ジョブの定義は checks.yml・image.yml の1か所に置き、ブランチ用とタグ用の両方から呼ぶ。

main への push では CI を回さない。マージ前に同じ中身をブランチで通しているので、main でも回すとほぼ二重実行になる（しかも続けてマージすると、concurrency のキャンセルで途中の実行が捨てられる）。

### ブランチでは差分が影響しうるジョブだけ回す

[.github/scripts/ci-targets.sh](../.github/scripts/ci-targets.sh) が main との merge-base からの差分ファイルを見て決める。手元でも `.github/scripts/ci-targets.sh` で試せる（標準出力に出る）。

| 変更したファイル | checks | image |
|---|---|---|
| Go のソース・go.mod / go.sum・assets・ci.yml・ci-targets.sh | 回す | 回す |
| `deploy/k8s/configmap.yaml`（config_sync_test が見ている） | 回す | |
| checks.yml | 回す | |
| Dockerfile・image.yml | | 回す |
| ドキュメント・`.claude/`・`.githooks/`・ruleset・Renovate の設定・`.gitignore`・`.tool-versions` | | |
| `deploy/` のその他・`testdata/`（手で反映するもの・手元の動作確認用） | | |
| docker-build.yml（タグでしか動かず、ブランチでは確かめようがない） | | |
| 上のどれにも当たらないもの | 回す | 回す |

新しい種類のファイルを足したら、この表とスクリプトを見直す。一覧に無いものは全部回るので、漏れても検査が抜けることはない（遅くなるだけ）。

タグ（リリース）ではブランチで省いた検査も全部回り、全部緑のときだけ k8sg1-repo へ PR が出る。

### 必須チェックは `ci-result` の1つだけ

ジョブを個別に必須にすると、差分でスキップされたジョブに結果が付かず、PR がマージできなくなる。`ci-result` は全ジョブを `needs` に持ち、成功とスキップは通し、失敗とキャンセルは落とす。

必須でないチェックは auto-merge も ruleset も待たない。マージ前に見たい検査は全部 `ci-result` の `needs` に入れる。

## リリース

変更は PR で main に入れておき、タグは main の先頭に打つ（タグは ruleset の対象外）：

```bash
git switch main && git pull
git tag v0.1.4
git push origin v0.1.4
```

タグで起きること（検査・イメージの push・k8sg1-repo への PR）は [README の Release / deploy](../README.md#release--deploy) を参照。
