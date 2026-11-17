# Terraform MR 리뷰 에이전트 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GitLab MR마다 provider 문서 근거의 코드 리뷰 코멘트와 HCP Terraform plan 분석 코멘트를 게시하는 Go 에이전트(Cloud Run Job)와 그 배포 산출물을 만든다.

**Architecture:** 결정적 Go 파이프라인(diff 파싱, 문서 조회·가공, plan 집계, 게시)이 모든 사실을 만들고, ADK `LlmAgent`는 tool 없이 서술 생성에만 쓴다. Terraform MCP 서버는 이미지에 내장한 stdio 자식 프로세스이며 코드가 직접 호출한다. 외부(GitLab, Vertex, MCP)는 모두 인터페이스 뒤에 두어 가짜 구현으로 테스트한다.

**Tech Stack:** Go 1.26.4, Google ADK for Go v2, Terraform MCP 서버(stdio), `hashicorp/hcl/v2`, `modelcontextprotocol/go-sdk`, GitLab REST, Cloud Run Job, Artifact Registry

**Spec:** `docs/superpowers/specs/2026-11-16-terraform-mr-review-agent-design.md`

## Global Constraints

- Go 1.26.4, 모듈 `github.com/terraform-agent`. ADK import 경로는 `google.golang.org/adk/v2/...`.
- Gemini 모델 기본값 `gemini-3.8-flash`. `-latest` 별칭 금지. 모델 이름은 이미지 안 설정 파일에만 둔다.
- MCP 서버는 `terraform-mcp-server` v1.4.1을 stdio로 실행한다. 노출 tool은 서버 측 `--tools`로 제한한다(`--toolsets`와 동시 사용 불가). 자식 프로세스 env는 명시한 값만 넘기고 `TFE_ADDRESS`는 `https://app.terraform.io`로 고정한다. 자식 프로세스의 stderr는 부모 로그로 흘리지 않는다.
- override로 받는 입력은 MR iid, head commit SHA, pipeline ID뿐이다. 코드가 읽는 env는 고정된 짧은 목록(`CLOUD_RUN_TASK_COUNT`, `CLOUD_RUN_TASK_INDEX`, ADC 관련)뿐이며 나머지는 읽지 않는다. task 수가 1이 아니거나 task index가 0이 아니면 실행하지 않는다. 모든 HTTP 클라이언트는 `HTTP_PROXY`·`HTTPS_PROXY` env를 무시한다. 보안 설정(허용 GitLab project ID, 주소, tool 허용 목록, 모델 이름)은 이미지 안 파일에서만 읽는다. 토큰은 볼륨 마운트 파일에서만 읽는다.
- job task timeout 45분, plan 최대 대기 30분(초기값), job 최대 재시도 0. 처리 시간 상한은 프로세스 내부 deadline으로 둔다.
- 마커는 `<!-- tf-review:v1:<kind>:<scope> -->` 형식이다. kind는 `code` 또는 `plan`이고, scope는 plan이면 workspace 이름, code이면 고정값 `-`이다. 기존 note는 마커와 bot 작성자가 모두 일치할 때만 대상으로 한다. 중복이 있으면 note ID가 가장 작은 bot note를 남기고 나머지를 삭제하며, 갱신도 그 note에 한다. commit SHA는 본문에 표시한다.
- LLM 입력은 `.tf` 변경분, 잘라낸 문서, 정적 검사 결과, plan 요약(리소스 주소·action·변경 attribute 이름), 에러 블록뿐이다. 값(value)은 어떤 경우에도 넘기지 않는다. `.tfvars`는 제외한다.
- LLM 출력은 평문 목록이다. `@` 멘션, 줄 첫머리 `/`, 링크, 이미지, HTML, 마커 문자열은 제거하고 길이 상한을 둔다.
- 공개 registry만 지원한다(private module·provider, fork MR, Terraform Enterprise는 범위 밖). 1차 module 처리는 source와 버전을 추출해 LLM에 맥락으로만 전달하고 module 문서 조회는 하지 않는다.
- 저장소 규약: TDD(실패 확인 후 구현), 커버리지 80% 이상, 불변 객체(수정 대신 새 값 반환), 함수 30줄 이하(최대 50), 파일 800줄 이하, 에러는 맥락을 붙여 전파하고 삼키지 않는다, 로그에 토큰·plan JSON·MCP 페이로드를 남기지 않는다.
- 커밋 메시지: `{기능|수정|리팩토링|테스트|문서|설정}: 요약` + 줄바꿈 후 `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Review Focus

설계가 암시하지만 개별 Task 테스트가 따로 다루지 않으면 놓치기 쉬운 경우다. 각 줄의 테스트는 해당 코드를 소유한 Task에 넣었다.

1. **sensitive 값이 plan 요약·LLM 입력·로그 어디에도 없어야 한다.** `prior_state`와 변경 전후에 평문 값이 든 plan JSON을 넣어도 값 문자열이 어떤 출력에도 나타나지 않는다. (Task 10, 13)
2. **`.tf` 변경이 없는 MR(`.tfvars`, 문서만 변경).** 코멘트를 만들지 않고, LLM도 호출하지 않으며, 정상 종료한다. (Task 8, 12)
3. **연속 push·force-push·위조 마커·동시 실행.** head SHA가 달라지면 게시하지 않고 종료한다. 다른 사용자가 마커 문자열을 넣은 note는 무시한다. 같은 SHA로 job 두 개가 동시에 돌아도 note는 하나만 남는다. (Task 5)
4. **큰 입력.** diff, 문서, plan 요약이 상한을 넘으면 오류 없이 잘라내고 코멘트에 잘렸다는 사실을 표시한다. (Task 9, 10, 12)
5. **리소스 주소에 특수문자.** 인덱스 키에 따옴표, 파이프(`|`), 백틱, 개행이 있어도 마크다운 표가 깨지지 않고 sanitizer가 주소를 훼손하지 않는다. (Task 4)

---

## Part 1. 에이전트 코어 (가짜 구현으로 완전히 테스트 가능)

Task 3~6은 Phase 0 결과와 무관하므로 Task 2의 사용자 협조를 기다리는 동안 병렬로 진행해도 된다. Task 7 이후는 Task 2 완료(게이트 통과)가 선행 조건이다.

### Task 1: 골격, 공용 타입, 로거, 설정 로더

**파일:** 수정 `go.mod`(의존성은 사용하는 Task에서 추가) · 생성 `internal/model/model.go`, `internal/logx/logx.go`, `internal/config/config.go`, 각 `_test.go`, `internal/config/testdata/valid.json`
**공용 타입 (`internal/model`, 불변 값 타입):**

| 타입 | 필드 |
| --- | --- |
| `Finding` | Resource, Severity, Message, DocRef |
| `Target` | Address, Action, Attrs |
| `PlanSummary` | Add, Change, Destroy, Replace, Targets, Truncated |
| `CheckResult` | Tool, Passed, Summary |
| `ProviderRef` | Namespace, Name, Version |

**설정 필드:**

| 필드 | 의미 |
| --- | --- |
| `allowedGitlabProjectID` | 처리 허용 GitLab project ID (필수) |
| `gitlabBaseURL` | 기본 `https://gitlab.com` |
| `tfeAddress`, `tfeOrganization`, `workspaces` | HCP Terraform 주소(고정 `https://app.terraform.io`), 조직, 허용 workspace 이름 목록 |
| `mcpBinaryPath`, `mcpToolAllowlist` | MCP 실행 파일과 논리 이름→실제 tool 이름 맵 |
| `model` | `gemini-3.8-flash` |
| `limits` | diff·문서·plan 요약·LLM 출력 크기 상한 |
| `waits` | plan 상태 대기(초기 대기, 전체 대기) |
| `tokenFiles` | TFE·GitLab 토큰 파일 경로 |

bot 사용자 이름은 설정에 두지 않고 GitLab `CurrentUser` 결과를 쓴다.
**Interfaces:** Produces `config.Load(path string) (Config, error)`, `logx.New(w io.Writer, secrets []string) *slog.Logger`(등록된 비밀 문자열이 출력에 나타나면 마스킹).
- [ ] 실패하는 테스트 작성: `TestLoad_Valid`, `TestLoad_RejectsUnknownField`(오류 메시지에 키 이름 포함), `TestLoad_RequiresAllowedProjectID`, `TestLoad_RejectsNonPositiveLimits`, `TestLogger_MasksRegisteredSecrets`(메시지·속성 값·오류 문자열 모두)
- [ ] 실행해 실패 확인: `go test ./internal/config/... ./internal/logx/... -v` → FAIL(미정의)
- [ ] 구현: `Load`는 `encoding/json`의 unknown field 거부 옵션과 검증 함수 분리, 로거는 `slog` 핸들러 래핑
- [ ] 통과 확인: `go test ./internal/... -cover` → PASS, 커버리지 80% 이상
- [ ] 커밋: `기능: 공용 타입, 로거, 설정 로더 추가`

### Task 2: Phase 0 검증 게이트 (사용자 협조 필요)

**파일:** 생성 `docs/superpowers/notes/phase0.md`(결과 기록, 스파이크 코드는 커밋하지 않음)
**목적:** 설계 10절 검증 항목을 구현 전에 확인한다. 사용자 협조가 필요 없는 항목(A)은 먼저 수행하고, 필요한 항목(B)은 사용자와 일정을 잡는다.

| 구분 | 항목 | 확인 방법 | 결과가 바꾸는 것 |
| --- | --- | --- | --- |
| A | 실제 tool 이름과 `--tools` 인자 형식 | 로컬 stdio 서버 `list_tools` | Task 1 `mcpToolAllowlist`, Task 7·9·10 |
| A | ADK Go v2 API(llmagent, runner, Vertex 모델 생성, 구조화 출력, 모델 주입 가능 여부) | 임시 디렉터리 스파이크 | Task 11 |
| A | Gemini 3.8 Flash 사용 가능 여부와 리전, 구조화 출력 지원 | ADC로 1회 호출 | Task 11 |
| B | `get_plan_json_output`의 응답 형태(본문 또는 URL)와 크기, admin team token으로 동작 | 파일럿 workspace의 speculative plan | Task 10 |
| B | TFE token을 준 상태의 `list_tools`(HCP tool 포함), rate limit(stdio에서 연속 호출 11회) | 사용자 실행 | Task 7 재시도 정책 |
| B | MR head commit status의 `target_url` 형태, status가 head SHA에 붙는지 merge ref에 붙는지 | GitLab statuses API 응답 저장 | Task 10 |
| B | MR pipeline ID token claim | 파일럿 MR job에서 payload 확인 | Task 16 WIF 조건 |
| B | note 작성에 필요한 GitLab 토큰 최소 권한, quick action 무력화 필요 여부 | 파일럿 MR | Task 5 |
| B | 파일럿 repository의 private module 사용과 `.terraform.lock.hcl` 커밋 여부 | 사용자 확인 | Task 9 |
| B | job outbound 통신(gitlab.com, app.terraform.io, registry.terraform.io, Vertex)이 조직 정책에 막히지 않는지 | 더미 job 실행 | 중단 조건 |
| B | **더미 Cloud Run Job**(공개 샘플 이미지)으로 WIF → SA impersonation → 실행 override: args·env·task 수 override 동작, 예약 env(`CLOUD_RUN_TASK_COUNT` 등) 덮어쓰기 가능 여부 | 사용자가 최소 리소스를 만들고 호출 | Task 3 방어 방식, 중단 조건 |

- [ ] A 항목을 수행하고 결과(실제 명령, 응답 요약, 결정 사항)를 `docs/superpowers/notes/phase0.md`에 기록한다. 응답 원문은 값(토큰, 비밀)을 제거하고 구조만 남긴다.
- [ ] B 항목을 사용자와 수행하고 같은 문서에 기록한다.
- [ ] **중단 조건 확인:** plan JSON tool 불가, run 식별 불가, 허용 tool 확대 필요, override(env·task 수)를 무력화할 수 없음, outbound 차단 중 하나라도 해당하면 구현(Task 7 이후)을 멈추고 사용자와 재설계를 합의한다.
- [ ] 커밋: `문서: Phase 0 검증 결과 기록`

### Task 3: 입력 검증과 HTTP 공통 설정

**파일:** 생성 `internal/input/input.go`, `internal/httpx/httpx.go`, 각 `_test.go`
**Interfaces:** Produces
- `input.Parse(args []string, getenv func(string) string) (Input, error)` — `Input{MRIID int, SHA string, PipelineID int}`
- `httpx.NewClient(timeout time.Duration) *http.Client` — env 프록시 무시, `httpx.DisableEnvProxy()` — 기본 transport에도 적용
- [ ] 테스트(`input`): `TestParse_Valid`, `TestParse_RejectsUnknownFlag`, `TestParse_RejectsMissingFlag`, `TestParse_RejectsNonNumeric`, `TestParse_RejectsSHAFormat`, `TestParse_RejectsTaskCountNotOne`, `TestParse_RejectsTaskIndexNotZero`
- [ ] 테스트(`httpx`, Review 보강): `HTTPS_PROXY`를 가짜 프록시로 설정해도 요청이 프록시를 거치지 않음(프록시 서버가 호출 횟수 0)
- [ ] 실패 확인 → 구현(`flag.FlagSet`, `ContinueOnError`; transport의 `Proxy`를 nil) → 통과 확인: `go test ./internal/input/... ./internal/httpx/... -cover`
- [ ] 커밋: `기능: 입력 검증과 프록시 무시 HTTP 설정 추가`

### Task 4: 렌더링, 마커, 출력 정리

**파일:** 생성 `internal/render/marker.go`, `internal/render/sanitize.go`, `internal/render/render.go`, 같은 이름의 `_test.go`, `internal/render/testdata/*.golden.md`
**Interfaces:** Produces
- `render.Marker(kind, scope string) string`, `render.HasMarker(body, kind, scope string) bool`
- `render.Sanitize(text string, maxLen int) string`
- `render.CodeReview(v CodeReviewView) string`, `render.Plan(v PlanView) string`

`CodeReviewView`: 기준 SHA, `[]model.CheckResult`, `[]model.Finding`, 잘림 여부, 버전 근거. `PlanView`: workspace, run URL, 기준 SHA, `model.PlanSummary`, 리스크 소견, 실패 분석(선택).
- [ ] 테스트(골든 파일 비교): 코드 리뷰 코멘트, plan 코멘트(정상), plan 코멘트(실패 분석), "plan 없음" 안내, "분석 실패" 안내
- [ ] 테스트(`Sanitize`, 표 기반): `@user` 무력화, 줄 첫머리 `/merge`·`/approve` 무력화, 마크다운 링크·이미지·`<details>`·`<img>`·`-->` 제거, 마커 문자열 제거, 길이 상한과 말줄임
- [ ] 테스트(Review Focus 5): 주소 `module.a["x|y`z"]`가 표 셀에서 이스케이프되고 코드 span이 깨지지 않으며, 개행이 포함된 주소는 한 줄로 정리된다
- [ ] 실패 확인 → 구현(`text/template`, 표 셀 이스케이프 함수 분리) → 통과 확인: `go test ./internal/render/... -cover`
- [ ] 커밋: `기능: 코멘트 렌더링과 출력 정리 추가`

### Task 5: GitLab 클라이언트와 게시

**파일:** 생성 `internal/gitlab/client.go`, `internal/gitlab/client_test.go`, `internal/publish/publish.go`, `internal/publish/publish_test.go`
**Interfaces:** Produces
- `gitlab.NewClient(baseURL, tokenFile string, hc *http.Client) (*Client, error)`. 메서드: `CurrentUser`, `MergeRequest`(head SHA 포함), `MergeRequestPipelines`, `MergeRequestDiffs`, `FileAtRef`, `PipelineJobs`, `JobArtifact`, `CommitStatuses`, `ListNotes`, `CreateNote`, `UpdateNote`, `DeleteNote`
- `publish.NoteStore`(note 관련 메서드, `CurrentUser`, `MergeRequest`만 담은 인터페이스), `publish.Upsert(ctx, store NoteStore, mrIID int, wantSHA, marker, body string) error`, `publish.ErrStale`
- [ ] 테스트(`httptest`): 목록 페이지네이션, 429·5xx 제한적 재시도 후 오류 전파, 토큰이 요청 헤더에만 있고 오류 메시지·로그에 없음. 엔드포인트 경로는 구현 시 GitLab 공식 문서로 확인한다.
- [ ] 테스트(`Upsert`, 가짜 store): 기존 bot note 갱신, 없으면 생성, head SHA 불일치 시 게시하지 않고 `ErrStale`, 비bot 작성자의 마커 note 무시, 중복 시 note ID가 가장 작은 bot note를 남기고 나머지 삭제 후 그 note를 갱신, **동시 실행 시나리오**(두 `Upsert`가 번갈아 실행돼도 최종 bot note는 1개), 실행 중 bot note가 삭제됐으면 새로 생성(Review Focus 3)
- [ ] 실패 확인 → 구현 → 통과 확인: `go test ./internal/gitlab/... ./internal/publish/... -cover`
- [ ] 커밋: `기능: GitLab 클라이언트와 note 게시 추가`

### Task 6: 정적 검사 결과 계약과 파서

**파일:** 생성 `internal/staticcheck/staticcheck.go`, `internal/staticcheck/staticcheck_test.go`, `internal/staticcheck/testdata/*`
**결정 사항:** CI의 정적 검사 job은 네 도구(fmt, validate, tflint, trivy) 결과를 하나의 JSON artifact로 모아 남긴다. 파일명은 상수 `staticcheck.ArtifactName`이고, 형식은 도구별 `{tool, passed, summary}` 목록이다. 이 계약의 정의와 파서가 이 Task의 산출물이며, CI 템플릿(Task 16)이 이 형식을 만든다.
**Interfaces:** Produces `staticcheck.ArtifactName string`, `staticcheck.Parse(raw []byte) ([]model.CheckResult, error)`
- [ ] 테스트: `TestParse_AllTools`, `TestParse_MissingToolIsReportedAsUnknown`(일부 도구만 있어도 오류 없이 나머지 반환), `TestParse_EmptyArtifact`(artifact 없음은 빈 결과로 처리하고 호출 측이 "정적 검사 결과 없음"을 표시), `TestParse_RejectsMalformedJSON`, `TestParse_TruncatesLongSummary`
- [ ] 실패 확인 → 구현 → 통과 확인: `go test ./internal/staticcheck/... -cover`
- [ ] 커밋: `기능: 정적 검사 artifact 계약과 파서 추가`

### Task 7: MCP 래퍼

**파일:** 생성 `internal/tfmcp/client.go`, `internal/tfmcp/client_test.go`, `internal/tfmcp/testdata/fakeserver/main.go`(테스트용 가짜 stdio 서버). 패키지 이름은 go-sdk의 `mcp`와 겹치지 않게 `tfmcp`로 한다.
**Interfaces:** Produces `tfmcp.Start(ctx, cfg config.Config, tfeTokenFile string) (*Client, error)`, `(*Client).VerifyTools(ctx) error`, `(*Client).Call(ctx, logicalName string, args map[string]any) (string, error)`, `(*Client).Close() error`, `tfmcp.Caller`(`Call`만 가진 인터페이스).
허용 tool 목록에는 진단용 `whoami`와 `get_token_permissions`를 포함한다.
- [ ] 테스트(가짜 서버): `TestStart_ChildEnvIsMinimal`(부모 env의 임의 변수가 자식에 전달되지 않고 `TFE_ADDRESS`는 고정값), `TestStart_ChildStderrNotForwarded`(자식 stderr 출력이 부모 로그에 나타나지 않음), `TestVerifyTools_FailsWhenAllowlistMissing`, `TestCall_RetriesOnRateLimitText`(문구가 담긴 tool error는 backoff 후 재시도, 상한 후 오류), `TestCall_DoesNotRetryOtherErrors`, `TestCall_ErrorDoesNotLeakPayload`, `TestCall_LargeResponse`(Task 2에서 확인한 크기 범위의 응답을 받는다)
- [ ] 실패 확인 → 구현(`go-sdk`의 command transport, 논리 이름→실제 이름 매핑은 설정에서, stderr는 폐기하거나 크기 제한 후 마스킹 로거로만 전달) → 통과 확인: `go test ./internal/tfmcp/... -cover`
- [ ] 커밋: `기능: stdio MCP 래퍼 추가`

### Task 8: HCL 변경 추출

**파일:** 생성 `internal/hclscan/scan.go`, `internal/hclscan/providers.go`, 각 `_test.go`, `internal/hclscan/testdata/*`
**Interfaces:** Produces
- `hclscan.ChangedResources(diffs []gitlab.FileDiff, head FileReader) (Changes, error)` — `Changes{Resources []ResourceChange, Modules []ModuleCall}`. `ResourceChange{File, Type, Name, ChangedAttrs []string, Removed bool}`, `ModuleCall{File, Name, Source, Version}`. `.tf` 이외 파일과 `.tfvars`는 무시한다.
- `hclscan.ProviderFor(rc ResourceChange, providerMetaArg string, decls ProviderDecls) (namespace, name string)` — resource type 접두사를 provider 로컬 이름으로, `required_providers`의 source로 namespace를 정한다. 선언이 없으면 `hashicorp`를 기본으로 한다. `provider =` 메타 인자(`google-beta` 등)를 우선한다.
- `hclscan.ParseProviderDecls(files map[string][]byte) ProviderDecls`
- [ ] 테스트: 추가·수정된 속성 추출, 블록 삭제는 `Removed=true`, 중첩 블록 속성, module 호출의 source와 version 추출, `.tfvars`·문서만 변경이면 빈 결과(Review Focus 2의 입력), provider 매핑(`google_compute_instance`→`hashicorp/google`, `provider = google-beta` 메타 인자, `required_providers`에 다른 namespace가 선언된 경우, provider alias)
- [ ] 실패 확인 → 구현(`hashicorp/hcl/v2/hclsyntax`, diff 추가 줄 범위와 속성 소스 범위의 교차로 변경 속성 판정) → 통과 확인: `go test ./internal/hclscan/... -cover`
- [ ] 커밋: `기능: HCL 변경과 provider 매핑 추출 추가`

### Task 9: provider 버전 결정과 문서 조회

**파일:** 생성 `internal/tfdocs/versions.go`, `internal/tfdocs/lookup.go`, 각 `_test.go`, `internal/tfdocs/testdata/compute_instance_search.txt`, `internal/tfdocs/testdata/compute_instance_doc.md`
**Interfaces:** Produces
- `tfdocs.ResolveVersion(ctx, c tfmcp.Caller, changedFile string, head FileReader, namespace, name string) (model.ProviderRef, Basis, error)` — `Basis`는 `lock`·`constraint`·`none`
- `tfdocs.Lookup(ctx, c tfmcp.Caller, p model.ProviderRef, resourceType string, attrs []string, maxBytes int) (Excerpt, error)`

**버전 규칙:** 변경 파일에서 가장 가까운 상위 디렉터리의 `.terraform.lock.hcl`을 따른다. lock이 없으면 `get_latest_provider_version`의 결과가 `required_providers` 제약을 만족할 때만 그 버전을 쓰고(`constraint`), 만족하지 않거나 제약이 없으면 `none`으로 표시해 문서 근거 없이 리뷰한다.
- [ ] 테스트(`ResolveVersion`, 가짜 Caller): lock 우선, 여러 root module의 가장 가까운 lock 선택, lock 없음+최신이 제약 만족, lock 없음+최신이 제약 불만족→`none`, 제약 없음→`none`
- [ ] 테스트(`Lookup`): 검색 결과의 유사 항목 중 title이 정확히 일치하는 것만 선택(`compute_instance`가 `compute_instance_group`보다 우선), 지정 버전으로 호출, 변경 attribute 설명과 deprecation 문구만 발췌, 상한 초과 시 잘림 표시(Review Focus 4), 같은 리소스 재조회는 Caller를 다시 호출하지 않음(캐시)
- [ ] 실패 확인 → 구현 → 통과 확인: `go test ./internal/tfdocs/... -cover`
- [ ] 커밋: `기능: provider 버전 결정과 문서 조회 추가`

### Task 10: plan 분석(run 식별, 대기, 집계, 로그 추출)

**파일:** 생성 `internal/plan/discover.go`, `internal/plan/wait.go`, `internal/plan/aggregate.go`, `internal/plan/logs.go`, 각 `_test.go`, `internal/plan/testdata/plan_with_sensitive.json`, `internal/plan/testdata/statuses.json`
**Interfaces:** Produces
- `plan.FindRun(statuses []gitlab.CommitStatus, src Source) (RunRef, bool)` — `Source{Host, Org string, Workspaces []string}`. `target_url`의 host·org·workspace를 설정값과 대조해 일치하는 것만 인정한다. 형태는 Task 2에서 확정한 것을 따른다.
- `plan.StatusSource`(`CommitStatuses(ctx)`만 가진 인터페이스), `plan.Wait(ctx, ss StatusSource, c tfmcp.Caller, src Source, w config.Waits, clock Clock) (RunState, error)` — 초기 대기 안에 일치하는 status가 없으면 `ErrNoPlan`, queued와 실행 중을 구분
- `plan.VerifyRunSHA(runDetails string, wantSHA string) error` — run의 commit이 MR head와 다르면 오류
- `plan.Aggregate(planJSON []byte, maxTargets int) (model.PlanSummary, error)`
- `plan.ExtractErrorBlocks(log string, maxBytes int) []string`
- [ ] 테스트(`Aggregate`, Review Focus 1): `plan_with_sensitive.json`(prior_state와 변경 전후에 알려진 비밀 문자열 포함)을 집계한 `PlanSummary`를 직렬화한 결과에 비밀 문자열이 없음. replace 판정(`delete`+`create`), no-op 제외, 상한 초과 시 `Truncated`
- [ ] 테스트(`FindRun`): 다른 org·workspace·host의 status 무시(위조 방어), 일치하는 것 반환
- [ ] 테스트(`Wait`, 가짜 시계): status 없음→`ErrNoPlan`, queued 후 running 후 완료, 전체 대기 초과→시간 초과 오류, 컨텍스트 취소
- [ ] 테스트(`VerifyRunSHA`): 일치/불일치
- [ ] 테스트(`ExtractErrorBlocks`): 여러 에러 블록 추출, 바이트 상한, 에러가 없으면 빈 결과
- [ ] 실패 확인 → 구현 → 통과 확인: `go test ./internal/plan/... -cover`
- [ ] 커밋: `기능: plan 분석 로직 추가`

### Task 11: LLM 서술 (ADK 격리)

**파일:** 생성 `internal/llm/core.go`, `internal/llm/adk.go`, `internal/llm/prompts.go`, `internal/llm/core_test.go`
**구조:** 프롬프트 조립, 응답 파싱, 리소스 주소 대조는 ADK와 무관한 순수 함수(`core.go`)로 두고 여기서 모두 테스트한다. `adk.go`는 `llmagent`와 `runner`를 호출하는 얇은 어댑터이며 호출마다 새 in-memory session을 만들고 tool은 등록하지 않는다. 구조화 출력과 모델 주입 방식은 Task 2에서 확정한 것을 따른다.
**Interfaces:** Produces `llm.Narrator`(인터페이스)
- `ReviewFindings(ctx, in ReviewInput) ([]model.Finding, error)`
- `PlanRisks(ctx, s model.PlanSummary) ([]string, error)`
- `PlanFailure(ctx, blocks []string) (string, error)`

순수 함수: `BuildReviewPrompt(in ReviewInput) string`, `ParseFindings(raw string, knownAddrs map[string]bool) ([]model.Finding, error)`
- [ ] 테스트(`core.go`): 프롬프트에 입력 문자열이 포함되고 `.tfvars` 내용과 plan 값이 포함되지 않음, `ParseFindings`가 응답 파싱 실패 시 오류, 입력에 없는 리소스 주소를 가리키는 소견은 제거
- [ ] 실패 확인 → 구현 → 통과 확인: `go test ./internal/llm/... -cover`
- [ ] 실제 모델 1회 호출 확인(사용자 환경, ADC): 작은 입력으로 `adk.go`의 `ReviewFindings`가 구조화된 응답을 반환. 결과를 `phase0.md`에 추가
- [ ] 커밋: `기능: ADK 기반 서술 생성기 추가`

### Task 12: 코드 리뷰 흐름

**파일:** 생성 `internal/review/code.go`, `internal/review/code_test.go`, `internal/review/deps.go`
**Interfaces:** Produces `review.Deps`(gitlab 읽기 인터페이스와 `publish.NoteStore`, `tfmcp.Caller`, `llm.Narrator`, 설정, 시계, 로거), `review.RunCode(ctx, d Deps, in input.Input) error`. 소비: Task 4~11.
**동작:** `.tf` 변경 추출 → 없으면 정상 종료(코멘트·LLM 호출 없음) → provider 매핑과 버전 결정 → 문서 조회 → 정적 검사 artifact 읽기 → LLM 소견 → 렌더링 → 게시.
- [ ] 테스트(가짜 의존성): 정상 경로 코멘트 1개 생성과 재실행 시 수 불변, `.tf` 변경 없음→게시·LLM 호출 없음(Review Focus 2), 입력이 상한 초과→잘림 표시(Review Focus 4), 버전 `none`일 때 코멘트에 근거 없음 표시, LLM 오류→"분석 실패" 안내 후 오류 반환, 정적 검사 artifact 없음→"결과 없음" 표시
- [ ] 실패 확인 → 구현(단계별 함수 분리, 각 30줄 이하) → 통과 확인: `go test ./internal/review/... -cover`
- [ ] 커밋: `기능: 코드 리뷰 흐름 추가`

### Task 13: plan 분석 흐름

**파일:** 생성 `internal/review/plan.go`, `internal/review/plan_test.go`
**Interfaces:** Produces `review.RunPlan(ctx, d Deps, in input.Input) error`. 소비: Task 10, 11, 4, 5.
**동작:** commit statuses로 run 식별 → `ErrNoPlan`이면 "plan 없음" 안내 게시 후 종료 → 대기 → `VerifyRunSHA` → plan JSON 집계 → LLM 리스크 소견 → 렌더링과 게시. plan 실패면 로그에서 에러 블록만 추출해 LLM 분석을 게시한다. workspace별 코멘트.
- [ ] 테스트(가짜 의존성, 종단): 정상 경로, plan 없음, plan 실패→에러 블록 기반 분석, run의 SHA가 MR head와 다르면 게시하지 않음, 시간 초과→안내 코멘트, 여러 workspace→workspace별 코멘트(마커 scope 구분)
- [ ] 테스트(Review Focus 1): 로그 출력(버퍼에 기록)과 LLM 입력과 게시 본문에 토큰 문자열, plan 비밀 문자열, MCP 응답 원문이 없음
- [ ] 실패 확인 → 구현 → 통과 확인: `go test ./internal/review/... -cover`
- [ ] 커밋: `기능: plan 분석 흐름 추가`

### Task 14: 진입점과 전체 게이트

**파일:** 생성 `cmd/tf-review/main.go`, `internal/review/run.go`, `internal/review/run_test.go`
**Interfaces:** Produces `review.Run(ctx, d Deps, in input.Input) error`(시작 검증 후 `RunCode`, `RunPlan` 순서 실행), `main`은 설정 로드, 프록시 무시 설정, 토큰 파일 읽기와 로거에 비밀 등록, 의존성 조립, 프로세스 deadline 적용, `Run` 호출만 한다.
**시작 검증:** SHA가 현재 MR head인지, pipeline ID가 해당 MR의 pipeline인지, MR의 project가 허용 project인지 확인 → MCP `VerifyTools` → `whoami`와 `get_token_permissions`는 진단용으로 호출해 로그만 남기고 실패 조건으로 쓰지 않는다.
- [ ] 테스트: 허용되지 않은 project의 MR→즉시 실패, SHA 불일치→종료, pipeline이 MR 것이 아님→실패, 진단 호출 실패는 무시, 코드 리뷰가 실패해도 plan 흐름은 시도하고 마지막에 오류 종료, deadline 초과 시 취소
- [ ] 전체 게이트: `go test ./... -cover` 전체 PASS와 전체 커버리지 80% 이상, `go vet ./...`, `gofmt -l .` 출력 없음
- [ ] 커밋: `기능: job 진입점과 전체 흐름 추가`

---

## Part 2. 배포 산출물과 파일럿 검증

### Task 15: 컨테이너 이미지

**파일:** 생성 `build/Dockerfile`, `build/config.json`(이미지에 포함할 운영 설정 예시, 비밀 없음), `build/README.md`(빌드와 푸시 절차)
**결정 사항:** 멀티스테이지 빌드. 빌드 단계에서 Go 바이너리를 정적 빌드하고, `hashicorp/terraform-mcp-server:1.4.1` 이미지에서 `/bin/terraform-mcp-server`를 복사한다. 최종 이미지는 셸 없는 최소 베이스(비루트)이며 CA 인증서를 포함한다. 이미지 태그는 커밋 SHA다.
- [ ] 이미지 빌드: `docker build -f build/Dockerfile -t tf-review:local .` → 성공
- [ ] 스모크: 잘못된 입력(`docker run --rm tf-review:local --mr-iid x`)이 종료 코드 비0과 입력 오류 메시지로 끝나고, `docker image ls`로 크기를 기록
- [ ] 이미지에 토큰이나 개인 정보가 없음을 `docker history`와 파일 목록으로 확인
- [ ] 커밋: `설정: 컨테이너 이미지 빌드 추가`

### Task 16: GitLab CI 템플릿과 필요 리소스 문서

**파일:** 생성 `ci/tf-review.gitlab-ci.yml`(재사용 템플릿), `docs/operations/required-resources.md`, `ci/template_test.go`
**템플릿 내용:** (1) 정적 검사 job: fmt, `init -backend=false` 후 validate, tflint, trivy. 결과를 `staticcheck.ArtifactName`과 같은 파일명의 단일 JSON artifact로 남기고 보존 기간은 job 최대 실행 시간보다 길게 둔다. (2) 트리거 job: 정적 검사 job에 의존, MR pipeline에서만 실행(workflow 규칙), `allow_failure: true`, GitLab ID token으로 WIF 인증 후 job을 실행하며 args로 MR iid, head SHA, pipeline ID만 전달한다. WIF 조건은 Task 2에서 확인한 claim 기준이다.
**필요 리소스 문서:** WIF pool·provider(조건은 `namespace_id`·`project_id`), 트리거 SA(`roles/run.jobsExecutorWithOverrides`를 job 단위로), job SA(`roles/aiplatform.user`, 두 secret 접근 권한), Secret 2개(볼륨 마운트), Cloud Run Job(task timeout 45분, 재시도 0, 이미지), Artifact Registry 저장소, 실패 알림을 표로 정리한다.
- [ ] 테스트(`template_test.go`): 템플릿 YAML에 `staticcheck.ArtifactName` 문자열이 포함, 트리거 job이 MR pipeline 규칙과 `allow_failure`를 가짐, 트리거 args가 허용된 세 값 외에 없음
- [ ] 템플릿 문법 검증: 파일럿 repository에서 파이프라인이 생성되는지 확인
- [ ] 커밋: `설정: CI 템플릿과 필요 리소스 문서 추가`

### Task 17: 파일럿 종단 검증 (사용자 협조 필요)

**파일:** 수정 `docs/superpowers/notes/phase0.md`(결과 추가)
- [ ] 이미지를 파일럿 프로젝트 Artifact Registry에 푸시하고 사용자가 리소스를 구성한다(`required-resources.md` 기준). 파일럿은 prod가 아닌 workspace로 한다.
- [ ] **공격 시나리오 재검증:** 실제 job에서 override로 env(`TFE_ADDRESS`, 토큰 관련 변수, `HTTPS_PROXY` 등)와 task 수를 바꿔 호출했을 때 토큰이 다른 곳으로 전송되지 않고 실행이 즉시 거부되는지 확인한다.
- [ ] 정상 MR 종단: 코드 리뷰 코멘트와 plan 코멘트가 게시되고, 재push 시 코멘트 수가 늘지 않으며, 연속 push에서 오래된 실행이 게시하지 않는다.
- [ ] plan이 없는 MR, plan 실패 MR, `.tf` 이외만 변경한 MR 동작을 확인한다.
- [ ] MR당 실제 토큰 사용량과 비용을 측정해 기록하고, 필요하면 모델 변경 여부를 사용자와 논의한다.
- [ ] job이 실패했을 때 구조화된 오류 로그가 남고 알림이 오는지 확인한다.
- [ ] 커밋: `문서: 파일럿 검증 결과 기록`

## 자체 검토

- **설계 커버리지:** 트리거·override 방어(2, 3, 14, 16, 17), 입력 검증(3), 코드 리뷰 흐름(8, 9, 11, 12), plan 흐름(10, 11, 13), 코멘트 규칙·마커·정리(4, 5), 정적 검사 계약(6, 16), 보안 요구(1, 3, 7, 13, 14), 제약(5, 7, 10), Phase 0(2, 17)을 각 Task가 소유한다.
- **타입 일관성:** 공용 타입은 Task 1의 `internal/model`에 있고 Task 4, 6, 10, 11, 12, 13이 같은 타입을 쓴다. `tfmcp.Caller`는 Task 7에서 정의하고 Task 9, 10이 소비한다. `publish.NoteStore`는 Task 5에서 정의하고 Task 12, 13이 소비한다.
- **순서:** 선행 정의가 후행 Task보다 앞에 있다. Task 3~6은 Task 2와 병렬 가능하다.
- **비례:** 계획의 대부분은 파일, 시그니처 수준의 인터페이스, 테스트 이름과 검증 기준이다.

## Devil's Advocate Review

- **도전한 가정:** (1) 이미지 설정과 볼륨 토큰만으로 override 방어가 충분하다 → 예약 env 덮어쓰기와 프록시 env가 우회 경로일 수 있어 Task 2(더미 job 검증), Task 3(프록시 무시, task index 검사)로 보강했다. (2) 파일럿에 lock 파일이 있다 → 없을 때의 폴백 규칙을 확정했다(Task 9). (3) HCP status가 `target_url`로 run을 가리키고 신뢰할 수 있다 → 위조·불일치 방어를 추가했다(Task 10).
- **검토한 대안:** Phase 0을 사용자 협조 불필요(A)와 필요(B)로 나눠 병렬 진행을 허용했다(채택). 공용 타입을 별도 패키지로 앞에 두었다(채택). module 문서 조회를 1차에 넣는 안은 기각했다(source·버전 추출은 맥락으로만 사용하고 조회는 F8·F11 범위).
- **식별된 리스크와 완화:** 게이트 항목 누락→Task 2에 spec 10절 전 항목과 중단 조건 추가. 타입·순서 역전→`internal/model` 도입과 Task 재배열. 동시 실행 note 경합→생존자 규칙(최소 ID)과 테스트. 정적 검사 계약 부재→Task 6 신설. 로거 부재→Task 1. Task 10 과대→Task 12·13·14로 분할. ADK 어댑터 미검증→순수 함수 분리와 실제 호출 1회 확인.
- **수용하지 않은 부분과 이유:** spec의 "추가 env가 있으면 즉시 실패"는 Cloud Run이 다수의 env를 주입하므로 추가 env의 정당성을 판별할 수 없어 구현하지 않고, "코드가 읽는 env를 고정 목록으로 제한 + 예약 env 검사 + 프록시 무시"로 같은 목적을 달성한다(spec도 이에 맞춰 수정).
- **결론:** 위 보강으로 게이트와 의존 순서가 정리되어 현재 계획이 spec을 구현하는 최소 경로다. 남은 불확실성은 Task 2의 실제 검증 결과에 달려 있다.
