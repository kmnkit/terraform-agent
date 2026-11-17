<!-- tf-review:v1:code:- -->
## Terraform 코드 리뷰

기준 커밋: `abcdef1`

### 정적 검사

| 도구 | 결과 | 요약 |
| --- | --- | --- |
| terraform validate | 통과 | 통과 |
| tflint | 실패 | 경고 2건 |

### 리뷰 소견

| 심각도 | 리소스 | 내용 | 문서 |
| --- | --- | --- | --- |
| high | `google_storage_bucket.logs` | uniform_bucket_level_access가 꺼져 있습니다. | https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/storage_bucket |
| medium | `google_project_iam_member.admin` | roles/owner 부여는 최소 권한 원칙에 어긋납니다. |  |

_근거: lock 파일의 provider 버전에 맞는 Terraform 공식 문서._
