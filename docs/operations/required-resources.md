# 필요 리소스

> 순서대로 복사해 실행하는 절차는 [pilot-setup.md](pilot-setup.md)에 있다.

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

`roles/run.jobsExecutorWithOverrides`는 실행 시 args, env, task 수, timeout을 바꿀 수 있는 권한이다. 에이전트는 이 중 args의 `--mr-iid`, `--sha`, `--pipeline-id`만 입력으로 받는다. 보안 설정(허용 GitLab project ID, 주소, tool 허용 목록, 모델 이름)은 이미지 안 파일에서만 읽고, task 수가 1이 아니면 실행하지 않는다. CI 템플릿도 args 외의 override를 쓰지 않는다. 트리거 SA 한 개가 impersonation 후 job 실행에 성공하는지는 종단 검증(설계 문서 Phase 0 항목 6)에서 확인한다.

에이전트 코드가 직접 읽는 env는 `CLOUD_RUN_TASK_COUNT`, `CLOUD_RUN_TASK_INDEX`, `GODEBUG` 세 개뿐이다(`GODEBUG`는 값이 있으면 즉시 실패하기 위해서만 읽는다). 다만 Go 표준 라이브러리와 사용 라이브러리는 env를 암묵적으로 읽으므로, env override로 동작이 바뀔 수 있는 여지가 남는다.

| env | 읽는 쪽 | 영향 | 에이전트의 대응 |
| --- | --- | --- | --- |
| `GODEBUG` | Go 런타임, `net/http` | `http2debug` 등은 요청 헤더(토큰 포함)를 표준 로그로 출력한다 | 값이 있으면 시작 즉시 실패한다. 추가로 HTTP/2를 끄고 표준 `log` 출력을 버린다 |
| `SSL_CERT_FILE`, `SSL_CERT_DIR` | `crypto/x509` | 신뢰할 루트 인증서를 바꾼다. 이미지 밖 경로는 컨테이너에 없으므로 대개 TLS 실패(DoS)로 끝난다 | 없음(잔여 위험) |
| `TMPDIR` | `os.TempDir` | MCP 자식 프로세스의 임시 HOME 위치가 바뀐다 | 없음. 쓰기 불가 경로면 시작 실패 |
| `GOOGLE_APPLICATION_CREDENTIALS` | Google 인증 라이브러리(ADC) | Vertex AI 호출에 쓸 자격 증명 파일을 바꾼다. 컨테이너 안 파일만 가리킬 수 있다 | 없음. 잘못된 값이면 Vertex 호출 실패 |
| `GCE_METADATA_HOST` | Google metadata 클라이언트 | metadata 서버 주소를 바꿔 ADC 토큰 요청을 다른 호스트로 보낼 수 있다 | 없음(잔여 위험, 아래 신뢰 경계 참고) |

프록시 env(`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`)는 모든 HTTP 클라이언트가 무시한다.

## 신뢰 경계

트리거 경로의 신뢰 경계는 "대상 GitLab project에 push할 수 있는 사람"이다. push 권한이 있으면 누구나 `.gitlab-ci.yml`을 고쳐 MR pipeline에서 ID token을 발급받고, WIF로 트리거 SA를 impersonation해 임의의 override(args, env, task 수, timeout)로 job을 실행할 수 있다. 템플릿을 거치지 않는 실행도 막을 수 없다고 가정한다.

에이전트의 방어:

- 입력 검증: args는 `--mr-iid`, `--sha`(40자리 소문자 hex), `--pipeline-id`만 받고 형식을 엄격히 검사한다.
- Preflight: MR의 target·source project가 이미지에 고정한 허용 project ID와 같은지(fork 거부), `--sha`가 MR의 현재 head인지, `--pipeline-id`가 그 MR의 해당 SHA pipeline인지 GitLab API로 확인한다. 통과하기 전에는 MCP 서버를 띄우거나 Vertex 자격 증명을 만들지 않는다.
- task 수 1, task index 0이 아니면 실행하지 않는다.
- env: `GODEBUG`가 있으면 즉시 실패한다. 나머지 env는 위 표대로 판별하지 않는다.
- 설정은 이미지 안 파일에서만, 토큰은 볼륨 마운트 파일에서만 읽는다. MCP 자식 프로세스에는 명시한 env만 넘긴다.
- 프로세스 내부 deadline(40분)이 override된 task timeout과 무관하게 실행 시간을 제한한다.

남는 위험:

- 비용 남용: 같은 project의 MR이면 push 권한자가 job을 반복 실행해 LLM 비용을 늘릴 수 있다.
- 암묵적으로 읽히는 env(위 표의 `SSL_CERT_*`, `TMPDIR`, `GOOGLE_APPLICATION_CREDENTIALS`, `GCE_METADATA_HOST`)로 실행을 실패시키는 DoS, 또는 `GCE_METADATA_HOST`로 job SA의 ADC 토큰 요청을 다른 호스트로 보내는 공격.

선택 강화:

- WIF provider attribute condition에 `assertion.pipeline_source == 'merge_request_event'`를 더해 MR pipeline에서 발급된 ID token만 받는다(claim 이름은 Phase 0 B에서 확인).
- 대상 저장소가 템플릿을 `include`할 때 `ref`를 이동 branch가 아닌 tag나 커밋 SHA로 고정한다.

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
| `tfe-token` | `/secrets/tfe/token` | HCP Terraform team token. 대상 workspace에 한정한 workspace admin 수준 권한(plan JSON endpoint가 admin 수준 token만 허용하기 때문, 설계 문서 3절과 8절). 쓰기 기능은 MCP `--tools` 허용 목록으로 끈다. 실제 권한 경계는 token 권한이므로 파일럿은 prod가 아닌 workspace로 시작한다 |

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
