# 컨테이너 이미지 빌드와 푸시

Cloud Run Job이 실행하는 `tf-review` 이미지를 만든다. 이미지 안에는 Go 바이너리, `terraform-mcp-server` v1.4.1, 운영 설정 `config.json`이 들어 있다. 베이스는 셸이 없는 `distroless/static-debian12:nonroot`이며 CA 인증서를 포함하고 비루트로 실행한다. 모든 `FROM`은 digest로 고정했다(멀티 아키텍처 index digest이며 `--platform linux/amd64`가 amd64 매니페스트를 고른다).

## 빌드와 Artifact Registry 푸시 (Cloud Build)

로컬 Docker 없이 Cloud Build로 빌드하고 푸시한다. 로컬에는 `gcloud`만 있으면 된다. 저장소 루트에서 실행한다. Cloud Build 작업자는 linux/amd64이므로 Cloud Run 요구 사항을 그대로 만족한다.

PowerShell:

```powershell
$SHA = git rev-parse --short=12 HEAD
gcloud builds submit . --config build/cloudbuild.yaml --region REGION `
  --substitutions "_IMAGE=REGION-docker.pkg.dev/PROJECT/REPO/tf-review,_TAG=$SHA"
```

bash:

```sh
SHA=$(git rev-parse --short=12 HEAD)
gcloud builds submit . --config build/cloudbuild.yaml --region REGION \
  --substitutions "_IMAGE=REGION-docker.pkg.dev/PROJECT/REPO/tf-review,_TAG=$SHA"
```

`REGION`, `PROJECT`, `REPO`는 실제 값으로 바꾼다. 이미지 태그는 커밋 SHA다. `latest` 같은 이동 태그는 쓰지 않는다. 어떤 코드와 설정이 배포됐는지 SHA로 추적한다. 커밋하지 않은 변경(예: 실제 값을 넣은 `config.json`)도 업로드되므로, 빌드 전 `git status`로 무엇이 들어가는지 확인한다.

처음 한 번 준비:

- Cloud Build API(`cloudbuild.googleapis.com`)를 활성화한다.
- 실행하는 사용자에게 `roles/cloudbuild.builds.editor`를 준다.
- Cloud Build가 쓰는 서비스 계정(프로젝트 설정에 따라 Compute Engine 기본 SA 또는 Cloud Build SA)에 대상 Artifact Registry 저장소의 `roles/artifactregistry.writer`를 준다.

전용 빌드 SA를 쓰려면 `--service-account`를 지정한다. 지정하지 않으면 기본 SA로 실행된다.

```sh
gcloud builds submit . --config build/cloudbuild.yaml --region REGION \
  --service-account "projects/PROJECT/serviceAccounts/BUILD_SA_EMAIL" \
  --substitutions "_IMAGE=REGION-docker.pkg.dev/PROJECT/REPO/tf-review,_TAG=$SHA"
```

전용 빌드 SA에 필요한 권한:

- 대상 Artifact Registry 저장소의 `roles/artifactregistry.writer`
- 프로젝트의 `roles/logging.logWriter` (`cloudbuild.yaml`은 로그를 Cloud Logging에만 남긴다)
- 소스 업로드 버킷 `gs://PROJECT_cloudbuild`의 `roles/storage.objectViewer` (`gcloud builds submit`이 소스를 이 버킷에 올린다)
- 실행하는 사용자는 이 SA에 대해 `roles/iam.serviceAccountUser`가 있어야 한다.

업로드 대상: 저장소에 `.gcloudignore`가 없으면 `gcloud`는 `.gitignore` 규칙으로 업로드 대상을 거른다. 따라서 `.mcp.json`, `CLAUDE.md` 등 gitignore된 파일은 올라가지 않는다. 그다음 Docker 빌드 단계에서 `.dockerignore`가 한 번 더 적용된다.

Docker 라이선스를 쓸 수 있는 환경이라면 로컬 빌드도 가능하다: `docker build --platform linux/amd64 -f build/Dockerfile -t tf-review:$SHA .`

## 설정은 이미지에 포함된다

`build/config.json`은 빌드 시 `/app/config.json`으로 복사된다. 허용 GitLab project ID, 주소, tool 허용 목록, 모델 이름을 바꾸려면 파일을 고치고 다시 빌드해야 한다. 런타임에 env나 job 인자로 이 값을 덮어쓸 수 없고, 이것이 의도한 override 방어다. 저장소의 값은 placeholder(`example-*`, project ID `12345678`)이므로 운영 이미지를 만들 때 실제 값으로 바꾼다. 설정에는 비밀을 넣지 않는다.

`go test ./build/`가 이 파일이 `config.Load` 검증을 통과하는지, 코드가 쓰는 모든 tool 논리 이름이 허용 목록에 있는지 확인한다.

## 토큰 마운트

토큰은 이미지에 없고 Secret Manager 볼륨 마운트로만 주입한다.

| 파일 | 내용 |
| --- | --- |
| `/secrets/gitlab/token` | GitLab 토큰 |
| `/secrets/tfe/token` | HCP Terraform 토큰 |

경로는 `config.json`의 `tokenFiles`와 일치해야 한다. 파일 앞뒤 공백은 제거되어 읽힌다.

## limits 근거

| 항목 | 값 | 이유 |
| --- | --- | --- |
| `diffBytes` | 200000 | 한 MR의 `.tf` 변경분 상한. 일반적인 MR을 담고, 거대 diff가 모델 입력과 비용을 키우지 않게 한다 |
| `docsBytes` | 60000 | 잘라낸 Terraform 문서 상한. 리소스 문서 한두 건(원문 약 40KB)을 넘지 않는 선 |
| `llmOutputBytes` | 8000 | LLM이 만든 문장 하나(리뷰 소견, 위험 소견, plan 실패 설명)의 바이트 상한. 넘으면 잘라 "…"로 끝낸다. GitLab note 한도(1MB)보다 훨씬 작게 둔다 |
| `waits.initial` | 10m | plan run이 나타나기를 기다리는 시간 |
| `waits.total` | 30m | plan 완료까지 최대 대기. job task timeout 45분, 프로세스 deadline 40분, 예약 8분 기준으로 `30m x workspace 1개 <= 32m`를 만족한다 |

워크스페이스는 순차로 대기하므로 `waits.total x 워크스페이스 수`가 32분을 넘으면 설정이 거부된다. 워크스페이스를 늘리려면 `waits.total`을 줄인다.

## 고정 digest 갱신

베이스 이미지를 올릴 때는 태그를 바꾼 뒤 `crane digest IMAGE:TAG`(`go install github.com/google/go-containerregistry/cmd/crane@latest`, Docker 불필요) 또는 `docker buildx imagetools inspect IMAGE:TAG`의 최상단 `Digest:`로 index digest를 확인해 `build/Dockerfile`의 세 `FROM` 줄에 반영한다. 단일 아키텍처 매니페스트 digest가 아니라 index digest를 써야 `--platform linux/amd64`가 올바른 매니페스트를 고른다. 갱신 후 다시 빌드해 스모크를 확인한다.
