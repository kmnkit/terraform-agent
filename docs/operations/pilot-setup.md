# 파일럿 구성 절차 (Cloud Shell 복사·붙여넣기용)

Cloud Shell에서 위에서부터 순서대로 블록을 복사해 붙여넣는다. 값은 0단계에서 만드는 파일 한 곳에만 적고, 이후 블록은 모두 그 파일을 읽는다. Cloud Shell이 끊겨도 각 블록을 그대로 다시 실행하면 된다.

- 각 리소스를 이미 만들었으면 `already exists` 에러가 난다. 무시하고 다음으로 넘어가면 된다.
- 설정 이유와 권한 설명은 `required-resources.md`, 빌드 상세는 `build/README.md`를 본다.

## 0. 저장소와 값 파일 준비

```bash
cd ~ && [ -d terraform-agent ] || git clone https://github.com/kmnkit/terraform-agent.git
cd ~/terraform-agent && git fetch origin && git checkout feature/mr-review-agent && git pull
[ -f ~/tf-review.env ] || cat > ~/tf-review.env <<'EOF'
P=jit-j-flow-dev            # Google Cloud project ID
R=asia-northeast1           # 리전 (Cloud Run Job, Artifact Registry, Cloud Build)
JOB=tf-review               # Cloud Run Job 이름
AR_REPO=tf-review           # Artifact Registry 저장소 이름
BUILD_SA=CHANGE_ME          # 전용 빌드 SA 이메일
GL_PID=CHANGE_ME            # 대상 GitLab 프로젝트의 숫자 ID
GL_NSID=CHANGE_ME           # 대상 GitLab 그룹(namespace)의 숫자 ID
EOF
cloudshell edit ~/tf-review.env
```

에디터에서 `CHANGE_ME`를 실제 값으로 바꾸고 저장한다. GitLab 숫자 ID 두 개는 **에이전트가 리뷰할 Terraform 저장소** 기준이다(Google Cloud project 번호가 아니다).

| 값 | 의미 | 확인 위치 |
| --- | --- | --- |
| `GL_PID` | 대상 Terraform 저장소(GitLab 프로젝트)의 숫자 ID | 저장소 첫 화면 오른쪽 위 ⋮ → "Copy project ID" |
| `GL_NSID` | 그 저장소를 **바로 담고 있는** 그룹(서브그룹)의 숫자 ID | 경로 `a/b/repo`라면 `b` 그룹 화면의 ⋮ → "Copy group ID" |

헷갈리면 아래로 두 값을 한 번에 출력한다. `GROUP/SUBGROUP/PROJECT`는 브라우저 주소창의 저장소 경로로 바꾼다.

```bash
read -rsp "GitLab token: " T; echo
curl -s -H "PRIVATE-TOKEN: $T" "https://gitlab.com/api/v4/projects/$(printf '%s' 'GROUP/SUBGROUP/PROJECT' | sed 's|/|%2F|g')" \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print('GL_PID =',d['id']);print('GL_NSID =',d['namespace']['id'],'(',d['namespace']['full_path'],')')"
unset T
```

## 1. config.json에 실제 값 넣기

```bash
cd ~/terraform-agent && cloudshell edit build/config.json
```

바꿀 항목:

| 항목 | 값 |
| --- | --- |
| `allowedGitlabProjectID` | 0단계의 `GL_PID`와 같은 숫자 |
| `gitlabBaseURL` | GitLab.com이면 그대로, 사내 GitLab이면 그 주소 |
| `tfeOrganization` | HCP Terraform organization 이름 |
| `workspaces` | 파일럿 workspace 이름 (prod 제외) |
| `vertexProject` | `jit-j-flow-dev` |
| `vertexLocation` | Gemini를 쓸 리전 |
| `model` | 그 리전에서 쓸 수 있는 Gemini 모델 이름 |

이 파일은 commit하거나 push하지 않는다. 설정이 이미지에 고정되므로 값을 바꾸면 2단계부터 다시 빌드한다.

```bash
cd ~/terraform-agent && go test ./build/
```

`ok`가 나오면 형식이 맞다.

## 2. 빌드 SA 권한 (처음 한 번)

```bash
source ~/tf-review.env
gcloud services enable cloudbuild.googleapis.com artifactregistry.googleapis.com --project=$P
gcloud artifacts repositories create $AR_REPO --project=$P --location=$R --repository-format=docker
gcloud artifacts repositories add-iam-policy-binding $AR_REPO --project=$P --location=$R \
  --member="serviceAccount:$BUILD_SA" --role=roles/artifactregistry.writer
gcloud projects add-iam-policy-binding $P --member="serviceAccount:$BUILD_SA" --role=roles/logging.logWriter
gcloud storage buckets add-iam-policy-binding gs://${P}_cloudbuild \
  --member="serviceAccount:$BUILD_SA" --role=roles/storage.objectViewer
gcloud iam service-accounts add-iam-policy-binding $BUILD_SA --project=$P \
  --member="user:$(gcloud config get-value account)" --role=roles/iam.serviceAccountUser
```

`gs://..._cloudbuild` 버킷이 없다는 에러가 나면 3단계 빌드를 한 번 실행해 버킷을 만든 뒤 이 줄만 다시 실행한다.

## 3. 이미지 빌드

```bash
source ~/tf-review.env && cd ~/terraform-agent
SHA=$(git log -1 --format=%h --abbrev=12)
gcloud builds submit . --config build/cloudbuild.yaml --region $R \
  --service-account "projects/$P/serviceAccounts/$BUILD_SA" \
  --substitutions "_IMAGE=$R-docker.pkg.dev/$P/$AR_REPO/tf-review,_TAG=$SHA"
echo "IMAGE=$R-docker.pkg.dev/$P/$AR_REPO/tf-review:$SHA" | tee -a ~/tf-review.env
```

빌드 중에 Ctrl+C를 누르면 빌드가 취소된다. `SUCCESS`가 나올 때까지 기다린다.

## 4. API 활성화

```bash
source ~/tf-review.env
gcloud services enable run.googleapis.com aiplatform.googleapis.com secretmanager.googleapis.com \
  iamcredentials.googleapis.com sts.googleapis.com --project=$P
```

## 5. 토큰을 Secret에 저장

토큰은 화면과 셸 기록에 남지 않도록 입력받는다. 실행하면 입력 대기 상태가 되므로 토큰을 붙여넣고 Enter를 누른다.

- GitLab: 대상 저장소의 project access token (`api` scope, 파일럿에서 최소 권한 확인)
- HCP Terraform: 파일럿 workspace에 한정한 workspace admin 수준 team token

```bash
source ~/tf-review.env
read -rsp "GitLab token: " T && printf '%s' "$T" | gcloud secrets create gitlab-token --project=$P --data-file=- ; unset T; echo
read -rsp "HCP Terraform token: " T && printf '%s' "$T" | gcloud secrets create tfe-token --project=$P --data-file=- ; unset T; echo
```

토큰을 바꿀 때는 `create` 대신 `versions add` (`gcloud secrets versions add gitlab-token --project=$P --data-file=-`)를 쓴다.

## 6. job SA와 Cloud Run Job

```bash
source ~/tf-review.env
gcloud iam service-accounts create tf-review-job --project=$P
JSA=tf-review-job@$P.iam.gserviceaccount.com
gcloud projects add-iam-policy-binding $P --member="serviceAccount:$JSA" --role=roles/aiplatform.user
for S in gitlab-token tfe-token; do
  gcloud secrets add-iam-policy-binding $S --project=$P --member="serviceAccount:$JSA" --role=roles/secretmanager.secretAccessor
done
gcloud run jobs create $JOB --project=$P --region=$R --image=$IMAGE \
  --service-account=$JSA --tasks=1 --max-retries=0 --task-timeout=45m \
  --set-secrets=/secrets/gitlab/token=gitlab-token:latest,/secrets/tfe/token=tfe-token:latest
```

SA를 만든 직후 바인딩이 `does not exist`로 실패하면 몇 초 뒤 같은 블록을 다시 실행한다.

## 7. 트리거 SA와 WIF

GitLab.com 기준이다. 사내 GitLab이면 `--issuer-uri`를 그 주소로 바꾼다.

```bash
source ~/tf-review.env
NUM=$(gcloud projects describe $P --format='value(projectNumber)')
gcloud iam service-accounts create tf-review-trigger --project=$P
TSA=tf-review-trigger@$P.iam.gserviceaccount.com
gcloud run jobs add-iam-policy-binding $JOB --project=$P --region=$R \
  --member="serviceAccount:$TSA" --role=roles/run.jobsExecutorWithOverrides
gcloud iam workload-identity-pools create gitlab --project=$P --location=global
gcloud iam workload-identity-pools providers create-oidc gitlab-com --project=$P --location=global \
  --workload-identity-pool=gitlab --issuer-uri="https://gitlab.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.project_id=assertion.project_id,attribute.namespace_id=assertion.namespace_id" \
  --attribute-condition="assertion.project_id == '$GL_PID' && assertion.namespace_id == '$GL_NSID'"
gcloud iam service-accounts add-iam-policy-binding $TSA --project=$P --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/$NUM/locations/global/workloadIdentityPools/gitlab/attribute.project_id/$GL_PID"
```

## 8. GitLab CI/CD 변수 값 출력

아래를 실행하면 GitLab에 등록할 값이 출력된다.

```bash
source ~/tf-review.env
NUM=$(gcloud projects describe $P --format='value(projectNumber)')
echo "TF_REVIEW_PROJECT      = $P"
echo "TF_REVIEW_REGION       = $R"
echo "TF_REVIEW_JOB          = $JOB"
echo "TF_REVIEW_WIF_PROVIDER = projects/$NUM/locations/global/workloadIdentityPools/gitlab/providers/gitlab-com"
echo "TF_REVIEW_TRIGGER_SA   = tf-review-trigger@$P.iam.gserviceaccount.com"
```

대상 저장소의 Settings → CI/CD → Variables에 다섯 개를 등록한다(비밀값이 아니므로 Masked는 필요 없다).

## 9. 대상 저장소에 CI 템플릿 넣기

1. [ci/tf-review.gitlab-ci.yml](../../ci/tf-review.gitlab-ci.yml)의 내용을 대상 저장소의 같은 경로(`ci/tf-review.gitlab-ci.yml`)에 복사한다. GitHub 파일 화면 오른쪽 위의 복사 버튼을 쓰면 된다.
2. 대상 저장소의 `.gitlab-ci.yml`에 아래를 추가한다.

```yaml
include:
  - local: 'ci/tf-review.gitlab-ci.yml'
```

대상 저장소가 `stages:`를 재정의하고 있으면 `test`와 `deploy`를 포함해야 한다. `workflow:rules`가 MR pipeline을 막고 있으면 두 job이 생성되지 않는다.

## 10. 첫 실행과 확인

1. prod가 아닌 workspace를 쓰는 테스트 MR을 만들고 `.tf`를 조금 바꿔 push한다.
2. MR pipeline에서 `tf-review-staticcheck`와 `tf-review-trigger` job이 도는지 본다.
3. Cloud Run Job 실행과 로그를 확인한다.

```bash
source ~/tf-review.env
gcloud run jobs executions list --job=$JOB --project=$P --region=$R --limit=5
gcloud logging read "resource.type=cloud_run_job AND resource.labels.job_name=$JOB" --project=$P --limit=50 --format='value(severity,textPayload,jsonPayload)'
```

4. MR에 코드 리뷰 코멘트와 plan 코멘트가 달리는지 본다. plan 코멘트가 「確認できませんでした」나 「分析失敗」로 나오면 HCP 응답 형태가 코드의 가정과 다른 것이다(Phase 0 B). 위 로그를 그대로 공유한다.

## 이미지를 다시 빌드했을 때

config.json이나 코드를 바꿔 3단계를 다시 실행했으면 job의 이미지를 갱신한다.

```bash
source ~/tf-review.env
gcloud run jobs update $JOB --project=$P --region=$R --image=$IMAGE
```
