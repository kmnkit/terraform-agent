<!-- tf-review:v1:plan:prod -->
## Terraform plan 分析

workspace: `prod`
基準コミット: `abcdef1`
実行: https://app.terraform.io/app/org/workspaces/prod/runs/run-abc123

> 分析失敗: plan の概要のみ表示します。リスク分析は提供されません。

### 変更概要

追加 1 / 変更 1 / 削除 1 / 置換 1

| リソース | 操作 | 変更された attribute |
| --- | --- | --- |
| `google_compute_instance.web` | replace | `machine_type`, `boot_disk` |
| `google_storage_bucket.old` | delete |  |
