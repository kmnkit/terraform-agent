# tf-review: Terraform MR レビューエージェント

GitLab の Merge Request (MR) に push された Terraform コードと、HCP Terraform の plan を自動で分析し、結果を MR のコメントとして投稿するエージェントです。Go と Google ADK (Agent Development Kit) で実装しており、Cloud Run Job として動きます。

> この README は社内でデプロイとパイロットを担当するエンジニア向けの使用説明書です。設計の詳細は `docs/superpowers/specs/2026-11-16-terraform-mr-review-agent-design.md`、必要なリソースの詳細は `docs/operations/required-resources.md` を参照してください（どちらも韓国語）。

## 概要

エージェントは MR への push ごとに次の 2 種類のコメントを投稿し、同じコメントを更新し続けます。

1. **コードレビュー**: MR の差分から変更された `.tf` のリソースと attribute を抽出し、Terraform MCP サーバー (`terraform-mcp-server`) 経由で取得した Terraform 公式ドキュメントの抜粋を根拠に、LLM (Vertex AI の Gemini) が所見を書きます。静的チェック (fmt、validate、tflint、trivy) の結果も表示します。
2. **plan 分析**: HCP Terraform workspace の plan を待ち、追加・変更・削除・置換の件数と対象リソースをコードで集計し、LLM が運用上のリスクを書きます。plan が失敗した場合は、ログのエラーブロックから原因の説明を書きます。

- コメントは MR ごとに、コードレビュー 1 件と workspace ごとの plan 分析 1 件です。再実行しても増えず、既存のコメントが更新されます。
- コメントはすべて日本語です。AI が書いた部分には「参考情報」である旨のラベルが付きます。
- エージェント 1 インスタンスは Google Cloud project 1 つに紐づきます。

## 仕組み

webhook や公開エンドポイントは使いません。対象 Terraform リポジトリの GitLab CI からのみ呼び出します。

```
MR pipeline (merge_request_event)
  1. tf-review-staticcheck ジョブ
       fmt / validate / tflint / trivy を実行し tf-review-staticcheck.json を artifact に保存
  2. tf-review-trigger ジョブ
       GitLab ID token → Workload Identity Federation → トリガー SA を impersonation
       gcloud run jobs execute --async --args=--mr-iid=...,--sha=...,--pipeline-id=...
                │
                ▼
Cloud Run Job (tf-review イメージ, job SA)
  - 入力検証と Preflight (許可 project・fork 拒否・head SHA・pipeline の確認)
  - MR の差分取得 → ドキュメント取得 (Terraform MCP) → LLM レビュー → コードレビューコメント
  - commit status から HCP Terraform run を特定 → plan 待機 → 集計 → LLM → plan コメント
```

- ジョブに渡すのは MR iid、head SHA、pipeline ID の 3 つの引数だけです。セキュリティ関連の設定はイメージ内の `/app/config.json` からのみ読み、トークンは Secret Manager のボリュームマウントからのみ読みます。
- Preflight を通過するまで MCP サーバーの起動や Vertex AI の認証は行いません。
- プロセス内部の締め切りは 40 分です。

## 前提条件

- GitLab は `https://gitlab.com` (WIF の issuer、`config.json` の `gitlabBaseURL` の既定値)。
- HCP Terraform (`https://app.terraform.io` のみ対応) の organization と、VCS 連携で MR に commit status を付ける workspace。パイロットは prod 以外の workspace で始めます。
- Vertex AI で `config.json` の `model` が使える Google Cloud project とリージョン。
- linux/amd64 の GitLab Runner (静的チェックジョブが amd64 バイナリを取得します)。
- ローカル開発: Go 1.26.11 以上 (`go.mod`)、イメージのビルドには Docker。
- fork からの MR は対象外です。

## 必要な Google Cloud リソース

すべて対象の Google Cloud project に手動で作成します。このリポジトリはこれらを作成する Terraform を提供しません。`gcloud` の例と各設定の理由は `docs/operations/required-resources.md` にあります。

| リソース | 要点 |
| --- | --- |
| Workload Identity Pool と OIDC provider | issuer `https://gitlab.com`。attribute condition は GitLab の数値 ID (`project_id`、`namespace_id`) で制限。claim は Phase 0 B で確認するまで暫定 |
| トリガー SA | Cloud Run Job 1 つだけに `roles/run.jobsExecutorWithOverrides`。WIF principal に `roles/iam.workloadIdentityUser`。SA キーは作らない |
| job SA (Cloud Run Job に付与) | `roles/aiplatform.user` (project)、2 つの secret だけに `roles/secretmanager.secretAccessor` |
| Secret Manager の secret 2 つ | GitLab project access token、HCP Terraform team token。ボリュームでマウントし、env では渡さない |
| Cloud Run Job | task timeout 45m、max retries 0、tasks 1、イメージはコミット SHA タグまたは digest |
| Artifact Registry リポジトリ | Docker 形式、Cloud Run Job と同じリージョン |
| 失敗通知 | job の ERROR ログに基づく Cloud Monitoring のアラート |

トークンの権限:

- GitLab: project access token (MR の差分・ファイルの読み取り、note の作成)。必要最小限の権限は Phase 0 B で確認します。
- HCP Terraform: 対象 workspace に限定した workspace admin 相当の team token (plan JSON の取得に admin 相当の権限が必要なため)。書き込み系の機能は MCP の `--tools` 許可リストで無効にしていますが、実際の権限境界はトークンの権限です。

## ビルドとイメージの push

リポジトリのルートで実行します。Cloud Run は linux/amd64 のみ対応なので、Apple Silicon でも `--platform` を必ず指定します。

```sh
SHA=$(git rev-parse --short=12 HEAD)
docker build --platform linux/amd64 -f build/Dockerfile -t tf-review:$SHA .

gcloud auth configure-docker REGION-docker.pkg.dev
docker tag tf-review:$SHA REGION-docker.pkg.dev/PROJECT/REPO/tf-review:$SHA
docker push REGION-docker.pkg.dev/PROJECT/REPO/tf-review:$SHA
```

- イメージには `/app/tf-review`、`/app/terraform-mcp-server` (v1.4.1)、`/app/config.json` が入ります。ベースは `distroless/static-debian12:nonroot` で、すべての `FROM` を digest で固定しています。
- タグはコミット SHA を使い、`latest` のような移動タグは使いません。
- 詳細とベースイメージ digest の更新手順は `build/README.md` を参照してください。

## 設定

### config.json

`build/config.json` がビルド時に `/app/config.json` へコピーされます。値を変えるにはファイルを修正して再ビルドします。実行時に env やジョブ引数で上書きすることはできません (意図的な override 対策です)。リポジトリ内の値はプレースホルダー (`example-*`、project ID `12345678`) なので、本番イメージでは実際の値に置き換えます。秘密情報は入れません。

| フィールド | 説明 |
| --- | --- |
| `allowedGitlabProjectID` | レビューを許可する GitLab project の数値 ID。MR の target・source project がこれと一致しないと実行しない |
| `gitlabBaseURL` | GitLab の URL。空なら `https://gitlab.com` |
| `tfeAddress` | `https://app.terraform.io` 固定 (それ以外は検証エラー) |
| `tfeOrganization` | HCP Terraform の organization 名 (`^[A-Za-z0-9_-]+$`) |
| `workspaces` | plan を分析する workspace 名のリスト。workspace ごとに plan コメントを 1 件投稿 |
| `mcpBinaryPath` | MCP サーバーのパス (`/app/terraform-mcp-server`) |
| `mcpToolAllowlist` | コードが使う論理名 → 実際の MCP tool 名。この 8 つ以外の tool は使わない |
| `model` | Vertex AI のモデル ID。`-latest` 別名は不可 |
| `vertexProject` / `vertexLocation` | Vertex AI の project とロケーション |
| `limits.diffBytes` | LLM に渡す `.tf` 差分の上限 (既定 200000) |
| `limits.docsBytes` | LLM に渡すドキュメント抜粋の上限 (既定 60000) |
| `limits.llmOutputBytes` | LLM が書いた文 1 つの上限。超えると「…」で切る (既定 8000) |
| `waits.initial` | plan の run が現れるまで待つ時間 (既定 10m) |
| `waits.total` | plan 完了までの最大待機 (既定 30m)。`waits.total × workspace 数` が 32 分を超えると設定エラー |
| `tokenFiles.gitlab` / `tokenFiles.tfe` | トークンファイルのパス |

`go test ./build/` で、このファイルが `config.Load` の検証を通ること、コードが使う tool の論理名がすべて許可リストにあることを確認できます。

### シークレットのマウント

| ファイル | 内容 |
| --- | --- |
| `/secrets/gitlab/token` | GitLab トークン |
| `/secrets/tfe/token` | HCP Terraform トークン |

パスは `tokenFiles` と一致させます。Cloud Run Job 作成時の例:

```sh
gcloud run jobs create JOB --project=PROJECT --region=REGION \
  --image=REGION-docker.pkg.dev/PROJECT/REPO/tf-review:GIT_SHA \
  --service-account=tf-review-job@PROJECT.iam.gserviceaccount.com \
  --tasks=1 --max-retries=0 --task-timeout=45m \
  --set-secrets=/secrets/gitlab/token=gitlab-token:latest,/secrets/tfe/token=tfe-token:latest
```

### 環境変数

エージェントのコードが読む env は `CLOUD_RUN_TASK_COUNT`、`CLOUD_RUN_TASK_INDEX`、`GODEBUG` の 3 つだけです。

- task 数が 1、task index が 0 でなければ実行しません。
- `GODEBUG` に値があると起動直後に失敗します (トークンを含むヘッダーがログに出る設定を防ぐため)。
- プロキシ env (`HTTP_PROXY` など) はすべての HTTP クライアントが無視します。

## GitLab CI への組み込み

対象の Terraform リポジトリの `.gitlab-ci.yml` で `ci/tf-review.gitlab-ci.yml` を `include:` します。`ref` は移動するブランチではなく tag またはコミット SHA に固定することを推奨します。

```yaml
include:
  - project: 'GROUP/THIS_REPO'
    ref: 'TAG_OR_COMMIT_SHA'
    file: 'ci/tf-review.gitlab-ci.yml'
```

対象リポジトリ (またはグループ) の CI/CD 変数に次を定義します。テンプレートは既定値を持ちません。

| 変数 | 値 |
| --- | --- |
| `TF_REVIEW_PROJECT` | エージェントの Google Cloud project ID |
| `TF_REVIEW_REGION` | Cloud Run Job のリージョン |
| `TF_REVIEW_JOB` | Cloud Run Job の名前 |
| `TF_REVIEW_WIF_PROVIDER` | `projects/NUM/locations/global/workloadIdentityPools/POOL/providers/PROVIDER` (スキームなし) |
| `TF_REVIEW_TRIGGER_SA` | トリガー SA のメールアドレス |

テンプレートの前提:

- 2 つのジョブ (`tf-review-staticcheck`、`tf-review-trigger`) は MR pipeline (`merge_request_event`) でのみ作られます。`workflow:rules` が MR pipeline を止めているとジョブが作られません。
- 既定の stage (`test`、`deploy`) を使います。`stages` を再定義している場合は両方を含めてください。
- どちらのジョブも `allow_failure: true` で、静的チェックの結果で pipeline を失敗させません。結果は MR コメントで確認します。
- ID token の `aud` は `https://iam.googleapis.com/` + `TF_REVIEW_WIF_PROVIDER` です。WIF provider の allowed audiences (既定値) と一致する必要があります。

## 投稿されるコメントの種類

各コメントの先頭行には、既存コメントを探して更新するための非表示マーカーがあります (`<!-- tf-review:v1:code:- -->`、`<!-- tf-review:v1:plan:<workspace> -->`)。マーカーを編集・削除しないでください。本文には基準コミットの短縮 SHA を表示します。push 中に MR の head が進んだ場合、古い実行は投稿しません。

### コードレビュー (「Terraform コードレビュー」)

- 静的チェックの表 (ツール、成功/失敗、概要)。
- レビュー所見の表 (重大度 info / warning / error、リソース、内容、ドキュメントへのリンク)。AI 参考ラベル付き。
- ドキュメントの根拠がないためレビュー対象外になったリソースの一覧。
- 削除されたリソースの一覧 (ドキュメント照会・レビューの対象外)。
- 根拠の表示: lock ファイルのバージョン / バージョン制約に基づくバージョン / 根拠なし。
- 変更が大きく一部のみレビューした場合の注記。
- 分析失敗時: 静的チェックの結果だけを表示する「分析失敗」版。
- `.tf` の変更がない MR にはコメントしません。

### plan 分析 (「Terraform plan 分析」、workspace ごと)

| 状態 | 内容 |
| --- | --- |
| 正常 | run へのリンク、変更概要 (追加/変更/削除/置換)、対象リソースと変更された attribute 名、リスク所見 (AI 参考ラベル付き) |
| plan 失敗 | エラーブロックに基づく原因の説明 (AI 参考ラベル付き)。エラーブロックがない、または説明を生成できない場合は固定の案内 |
| plan なし | このコミットに対応する plan が見つからない (パスフィルターなどで plan が走らなかった) |
| タイムアウト | 待機時間内に plan が終わらなかった。最後の状態 (queued / running) を表示 |
| 未確認 | リンクされた run をこのコミットの run と確認できなかった |
| 分析失敗 | plan の分析ステージが失敗した。run リンクや概要は表示しない |
| リスク分析失敗 | plan の概要だけを表示し、リスク分析は表示しない |

変更概要とリスク分析では、plan の attribute の値 (sensitive な値を含む) をコメントにも LLM にも渡しません。リソースアドレス、action、変更された attribute 名だけを使います。plan 失敗時に LLM に渡すのは、ログから抽出したエラーブロック (サイズ上限あり) だけです。

## ローカル開発

```sh
go build ./...
go vet ./...
gofmt -l .        # 出力が空であること
go test ./...
```

コメントのレンダリングは golden ファイル (`internal/render/testdata/*.golden.md`) と比較します。文言を変えたら golden を再生成し、差分を確認します。

```sh
go test ./internal/render -run TestGoldenComments -update
```

テストは MCP、GitLab、LLM をすべて偽実装で置き換えており、クラウドへの接続は不要です。

## パイロット手順

パイロットはユーザーの協力が必要な作業です。prod 以外の workspace で行ってください。

### 1. Phase 0 B: 実際の応答形式の確認 (最優先)

いくつかの処理は実環境の応答形式を確認する前の**暫定 (PROVISIONAL)** 実装です。リソースを作ったら、まず次を確認して `docs/superpowers/notes/phase0.md` に記録します。

- [ ] MR head コミットの commit status の `target_url` の形式と、status が head SHA に付くか merge ref に付くか。
- [ ] HCP Terraform の run 詳細 (`get_run_details`) の応答形式。コードは run の status、plan ID、コミット SHA、workspace/organization を JSON:API 形式で読む前提です。実際の応答にこれらが含まれないと、安全側に倒れて毎回「未確認」または「分析失敗」になります。
- [ ] `get_plan_json_output` の応答形式 (本文か URL か) とサイズ、admin 相当の team token で動くか。入力引数名 (`plan_id`) も確認します。
- [ ] TFE token を渡した状態の `tools/list` に許可リストの 8 つが出るか、連続呼び出し時の rate limit。
- [ ] MR pipeline の ID token の claim (WIF の attribute condition、任意で `pipeline_source`)。
- [ ] merged results pipeline の場合に `TF_REVIEW_SHA` (`CI_MERGE_REQUEST_SOURCE_BRANCH_SHA` 優先) が MR の head SHA と一致するか。
- [ ] note 作成に必要な GitLab トークンの最小権限、quick action の無効化が必要か。
- [ ] パイロットリポジトリで private module を使っているか、`.terraform.lock.hcl` をコミットしているか。
- [ ] job からの外向き通信 (gitlab.com、app.terraform.io、registry.terraform.io、Vertex AI) が組織ポリシーで止められていないか。
- [ ] WIF → SA impersonation → 実行時 override (args、env、task 数) の動作。

plan JSON tool が使えない、run を特定できない、許可 tool の追加が必要、override を無効化できない、外向き通信が止められている、のいずれかに当たる場合は先に進まず、設計を見直します。

### 2. エンドツーエンド検証

- [ ] イメージをパイロット project の Artifact Registry に push し、`docs/operations/required-resources.md` に従ってリソースを構成する。
- [ ] 攻撃シナリオの再検証: 実際の job で override により env (`TFE_ADDRESS`、トークン関連の変数、`HTTPS_PROXY` など) や task 数を変えて呼び出し、トークンが他所へ送られず、実行が即座に拒否されることを確認する。
- [ ] 通常の MR: コードレビューと plan のコメントが投稿され、再 push でコメント数が増えず、連続 push で古い実行が投稿しないこと。
- [ ] plan がない MR、plan が失敗する MR、`.tf` 以外だけを変更した MR の動作。
- [ ] MR あたりの実際のトークン使用量とコストを測定・記録し、必要ならモデルの変更を検討する。
- [ ] job が失敗したときに構造化されたエラーログが残り、アラートが届くこと。

## セキュリティと信頼境界

トリガー経路の信頼境界は「対象 GitLab project に push できる人」です。push 権限があれば `.gitlab-ci.yml` を書き換えて ID token を取得し、任意の override で job を実行できます。エージェントは次で被害を限定します。

- 引数は 3 つだけを厳密に検証し、MR の project・head SHA・pipeline を GitLab API で確認 (Preflight)。
- 設定はイメージ内のファイル、トークンはマウントファイルからのみ読む。MCP 子プロセスには明示した env だけを渡す。
- task 数・index の検証、`GODEBUG` の拒否、プロキシ env の無視、40 分の内部締め切り。
- MR の差分、ドキュメント、ログは信頼しない入力として扱う。LLM には tool も書き込み経路もなく、出力はメンション・quick action・リンク・HTML・マーカーを無効化してから投稿する。

残るリスク (同じ project の MR による LLM コストの濫用、暗黙に読まれる env による DoS など) と任意の強化策 (WIF condition に `pipeline_source`、`include` の `ref` 固定) は `docs/operations/required-resources.md` の「신뢰 경계」節を参照してください。

## 制限事項

- ドキュメントの根拠 (lock ファイルまたはバージョン制約を満たす最新バージョンのドキュメント) が得られないリソースはレビューせず、「レビュー対象外」として一覧表示します。lock ファイルがなく制約もない場合などが該当します。
- 削除されたリソースはドキュメントを照会せず、レビューもしません (一覧表示のみ)。
- module はドキュメントを照会せず、source とバージョンを LLM の文脈として渡すだけです。module だけの変更は AI レビューされません。
- 対象は公開 registry の provider だけです。private module・provider は対象外です。
- AI の所見は参考情報です。最終判断はレビュアーが行ってください。
- HCP Terraform は `https://app.terraform.io` のみ。fork からの MR は対象外です。
- workspace は順番に待機するため、既定の待機時間 (30m) では workspace は 1 つまでです。増やすには `waits.total` を短くします。
- 一時的な障害でも「分析失敗」のコメントが投稿されることがあります (前のコミットの古いコメントを残さないため)。
