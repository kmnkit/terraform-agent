# 필요 리소스

에이전트를 운영하려면 사용자가 Google Cloud project에 아래 리소스를 직접 만들어야 한다. 이 저장소는 이 리소스를 만드는 Terraform을 제공하지 않는다(설계 문서 범위 밖). 아래 `gcloud` 예시는 필요한 설정을 보여주는 용도이며 실제 값(`PROJECT`, `NUM`, `POOL` 등)은 환경에 맞게 바꾼다.

에이전트 한 인스턴스는 Google Cloud project 하나에 종속된다. 아래 리소스는 모두 그 project에 만든다. 대상 Terraform 저장소의 GitLab CI는 `ci/tf-review.gitlab-ci.yml` 템플릿을 `include`해서 이 리소스를 호출한다.

## 리소스 목록

| 리소스 | 설정 | 비고 |
| --- | --- | --- |
| Workload Identity Pool과 OIDC provider | issuer `https://gitlab.com`, attribute mapping, attribute condition은 `assertion.project_id`와 `assertion.namespace_id` 숫자 ID | 조건 claim은 PROVISIONAL. Phase 0 B에서 MR pipeline ID token의 실제 claim을 확인한 뒤 확정한다 |
| 트리거 SA | `roles/run.jobsExecutorWithOverrides`를 Cloud Run Job 한 개에만 부여. WIF principal에 `roles/iam.workloadIdentityUser` 부여 | project 수준 역할을 주지 않는다. SA key는 만들지 않는다 |
| job SA (Cloud Run Job에 부착) | `roles/aiplatform.user`(project), `roles/secretmanager.secretAccessor`를 두 secret에만 부여 | Vertex AI 호출과 토큰 읽기에 쓴다 |
| Secret Manager secret 2개 | GitLab project access token, HCP Terraform team token | 볼륨으로 마운트. env로 주입하지 않는다 |
| Cloud Run Job | task timeout 45m, max retries 0, tasks 1, 이미지는 digest 또는 커밋 SHA 태그 | 인자는 실행 시 `--args`로만 받는다 |
| Artifact Registry 저장소 | Docker 형식, Cloud Run Job과 같은 리전 | 이미지 빌드와 푸시는 `build/README.md` 참조 |
| 실패 알림 | job 실패 로그 기반 alert | Cloud Monitoring 로그 기반 알림 정책 |

## Workload Identity Federation

GitLab ID token의 issuer는 `https://gitlab.com`이다. provider의 attribute condition은 이름이 아닌 숫자 ID로 제한한다. 이름은 바뀔 수 있고 다른 그룹이 같은 이름을 만들 수 있기 때문이다. fork에서 올라온 MR은 `project_id`가 달라 이 조건이 거부한다(fork MR은 지원 범위 밖).

PROVISIONAL: 아래 mapping과 condition은 GitLab 문서 기준의 초안이다. MR pipeline에서 발급된 실제 ID token의 claim으로 Phase 0 B에서 확인하기 전까지 확정이 아니다.

```sh
gcloud iam workload-identity-pools create POOL --project=PROJECT --location=global

gcloud iam workload-identity-pools providers create-oidc PROVIDER \
  --project=PROJECT --location=global --workload-identity-pool=POOL \
  --issuer-uri="https://gitlab.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.project_id=assertion.project_id,attribute.namespace_id=assertion.namespace_id" \
  --attribute-condition="assertion.project_id == 'GITLAB_PROJECT_ID' && assertion.namespace_id == 'GITLAB_NAMESPACE_ID'"
```

`allowed-audiences`는 기본값(provider 전체 리소스 URL)을 그대로 쓴다. CI 템플릿은 ID token의 `aud`를 `https://iam.googleapis.com/` 뒤에 `TF_REVIEW_WIF_PROVIDER`를 붙여 만든다. 이 값이 provider URL과 일치해야 한다.

## 트리거 SA

Cloud Run은 WIF direct resource access를 지원하지 않으므로 이 구간만 SA impersonation을 쓴다. 트리거 SA는 job 실행 외에 아무 권한도 없다.

```sh
gcloud iam service-accounts create tf-review-trigger --project=PROJECT

# job 한 개에만 실행 권한(override 포함)
gcloud run jobs add-iam-policy-binding JOB --project=PROJECT --region=REGION \
  --member="serviceAccount:tf-review-trigger@PROJECT.iam.gserviceaccount.com" \
  --role="roles/run.jobsExecutorWithOverrides"

# 허용된 GitLab project의 WIF principal만 impersonation
gcloud iam service-accounts add-iam-policy-binding \
  tf-review-trigger@PROJECT.iam.gserviceaccount.com --project=PROJECT \
  --member="principalSet://iam.googleapis.com/projects/NUM/locations/global/workloadIdentityPools/POOL/attribute.project_id/GITLAB_PROJECT_ID" \
  --role="roles/iam.workloadIdentityUser"
```

`roles/run.jobsExecutorWithOverrides`는 실행 시 args, env, task 수, timeout을 바꿀 수 있는 권한이다. 에이전트는 이 중 args의 `--mr-iid`, `--sha`, `--pipeline-id`만 읽는다. 보안 설정(허용 GitLab project ID, 주소, tool 허용 목록, 모델 이름)은 이미지 안 파일에서만 읽고, task 수가 1이 아니면 실행하지 않으며, 코드가 읽는 env는 고정된 짧은 목록뿐이다. CI 템플릿도 args 외의 override를 쓰지 않는다. 트리거 SA 한 개가 impersonation 후 job 실행에 성공하는지는 종단 검증(설계 문서 Phase 0 항목 6)에서 확인한다.

## job SA

```sh
gcloud iam service-accounts create tf-review-job --project=PROJECT

gcloud projects add-iam-policy-binding PROJECT \
  --member="serviceAccount:tf-review-job@PROJECT.iam.gserviceaccount.com" \
  --role="roles/aiplatform.user"

# 두 secret에만 접근 권한(project 수준으로 주지 않는다)
for S in gitlab-token tfe-token; do
  gcloud secrets add-iam-policy-binding "$S" --project=PROJECT \
    --member="serviceAccount:tf-review-job@PROJECT.iam.gserviceaccount.com" \
    --role="roles/secretmanager.secretAccessor"
done
```

## Secret

| secret (예시 이름) | 마운트 경로 | 내용 |
| --- | --- | --- |
| `gitlab-token` | `/secrets/gitlab/token` | GitLab project access token(MR diff과 파일 읽기, note 작성) |
| `tfe-token` | `/secrets/tfe/token` | HCP Terraform team token(plan 읽기 전용 권한) |

경로는 이미지 안 `config.json`의 `tokenFiles`와 일치해야 한다. 값은 env로 주입하지 않고 볼륨 마운트 파일로만 읽는다.

## Cloud Run Job

```sh
gcloud run jobs create JOB --project=PROJECT --region=REGION \
  --image=REGION-docker.pkg.dev/PROJECT/REPO/tf-review:GIT_SHA \
  --service-account=tf-review-job@PROJECT.iam.gserviceaccount.com \
  --tasks=1 --max-retries=0 --task-timeout=45m \
  --set-secrets=/secrets/gitlab/token=gitlab-token:latest,/secrets/tfe/token=tfe-token:latest
```

| 항목 | 값 | 이유 |
| --- | --- | --- |
| task timeout | 45m | plan 최대 대기 30분, 프로세스 deadline 40분, 예약 8분 기준(`build/README.md` limits 근거) |
| max retries | 0 | 같은 MR에 코멘트 중복 작성이나 LLM 비용 중복을 막는다 |
| tasks | 1 | 에이전트는 task 수가 1이 아니면 실행하지 않는다 |
| 이미지 | digest 또는 커밋 SHA 태그 | `latest` 같은 이동 태그는 쓰지 않는다 |

## Artifact Registry

```sh
gcloud artifacts repositories create REPO --project=PROJECT --location=REGION \
  --repository-format=docker
```

이미지 빌드, 태그, 푸시 절차는 `build/README.md`를 따른다. job SA 또는 배포 주체가 이미지를 읽을 수 있어야 한다(Cloud Run 서비스 에이전트에 `roles/artifactregistry.reader`가 필요할 수 있다).

## 실패 알림

job 실패는 구조화된 오류 로그로 남는다. 로그 기반 알림 정책으로 운영자에게 알린다. MR 코멘트는 job이 게시에 성공했을 때만 남으므로, 코멘트가 없다는 사실만으로는 실패를 알 수 없다.

예시 로그 필터(Cloud Monitoring 로그 기반 알림의 쿼리):

```
resource.type="cloud_run_job"
resource.labels.job_name="JOB"
severity>=ERROR
```

정확한 필터는 에이전트가 남기는 오류 로그 필드를 보고 정한다. 알림 채널(이메일, Slack 등)은 운영자가 정한다.

## CI 템플릿이 쓰는 변수

대상 저장소의 CI/CD 변수로 정의한다(템플릿 상단 주석과 같은 목록).

| 변수 | 값 |
| --- | --- |
| `TF_REVIEW_PROJECT` | Google Cloud project ID |
| `TF_REVIEW_REGION` | Cloud Run Job 리전 |
| `TF_REVIEW_JOB` | Cloud Run Job 이름 |
| `TF_REVIEW_WIF_PROVIDER` | `projects/NUM/locations/global/workloadIdentityPools/POOL/providers/PROVIDER` (스킴 없음) |
| `TF_REVIEW_TRIGGER_SA` | 트리거 SA 이메일 |
