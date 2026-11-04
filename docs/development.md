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
| 必須チェック | `ci-result` だけ | ジョブは差分で間引く（下の「CI」）ので、個別に必須にするとスキップされたジョブに結果が付かずマージできなくなる |
| 最新化の強制 | しない（strict=false） | Renovate の PR が複数並んだとき、1件マージするたびに全件を rebase・再 CI させないため |
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

## 依存更新（Renovate）

依存は Renovate（GitHub App）が出す PR で上げる。設定は [.github/renovate.json5](../.github/renovate.json5)（各ルールの理由はコメント参照）。

- **毎週月曜・木曜の 9時前（JST）** にまとめて PR が来る（Go の間接依存の一括更新は月曜のみ）。自動マージはしない。
- **メジャー更新**は PR が自動では来ず、Issue「Dependency Dashboard」の Pending Approval に並ぶ（コード側の対応が要るため）。
- 1つの PR にまとまるもの：
  - **Go**：go.mod の `go` ディレクティブ・Dockerfile の `golang:X.Y.Z`・`.tool-versions`（CI の setup-go は go.mod を読むので、揃えないと CI とイメージで版がずれる）
  - **Go indirect dependencies**：go.mod の `// indirect`（`golang.org/x/sys` など。gomod は lockFileMaintenance に対応していないので、その代わり）
- Go モジュールは公開から3日待ってから PR になる（乗っ取られて公開された版は数日以内に取り下げられることが多いため）。
- ベースイメージ（`golang`・`distroless`）と Dockerfile の syntax は digest で固定。中身の更新は digest 更新の PR として来る。
- **Debian のリリース（`static-debian12` → `13`）は Renovate が提案しない**（イメージ名が変わるため）。distroless のサポート状況を見て手で上げる。
- 対象外：`deploy/cloudflare-worker`（lockfile も CI も無く、wrangler で手でデプロイしている）と `assets/web-vitals.js`（手で同梱している）。上げるときは手で。

### PR が来たら

1. CI（`ci-result`）が緑か見る。赤なら ruleset でマージできない。go.mod と Go のソースの変更は checks と image が両方走り、Dockerfile だけの変更は image だけが走る。
2. PR 本文の変更履歴（Renovate が載せる）を読む。go.mod / go.sum の差分で、実際に入る版が変わっているかを見る（`go mod tidy` で間接依存も動くことがある）。
3. 種類に応じて確認を足す。CI が見ているのは gofmt・`go vet`・unit テスト（`go test -race`）と、本番イメージがビルドできることまで（起動はまだ確かめていない）：

   | 種類 | 例 | マージ前の確認 |
   |---|---|---|
   | メトリクス | `prometheus/client_golang`・`prometheus/common`・`client_model` | unit テストはレジストリの値（`Gather`）までで、**`/metrics` のテキスト出力は見ていない**。common は 0.x で、マイナーでも出力（名前のエスケープ・Content-Type の交渉）を変え得るのに Renovate はメジャー扱いしない。変更履歴を読み、マージ後に本番の `/metrics` を VictoriaMetrics が取れているか見る |
   | Go の版 | Go グループ | checks・image が緑ならマージ。マイナー（1.26 → 1.27）はリリースノートで net/http の挙動の変更を見る |
   | ベースイメージ | `distroless` の digest | ビルドまでしか見ていない。静的バイナリを動かすだけなので壊れにくいが、マージ後のリリースでは Pod が起動したかを見る（deploy/k8s/README.md の確認手順） |
   | リンクされているが使っていないもの | `prometheus/procfs`・`golang.org/x/sys`（プロセスや Go ランタイムのメトリクスは登録していない） | CI 緑ならマージ |
   | タグ時にしか動かない Actions | docker/login-action・docker/metadata-action・peter-evans/create-pull-request | PR の CI では実行されない。マージ後、次のリリースの docker-build.yml で初めて動く |
4. squash マージする。
5. 手元で取り込む：
   ```bash
   git switch main && git pull
   go mod download
   ```
   `.tool-versions` の golang が上がったときは `asdf install` も行う。

### 承認待ち（メジャー）の片付け方

- 着手するときに Dashboard のチェックを入れる → PR が作られる。一度に多く承認せず1つずつ。
- CI が落ちたら、その Renovate のブランチ（`renovate/...`）に直接コミットを足して直す。人のコミットが入ったブランチは Renovate が以後書き換えない。

### その他

- 今は要らない更新：PR を閉じる（その版は作り直されない。次の版が出れば再び PR になる）。恒久的に止めるなら renovate.json5 にルールを足す。
- 急ぐ更新（セキュリティ修正など）：Dashboard の Awaiting Schedule でチェックを入れると、スケジュールを待たずに PR が作られる。
- 手元で `go get -u` はしない（Renovate の PR とぶつかる）。依存の追加はブランチを切って PR で。
- 設定を変えたら検証する（ファイルを引数に渡すとグローバル設定として検証されるので、引数なしで）：
  ```bash
  npx -p renovate renovate-config-validator --strict
  # どの PR に分かれるかの模擬実行（未コミットの設定も読ませる）
  GITHUB_COM_TOKEN=$(gh auth token) RENOVATE_CONFIG_FILE=.github/renovate.json5 \
    npx -p renovate renovate --platform=local --dry-run=lookup
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
