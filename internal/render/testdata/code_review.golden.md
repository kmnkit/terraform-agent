<!-- tf-review:v1:code:- -->
## Terraform コードレビュー

基準コミット: `abcdef1`

### 静的チェック

| ツール | 結果 | 概要 |
| --- | --- | --- |
| terraform validate | 成功 | 成功 |
| tflint | 失敗 | 警告 2 件 |

### レビュー所見

_AIが作成した所見です。参考情報として扱ってください。_

| 重大度 | リソース | 内容 | ドキュメント |
| --- | --- | --- | --- |
| error | `google_storage_bucket.logs` | uniform_bucket_level_access が無効になっています。 | https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/storage_bucket |
| warning | `google_project_iam_member.admin` | roles/owner の付与は最小権限の原則に反します。 |  |

_根拠: lock ファイルの provider バージョンに対応する Terraform 公式ドキュメント。_
