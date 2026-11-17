<!-- tf-review:v1:plan:prod -->
## Terraform plan 분석

workspace: `prod`
기준 커밋: `abcdef1`
실행: https://app.terraform.io/app/org/workspaces/prod/runs/run-abc123

### 변경 요약

추가 1 / 변경 1 / 삭제 1 / 교체 1

| 리소스 | 작업 | 변경 attribute |
| --- | --- | --- |
| `google_compute_instance.web` | replace | `machine_type`, `boot_disk` |
| `google_storage_bucket.old` | delete |  |

### 위험 소견

- web 인스턴스가 교체되어 짧은 중단이 발생합니다.
- old 버킷 삭제는 되돌릴 수 없습니다.
