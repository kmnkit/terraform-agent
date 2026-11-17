<!-- tf-review:v1:plan:prod -->
## Terraform plan 분석

workspace: `prod`
기준 커밋: `abcdef1`
실행: https://app.terraform.io/app/org/workspaces/prod/runs/run-abc123

> 분석 실패: plan 요약만 표시합니다. 위험 분석은 제공되지 않습니다.

### 변경 요약

추가 1 / 변경 1 / 삭제 1 / 교체 1

| 리소스 | 작업 | 변경 attribute |
| --- | --- | --- |
| `google_compute_instance.web` | replace | `machine_type`, `boot_disk` |
| `google_storage_bucket.old` | delete |  |
