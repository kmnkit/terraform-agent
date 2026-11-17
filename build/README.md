# 컨테이너 이미지 빌드와 푸시

Cloud Run Job이 실행하는 `tf-review` 이미지를 만든다. 이미지 안에는 Go 바이너리, `terraform-mcp-server` v1.4.1, 운영 설정 `config.json`이 들어 있다. 베이스는 셸이 없는 `distroless/static-debian12:nonroot`이며 CA 인증서를 포함하고 비루트로 실행한다. 모든 `FROM`은 digest로 고정했다(멀티 아키텍처 index digest이며 `--platform linux/amd64`가 amd64 매니페스트를 고른다).

## 빌드

저장소 루트에서 실행한다. Cloud Run은 linux/amd64만 지원하므로 Apple Silicon에서도 `--platform`을 반드시 지정한다.

```sh
SHA=$(git rev-parse --short=12 HEAD)
docker build --platform linux/amd64 -f build/Dockerfile -t tf-review:$SHA .
```

이미지 태그는 커밋 SHA다. `latest` 같은 이동 태그는 쓰지 않는다. 어떤 코드와 설정이 배포됐는지 SHA로 추적한다.

## Artifact Registry 푸시

```sh
docker tag tf-review:$SHA REGION-docker.pkg.dev/PROJECT/REPO/tf-review:$SHA
docker push REGION-docker.pkg.dev/PROJECT/REPO/tf-review:$SHA
```

`REGION`, `PROJECT`, `REPO`는 실제 값으로 바꾼다. 푸시 전 `gcloud auth configure-docker REGION-docker.pkg.dev`가 필요하다.

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
| `planSummaryBytes` | 40000 | plan 요약(주소, action, 변경 attribute 이름)만 담으므로 값이 없어 작게 잡는다 |
| `llmOutputBytes` | 8000 | MR 코멘트로 읽을 수 있는 길이. GitLab note 한도(1MB)보다 훨씬 작게 둔다 |
| `waits.initial` | 10m | plan run이 나타나기를 기다리는 시간 |
| `waits.total` | 30m | plan 완료까지 최대 대기. job task timeout 45분, 프로세스 deadline 40분, 예약 8분 기준으로 `30m x workspace 1개 <= 32m`를 만족한다 |

워크스페이스는 순차로 대기하므로 `waits.total x 워크스페이스 수`가 32분을 넘으면 설정이 거부된다. 워크스페이스를 늘리려면 `waits.total`을 줄인다.
