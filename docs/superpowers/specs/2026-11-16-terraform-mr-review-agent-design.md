# Terraform MR 리뷰 에이전트 설계 (Go ADK / Cloud Run Job)

작성일: 2026-11-16 · 기반 문서: `agent-prd.md` (Python 관련 내용은 이 설계에서 모두 무시한다)

## 1. 의도와 이유

Terraform 코드는 GitLab(gitlab.com)에서 관리하고, HCP Terraform workspace가 VCS 연동으로 MR마다 speculative plan을 자동 실행한다. HCP Terraform이 MR에 남기는 것은 run 링크와 status뿐이라 리뷰어는 plan 화면에 들어가 변경 내용을 직접 해석해야 한다. 코드 리뷰 품질도 리뷰어 개인의 provider 지식에 의존한다(deprecated attribute, 오래된 module 버전 등). plan이 실패하면 원인 파악에도 시간이 든다.

이 에이전트는 MR에 push할 때마다 두 가지 정형 코멘트를 남겨 위 문제를 줄인다. 하나는 provider 공식 문서를 근거로 한 코드 리뷰 코멘트이고, 다른 하나는 plan 분석 코멘트(변경 건수, destroy·replace 대상, 리스크 소견, 실패 시 원인)다. 에이전트는 개별 Google Cloud project에 종속되어 그 프로젝트 안에서 실행되고, 공개 엔드포인트와 webhook 없이 호출된다.

## 2. 목표, 비목표, 반목표

**목표**
- push마다 코드 리뷰 코멘트와 plan 분석 코멘트를 정형 포맷으로 게시하거나 갱신한다.
- 건수와 대상 목록 같은 집계는 코드가 결정적으로 만들고, LLM은 리스크 소견과 리뷰 소견 서술만 맡는다.
- inbound 엔드포인트 없이 동작한다.

**비목표**
- HCP Terraform run을 막는 gate 기능(필요하면 GitLab merge check로 대체).
- apply, destroy 등 파괴적 작업 실행.
- Terraform Enterprise, HCP Europe 지원.
- fork에서 올라온 MR 지원(fork pipeline은 `project_id` claim이 달라 WIF가 거부한다).
- Google Cloud 프로젝트 인프라(WIF, SA, Secret, Job 정의) 프로비저닝 자체. 이 저장소는 에이전트와 이미지 빌드까지 다룬다.

**반목표 (기술적으로 동작해도 실패로 본다)**
- sensitive 값이 job 밖으로 나가는 경우(LLM 입력, MR 코멘트, 로그). job 안에서는 plan JSON(`prior_state`와 변경 전후 값 포함, sensitive 표시는 마커일 뿐 값은 평문)을 받아 처리하므로, 이 사실은 보안 검토 시 참고할 수 있게 문서에 남긴다.
- push마다 코멘트가 쌓여 MR이 지저분해지는 경우.
- 에이전트 장애가 MR merge를 막는 경우.
- 공개 접근 가능한 엔드포인트가 생기는 경우.

## 3. 결정 사항

**사용자가 정한 것**
- 언어와 프레임워크: Go, Go용 Google ADK
- 실행 환경: Google Cloud Run Job (프로젝트별 1개)
- 호출: GitLab CI가 Cloud Run Admin API의 job 실행을 호출하며, 이번 실행에만 적용되는 override로 MR 식별자를 전달
- Terraform MCP: 이미지에 내장한 stdio 서버(원격 서버 사용 안 함)
- GitLab: gitlab.com
- 코멘트: MR당 2개(코드 리뷰, plan 분석), 마커로 갱신
- 에이전트 내부: 접근 방식 A (아래 7절)
- plan JSON 입력: 대상 workspace에 한정한 workspace admin 수준 team token을 쓰고, MCP `--tools`로 쓰기 기능을 비활성화한다(HCP Terraform 문서상 plan JSON endpoint가 admin 수준 user·team token만 허용하기 때문).
- plan 실패 로그: 코드가 에러 블록만 추출하고 크기를 제한해 LLM에 전달한다. 자유 텍스트라 남는 위험은 사용자가 수용한다.
- 사내 보안 승인: 필요 없음.
- 이미지 저장소: Google Artifact Registry.

**제 판단 (확인 요청)**
- GitLab 읽기·쓰기는 공식 GitLab MCP 대신 REST API를 직접 호출한다.
- TFE team token과 GitLab project access token은 해당 프로젝트의 Secret Manager에 두고 job에 파일(볼륨 마운트)로 주입한다. GitLab CI 변수에는 두지 않는다. 이유: protected 변수는 unprotected 소스 branch의 MR pipeline에 주입되지 않고, 환경변수로 주입하면 실행 override가 덮어쓸 수 있다.
- CI→job 트리거용 SA에는 사전 정의 role `roles/run.jobsExecutorWithOverrides`를 job 단위로 부여한다(job 설정을 바꿀 수 있는 `roles/run.developer`는 쓰지 않는다).
- 보안에 중요한 설정(허용 GitLab project ID, GitLab·HCP Terraform 주소, tool 허용 목록)은 이미지 안의 파일에 둔다. 문서상 실행 override는 컨테이너 env를 기존 값 위에 덮어쓸 수 있으므로, override로 바뀔 수 있는 출처에는 두지 않는다.
- override로는 MR iid, head commit SHA, pipeline ID만 args로 받는다. 예상하지 못한 입력(추가 env, task 수가 1이 아님 등)이 있으면 즉시 실패한다. 처리 시간 상한은 override와 무관하게 프로세스 내부 deadline으로 둔다.
- MCP 자식 프로세스는 부모 env를 상속하지 않고, 필요한 값만 명시한 env로 띄운다(`TFE_ADDRESS`는 고정).
- MCP 서버와 Terraform provider 문서 조회는 코드가 직접 호출하며 LLM은 tool을 갖지 않는다.
- 정적 검사(fmt, validate, tflint, trivy)는 CI job에서 실행하고, 결과는 artifact로 남겨 에이전트가 GitLab API로 읽는다.
- CI 트리거 job은 job 완료를 기다리지 않고(fire-and-forget) `allow_failure`로 둔다.
- 한 MR에서 여러 workspace가 plan하면 plan 코멘트를 workspace별로 하나씩 둔다(마커에 workspace 식별자 포함). 1차 파일럿은 단일 workspace를 가정한다.
- 파일럿은 prod가 아닌 workspace로 시작한다(admin 권한 token의 영향 범위를 줄이기 위해).
- 이미지는 파일럿 프로젝트의 Artifact Registry에 저장한다. 여러 프로젝트로 확대할 때의 배포 방식(프로젝트별 복제 또는 중앙 저장소 pull 권한)은 그때 정한다.
- Gemini 모델은 `gemini-3.8-flash`를 기본값으로 한다. 이유: 안정(GA) 버전이라 출시 후 최소 12개월 제공이 문서에 명시돼 있고(2026-09-02 출시, 2027-09-02 이후 퇴역), Flash 계열이라 MR당 비용이 낮으며, 서술 생성 두 곳에는 충분하다고 본다. `-latest` 별칭은 가리키는 버전이 바뀌므로 쓰지 않는다. 모델 이름은 이미지 안 설정 파일에 둔다. `gemini-4-argon`(2026-10-13 출시, 12개월 안정)은 품질을 측정한 뒤 올릴 후보로 남긴다. 비용 상한은 정하지 않았고, MR당 실제 비용은 Phase 0에서 측정한다.
- LLM 입력에는 `.tf` 변경분만 포함한다. `.tfvars` 등 변수 값 파일과 `.tf` 이외 파일은 LLM에 보내지 않는다.
- 같은 MR에 연속 push하면 job이 겹칠 수 있다. 게시 직전에 MR의 현재 head SHA를 확인해 자기 SHA와 다르면 게시하지 않고 종료한다. 이전 execution을 취소하지는 않는다(트리거 SA 권한을 늘리지 않기 위해). 코멘트에는 기준 commit SHA를 표시한다.
- 이 저장소는 재사용 가능한 GitLab CI 템플릿(정적 검사 job과 트리거 job)과 정적 검사 artifact의 파일명·포맷 계약을 함께 제공한다. 대상 repository는 이 템플릿을 `include`하고, 파일럿 repository가 첫 사용처다. 정적 검사 도구와 버전은 구현자가 정한다.
- provider 버전은 `.terraform.lock.hcl`에서 얻고, 없으면 `required_providers` 제약을 만족하는 최신 버전을 쓴다. 둘 다 없으면 문서 근거 없이 리뷰하지 않고 코멘트에 그 사실을 표시한다. 1차는 공개 registry만 다루며 private module과 provider는 범위 밖이다(F10).
- Phase 0 검증이 실패하면 구현을 멈추고 사용자와 재설계를 합의한다. 읽기 전용 tool이더라도 허용 목록을 넓히려면 사용자 승인이 필요하다.

## 4. 구성

**트리거 경로**
GitLab MR pipeline → GitLab ID token(id_tokens) → Workload Identity Federation → 트리거 SA impersonation → Cloud Run Admin API로 job 실행(override에는 MR iid, head commit SHA, pipeline ID만 args로 전달).
Cloud Run은 WIF direct resource access를 지원하지 않아 이 구간만 SA impersonation을 쓴다. WIF provider의 attribute condition은 이름이 아닌 숫자 ID(`namespace_id`, `project_id`)로 제한한다.

**실행 단위**
Cloud Run Job 한 개가 이미지 한 개를 실행한다. 이미지 안에는 Go 에이전트 바이너리와 `terraform-mcp-server` 바이너리가 함께 들어 있고, 에이전트가 MCP 서버를 자식 프로세스(stdio)로 띄운다. 이미지는 공식 이미지(관측 시점 v1.4.1)에서 바이너리를 복사해 버전을 고정한다. 공식 이미지는 34.9MB이고 셸이 없다.

**외부 의존**
| 대상 | 인증 | 용도 |
| --- | --- | --- |
| Vertex AI(Gemini) | job에 부착한 SA(ADC), `roles/aiplatform.user` | 서술 생성 2곳 |
| Secret Manager | job SA의 secretAccessor | TFE team token, GitLab project token |
| GitLab REST | project access token | MR diff·파일·artifact 읽기, note 작성·수정 |
| HCP Terraform / registry | stdio MCP 서버가 TFE token으로 호출 | provider 문서, run·plan 조회 |

네트워크는 job에서 나가는 outbound(gitlab.com, app.terraform.io, registry.terraform.io, Vertex AI)뿐이다. inbound 엔드포인트는 없다.

## 5. 처리 흐름

1. CI가 정적 검사를 실행해 결과를 artifact로 남기고, 이어서 트리거 job이 Cloud Run Job을 실행한다.
2. job은 시작 시 입력을 검증한다. 허용 GitLab project ID는 이미지 안 설정에서 읽고, commit SHA가 해당 MR의 현재 head인지, pipeline ID가 해당 MR의 pipeline인지 GitLab API로 확인한다. MCP 서버의 `list_tools` 결과가 허용 목록을 모두 포함하는지 확인하고 없으면 즉시 실패한다. `whoami`와 `get_token_permissions`는 진단용으로 호출해 로그만 남기고 실패 조건으로 쓰지 않는다.
3. **코드 리뷰(F1, F2)**: MR diff에서 변경된 `.tf`를 읽고, resource type·변경 attribute·module source와 버전을 추출한다. provider 버전은 head commit의 `.terraform.lock.hcl`에서 얻어 그 버전의 문서를 조회한다. 문서 검색 결과에서는 정확한 title 일치 항목을 코드가 고르고, 변경된 attribute 설명과 deprecation 문구만 잘라 낸다(리소스당 문서 전체는 약 53KB이므로). 조회 결과는 job 안에서 캐시한다. 잘라 낸 문서와 diff, 정적 검사 결과를 LLM에 주어 구조화된 소견 목록을 받고, 고정 템플릿으로 코멘트를 렌더링해 게시하거나 갱신한다.
4. **plan 분석(F3~F5)**: MR head commit의 commit status에서 HCP Terraform run을 찾는다(미결 검증 항목). 처음 짧은 시간(초기값 수 분) 안에 status가 나타나지 않으면 "이 MR에서는 plan이 실행되지 않음"을 게시하고 끝낸다(경로 필터에 걸리거나 speculative plan을 끈 경우). status가 있으면 queued 상태와 실행 상태를 구분해, 전체 최대 대기 시간(초기값 30분) 동안 polling한다. 완료되면 plan JSON을 MCP로 받아 코드가 add/change/destroy/replace 건수와 대상 목록을 집계한다. sensitive 표시가 있는 값은 제거하고 리소스 주소, action, 변경된 attribute 이름만 남긴 요약을 LLM에 주어 리스크 소견을 받는다. plan이 실패했으면 plan 로그에서 코드가 에러 블록만 추출하고 크기 상한을 적용해 LLM에 주어 원인과 수정안을 서술받는다. run URL은 코드가 조립하고, 템플릿으로 코멘트를 게시하거나 갱신한다.
5. 어느 단계에서든 실패하면 해당 코멘트에 "분석 실패" 안내를 남기는 것을 최선으로 시도하고 job은 오류로 종료한다.

## 6. 코멘트 규칙

- 코드 리뷰 코멘트 1개, plan 분석 코멘트 1개(workspace별). 본문 첫 줄에 마커를 넣고, 기존 note를 찾아 수정한다. 마커는 형식 버전, 코멘트 종류, workspace 식별자를 담는다(commit SHA는 마커가 아닌 본문에 둔다). 나중에 마커 형식을 바꾸면 기존 note가 고아가 되므로 처음부터 버전 필드를 둔다.
- 기존 note를 찾을 때는 마커와 함께 작성자가 에이전트의 bot 사용자인지 확인한다. 게시 후 같은 마커의 note가 둘 이상이면 하나만 남기고 정리한다. 같은 commit에서 반복 실행해도 코멘트 수는 늘지 않는다.
- 집계 섹션은 템플릿으로 고정하고 LLM 출력은 "리스크 소견", "리뷰 소견" 섹션에만 들어간다. AI 소견은 참고용임을 표기한다.
- LLM 출력은 서식 없는 텍스트 목록으로 제한하고 게시 전에 정리한다. `@` 멘션, 줄 첫머리의 `/` 로 시작하는 GitLab quick action, 링크, 이미지, HTML(주석 종결 포함), 마커 문자열은 모두 무력화하거나 제거한다. 출력 길이에는 상한을 둔다. 소견이 가리키는 리소스 주소는 diff나 plan에 실제로 있는 주소인지 코드가 대조한다.
- 출력 항목은 allowlist(리소스 주소, action, 변경 attribute 이름)로 제한한다.

## 7. 접근 방식

**A. 결정적 파이프라인 + LLM은 서술만 (선택)**
Go 코드가 diff 파싱, 문서 조회와 가공, plan 집계를 모두 수행한다. ADK `LlmAgent`는 tool 없이 코드 리뷰 소견과 plan 리스크 서술, 두 곳에서 한 번씩 호출한다. 비용과 동작이 예측 가능하고, MCP와 GitLab과 LLM을 가짜로 바꿔 테스트하기 쉽고, sensitive 값이 LLM에 도달하지 않는다. 단점은 ADK를 얇게 쓴다는 점과 LLM이 추가 문서를 스스로 찾지 못한다는 점이다.

**B. 하이브리드 (기각, 승격 경로로 남김)**
F1만 ADK tool loop을 쓰되 MCP 원본 tool 대신 "리소스 문서 조회" 하나만 노출한다. 리뷰 품질은 높을 수 있으나 호출 횟수와 토큰이 예측하기 어렵다. A로 P0를 검증한 뒤 F1 품질이 부족하면 F1만 B로 올린다.

**C. PRD 원안 (기각)**
모든 기능에서 LLM이 MCP tool을 직접 호출한다. 문서 응답이 크고 plan JSON이 LLM에 노출되어 반목표와 충돌한다.

## 8. 보안과 권한

- SA key는 만들지 않는다. CI→job 구간은 WIF, job→Vertex는 부착 SA를 쓴다.
- 트리거 SA는 해당 job에 대한 실행과 override 실행 권한(`roles/run.jobsExecutorWithOverrides`)만 가진다. job 설정을 바꿀 수 있는 `roles/run.developer`는 쓰지 않는다.
- 실행 override는 args 외에 env, timeout, task 수도 바꿀 수 있다. 호출 권한이 있는 쪽(MR pipeline을 만들 수 있는 사람)이 override 값을 정한다고 가정하고 설계한다. 설정은 이미지 파일에서 읽고, 토큰은 볼륨으로 주입하고, MCP 자식 프로세스 env는 명시한 값만 넘긴다.
- TFE token은 대상 workspace에 한정된 team token이다. user token과 organization token은 쓰지 않는다. HCP Terraform 문서상 plan JSON endpoint는 workspace admin 수준의 user 또는 team token만 허용하고 organization token은 거부한다. 따라서 F3~F5의 plan JSON 입력에는 대상 workspace에 한정한 admin 수준 team token을 쓴다(3절 결정). 이 token이 유출되면 해당 workspace의 admin 권한이 노출되므로, 토큰은 볼륨으로만 주입하고 MCP로 쓰기 기능을 열지 않는다.
- `--tools` 허용 목록은 MCP가 노출하는 기능만 줄이며, 실제 권한 경계는 token의 권한이다.
- MCP 서버는 `--tools`로 필요한 tool만 서버 측에서 활성화한다(`--toolsets`와 동시 사용 불가). 상태 조회(state version), run 생성, workspace 변경 계열은 활성화하지 않는다. tool 이름은 설정으로 분리하고 시작 시 검증한다.
- MR diff와 문서, 로그는 신뢰하지 않는 입력이다. LLM은 tool과 쓰기 경로가 없고, 게시는 코드가 정리한 출력으로만 한다.
- plan JSON과 workspace 정보는 job 안에서만 처리되고, LLM에는 정리된 요약만 전달된다. plan 실패 로그는 에러 블록만 추출해 크기를 제한하고 전달한다.

## 9. 제약

- 문서 조회는 두 단계다(검색으로 doc ID를 얻고 상세를 조회). 검색 결과에는 유사 항목이 섞이므로 코드가 정확히 일치하는 항목을 고른다.
- 서버 측 rate limit 설정은 HTTP 모드용 설정이라 stdio에서 적용되는지 확인이 필요하다. 호출은 순차로 하고 rate limit 문구가 담긴 tool error는 backoff 후 재시도한다.
- 입력 크기 상한(diff, 문서, plan 요약)을 두고 초과 시 집계만 게시한다. 초기값은 구현자가 정한다.
- task timeout은 기본 10분이므로 job 설정에서 plan 대기 시간보다 길게(초기값 45분) 지정한다. job의 최대 재시도 횟수는 0으로 둔다(재시도는 LLM 호출과 게시를 반복한다). 일시적 오류는 프로세스 안에서 API 호출 단위로만 재시도한다.
- 정적 검사 job은 `terraform init -backend=false` 후 validate를 실행한다. private module을 쓰는 repository는 1차에서 지원하지 않는다. artifact는 job의 최대 실행 시간보다 오래 보존하고, 트리거는 MR pipeline에서만 일어나게 workflow 규칙으로 제한한다.
- 같은 repository에 root module이 여러 개면 변경된 파일에서 가장 가까운 상위 디렉터리의 lock 파일을 따른다. 버전을 정한 근거(lock 파일, 제약 조건, 최신 버전)는 코멘트에 적는다.
- job 실패는 구조화된 오류 로그로 남아야 하고, 실패 알림(예: Cloud Monitoring 알림)은 인프라 쪽 요구사항으로 둔다.
- 비용: 1 vCPU, 0.5GiB 기준 35분 실행이 약 $0.04, 코드 리뷰만이면 약 $0.002. MR당 비용은 Gemini 호출이 좌우한다.

## 10. 범위와 검증

**1차(P0)**: F1~F5와 코멘트 멱등성, 파일럿 repository 1개. F6~F13(변수 누락, drift, 버전 점검, apply 결과, 사내 module 권장 등)은 `agent-prd.md`의 P1·P2 범위를 따르며 이 설계의 범위가 아니다.

**테스트 원칙**: MCP, GitLab, LLM을 대체하는 가짜 구현으로 단위·통합 테스트를 하고 코멘트 렌더링은 기대 출력 파일과 비교한다. 파일럿 repository의 실제 MR로 종단 검증한다.

**Phase 0 검증 항목** (구현 전에 확인)
1. 내장 stdio 서버의 실제 tool 이름과 `--tools`로 쓸 이름. TFE token을 준 상태에서 `list_tools` 확인.
2. stdio에서 rate limit 적용 여부.
3. MR pipeline에서 발급되는 GitLab ID token의 claim(ref, sub 등)과 attribute condition 동작.
4. MR head commit의 commit status에서 run을 식별할 수 있는지(`target_url` 등 실제 응답).
5. GitLab note 작성에 필요한 최소 토큰 권한, quick action 무력화 필요 여부.
6. WIF → impersonation → job 실행(override 포함) 종단 동작. 공격 시나리오 검증: override로 env(`TFE_ADDRESS` 등)를 바꿔 토큰이 다른 곳으로 가지 않는지, task 수와 timeout을 바꿔도 동작이 제한되는지.
6-1. 파일럿 repository가 private module을 쓰는지, `.terraform.lock.hcl`을 커밋하는지.
7. job의 outbound 통신이 조직 정책에 막히지 않는지.
8. Go ADK v2 모듈 경로와 버전, job에서 Vertex를 ADC로 호출.
9. 선택한 Gemini 모델이 대상 프로젝트와 리전에서 사용 가능한지, 구조화 출력 지원 여부, MR당 실제 토큰 사용량과 비용.

## 11. 남은 사항

사용자 결정이 필요한 미결 사항은 없다. 남은 것은 Phase 0 검증 항목(10절)이다. Cloud Run Job, SA, WIF, Secret을 만드는 Terraform은 이 저장소의 범위가 아니며, 필요한 리소스 목록은 구현 계획에서 정리한다.

## 12. 빌더에게 맡기는 것

Go 패키지 구조, 구조화된 소견의 스키마, 코멘트 템플릿 문구, 재시도와 backoff 값, 입력 크기 상한 값, 로그 형식, HCL 파싱 방법.
