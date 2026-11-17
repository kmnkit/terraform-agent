# 파일럿용 테스트 변경

대상 Terraform 저장소(GitLab)에 복사해서 MR을 만들고 에이전트 동작을 확인하기 위한 예시다. 이 저장소의 코드와는 무관하다.

## 구성

| 파일 | 복사 위치 (대상 저장소) | 역할 |
| --- | --- | --- |
| `modules/tf_review_test/*.tf` | `modules/tf_review_test/` | 라벨이 붙은 GCS 버킷 하나를 만드는 작은 모듈 |
| `modules/tf_review_test/tests/defaults_unit_test.tftest.hcl` | `modules/tf_review_test/tests/` | mock provider로 plan만 하는 단위 테스트(자격 증명·실제 리소스 불필요) |
| `dev/tf_review_test.tf` | 파일럿 workspace의 Working Directory | 모듈 호출. `source` 상대 경로와 `project_id`를 실제에 맞게 고친다 |

대상 저장소의 기존 `terraform_check` job이 `modules/*/tests`를 자동으로 찾아 `terraform test`를 실행하므로, MR pipeline에서 이 테스트도 함께 돈다.

## 미리 확인 (Cloud Shell, 대상 저장소 루트)

```bash
terraform -chdir=modules/tf_review_test init -backend=false
terraform -chdir=modules/tf_review_test test
terraform fmt -check -recursive modules/tf_review_test dev
```

`Success! 4 passed, 0 failed.`가 나오면 된다.

## 에이전트에서 기대하는 결과

- plan 코멘트: `module.tf_review_test.google_storage_bucket.this` 추가 1건
- 코드 리뷰 코멘트: 버킷 리소스가 provider 문서(버전 제약 `>= 8.0, < 9.0` 기준)로 리뷰된다. `uniform_bucket_level_access`와 `public_access_prevention`을 지정하지 않았으므로 정적 검사(trivy)나 AI 소견에서 지적될 수 있다. 일부러 넣지 않은 것이다.

확인이 끝나면 MR을 머지하지 않고 닫는다.
