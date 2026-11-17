# Phase 0 검증 결과

작성일: 2026-11-17 · 대상: 구현 계획 Task 2 · 범위: A 항목(사용자 협조 불필요). B 항목과 실제 Gemini 호출은 미수행이다.

응답 원문은 남기지 않고 구조만 기록한다. 이번 검증에는 TFE token, GitLab token, API key를 쓰지 않았다. 스파이크 코드는 저장소 밖 임시 디렉터리에 두었고 커밋하지 않는다.

## 요약

| 항목 | 결과 | 영향받는 Task |
| --- | --- | --- |
| A1 tool 이름과 `--tools` | 문서 tool 이름은 `search_provider_docs`, `get_provider_details`, `get_latest_provider_version`이다. 계획의 HCP tool 이름 6개는 모두 서버가 인정하는 유효한 이름이다. `--tools`로 `tools/list`를 제한하는 것도 확인했다. 다만 token이 없으면 HCP tool은 목록에 나오지 않는다. | Task 1, 7, 9, 10 |
| A2 ADK Go v2 API | 계획한 구성(tool 없는 `llmagent`, in-memory session의 `runner`, Vertex Gemini, `OutputSchema`, 가짜 `model.LLM` 주입)이 모두 동작한다. 가짜 모델 테스트도 통과했다. **최신 v2.8.0은 Go 1.26.10 이상을 요구해 전역 제약(Go 1.26.4)과 충돌한다.** | Task 11, 저장소 `go.mod` |
| A3 Gemini 실제 호출 | 사용자 게이트로 미뤘다. 호출에 필요한 값은 아래에 정리했다. | Task 11 |

**결정 필요(controller/사용자):** Go 버전과 ADK 버전 조합(A2-1절), Vertex 리전(A3절).

---

## A1. 실제 tool 이름과 `--tools` 인자 형식

### 환경

- 이미지 `hashicorp/terraform-mcp-server:1.4.1` (digest `sha256:11dd418bba6003dd5777e9d0979b68de59929a155b355f338f407947e00e9d80`, arm64로 받음).
- entrypoint는 `/bin/terraform-mcp-server`, CMD는 없고 shell도 없다. `--version` 출력은 `Version: 1.4.1`이다.
- 모든 실행에 token을 넘기지 않았다. `docker run`은 호스트 env를 넘기지 않는다.

### 명령

```sh
docker pull hashicorp/terraform-mcp-server:1.4.1
docker run --rm hashicorp/terraform-mcp-server:1.4.1 --help
docker run --rm hashicorp/terraform-mcp-server:1.4.1 stdio --help

# stdin으로 아래 세 줄의 JSON-RPC를 보내고 4초 뒤 stdin을 닫는다
#   {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"phase0-probe","version":"0.0.1"}}}
#   {"jsonrpc":"2.0","method":"notifications/initialized"}
#   {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
docker run -i --rm hashicorp/terraform-mcp-server:1.4.1 [전역 플래그] stdio [전역 플래그]
```

조합별로 실행한 플래그:

1. 플래그 없음(기본 `--toolsets=all`)
2. `--tools=search_provider_docs,get_provider_details`
3. `--tools=search_provider_docs,get_latest_provider_version,list_runs,get_run_details,get_plan_json_output,get_plan_logs,whoami,get_token_permissions`
4. `--tools=no_such_tool_xyz,search_modules` (잘못된 이름을 어떻게 처리하는지 확인)
5. 후보 이름 묶음(아래 "유효한 이름 목록" 참고)
6. `stdio --tools=search_provider_docs,get_provider_details` (플래그를 서브커맨드 뒤에 두고 `HOME=/nonexistent`로 실행)

### `--tools` 형식

- `--tools`는 **전역 플래그**다. 쉼표로 구분한 개별 tool 이름 목록이며 공백은 넣지 않는다. 예: `--tools=a,b,c`
- 서브커맨드 앞(`--tools=... stdio`)에 두든 뒤(`stdio --tools=...`)에 두든 결과가 같다(조합 2와 6).
- help 문구에 "Cannot be used together with --toolsets flag"라고 나온다. `--toolsets`의 기본값은 `all`이며, `--tools`를 주면 `--toolsets`는 적용되지 않는다.
- **잘못된 이름은 조용히 무시된다.** stderr에 `level=warning msg="Invalid tool names ignored: [...]"`만 찍히고, 프로세스는 정상 기동하며 나머지 이름으로 동작한다(조합 4). 계획상 자식 stderr는 폐기하므로 오타가 있어도 알아챌 수 없다. 따라서 Task 7의 `VerifyTools`(`tools/list` 결과가 허용 목록을 모두 포함하는지 확인)는 생략할 수 없는 필수 검사다. 설계 판단을 그대로 뒷받침하는 결과다.
- stderr에 `Enabled individual tools: [...]` 로그가 찍히므로 유효한 이름인지 판별할 수 있다.

### `initialize` 응답 구조

- `protocolVersion`: `2025-06-18`
- `serverInfo`: `{name: terraform-mcp-server, version: 1.4.1}`
- `capabilities` 키: `resources`, `tools`, `elicitation`

### token 없이 `tools/list`에 나오는 tool (조합 1, 9개)

| tool | 필수 인자 | 선택 인자 |
| --- | --- | --- |
| `get_latest_module_version` | `module_publisher`, `module_name`, `module_provider` | — |
| `get_latest_provider_version` | `namespace`, `name` | — |
| `get_module_details` | `module_id` | — |
| `get_policy_details` | `terraform_policy_id` | — |
| `get_provider_capabilities` | `namespace`, `name` | `version` |
| `get_provider_details` | `provider_doc_id` | — |
| `search_modules` | `module_query` | `current_offset` |
| `search_policies` | `policy_query` | — |
| `search_provider_docs` | `provider_name`, `provider_namespace`, `service_slug`, `provider_document_type` | `provider_version` |

- provider 문서 tool은 **`search_provider_docs`와 `get_provider_details`**다. `search_providers`라는 이름은 없다(잘못된 이름으로 무시됨).
- **`get_latest_provider_version`은 있다.**
- `search_provider_docs.provider_document_type`의 enum은 `resources`, `data-sources`, `functions`, `guides`, `overview`, `actions`, `list-resources`이다. `provider_version`은 `x.y.z` 또는 `latest`를 받는다.
- 9개 모두 annotation이 `readOnlyHint: true`, `destructiveHint: false`이다.

### `--tools` 제한 확인

- 조합 2에서는 `tools/list`에 정확히 2개(`get_provider_details`, `search_provider_docs`)만 나왔다. 제한이 동작한다.
- 조합 3에서는 8개 이름을 모두 유효하다고 받아들였다(경고 없음, `Enabled individual tools`에 8개 모두 표시). 그러나 `tools/list`에는 registry tool 2개(`get_latest_provider_version`, `search_provider_docs`)만 나왔다.
- 서버 로그는 `NewSessionHandler failed to create TFE client ... credentials.tfrc.json: no such file or directory`, `Session has no valid TFE client - TFE tools will not be available`였다. 즉 **HCP tool(`list_runs`, `get_run_details`, `get_plan_json_output`, `get_plan_logs`, `whoami`, `get_token_permissions`)은 유효한 이름이지만 TFE client가 있을 때만 `tools/list`에 나온다.** token을 준 상태의 목록과 입력 스키마는 B 항목이다.

### 유효한 이름 목록 (참고용, 불완전할 수 있음)

이미지에서 바이너리를 꺼냈다(`docker create` → `docker cp`). 바이너리 문자열에서 동사 접두사(`get_`, `list_`, `search_`, `create_` 등)로 후보 114개를 뽑아 `--tools`에 넣었고, 서버가 유효하다고 판정한 이름은 53개였다. token을 준 `tools/list`가 아니며, 접두사 패턴에 걸리지 않은 이름은 빠졌을 수 있다.

> `action_run`, `add_team_member`, `attach_variable_set_to_workspaces`, `create_no_code_workspace`, `create_project`, `create_run`, `create_team`, `create_variable_in_variable_set`, `create_variable_set`, `create_workspace`, `create_workspace_tags`, `create_workspace_variable`, `delete_project`, `delete_team`, `delete_workspace_safely`, `force_unlock_workspace`, `get_apply_details`, `get_apply_logs`, `get_latest_module_version`, `get_latest_provider_version`, `get_module_details`, `get_plan_details`, `get_plan_json_output`, `get_plan_logs`, `get_policy_details`, `get_private_module_details`, `get_private_provider_details`, `get_project`, `get_provider_capabilities`, `get_provider_details`, `get_run_comments`, `get_run_details`, `get_sentinel_mock`, `get_stack_details`, `get_state_version`, `get_team`, `get_token_permissions`, `get_workspace_details`, `list_runs`, `list_stacks`, `list_state_versions`, `list_teams`, `list_terraform_orgs`, `list_terraform_projects`, `list_workspace_variables`, `list_workspaces`, `search_modules`, `search_policies`, `search_private_modules`, `search_private_providers`, `search_provider_docs`, `update_workspace`, `update_workspace_variable`, `whoami`

무효로 판정된 이름 중 혼동하기 쉬운 것: `search_run`, `search_runs`, `search_workspaces`, `get_plan_json`, `search_providers`, `get_provider_doc`. 서버의 자체 사용 안내문에는 `search_run`, `search_workspaces` 같은 이름이 나오지만 v1.4.1에서는 무효다. 안내문 대신 이 결과를 따른다.

`create_run`, `action_run`, `delete_*`, `update_*` 등 쓰기 tool이 같은 바이너리에 있다. 따라서 `--tools` 제한은 실제 보안 경계다.

### 문서 조회 응답 형태 (registry, token 불필요)

Task 9 테스트 데이터를 만들 때 참고하도록 공개 registry 호출 3건의 구조를 확인했다.

- `search_provider_docs`(`hashicorp/google`, `compute_instance`, `resources`, `6.0.0`): `content[0]`이 text 한 개(약 2KB)다. 머리말 다음에 `---`로 구분한 항목이 이어지고, 항목마다 `providerDocID`, `Title`, `Category`, `Description` 네 줄이 있다. `compute_instance` 뒤에 `compute_instance_from_machine_image`, `compute_instance_from_template`, `compute_instance_group` 같은 유사 항목이 함께 나온다. 계획의 "title 정확 일치만 선택" 규칙이 필요하다는 것을 확인했다.
- `get_provider_details`(위 결과의 `compute_instance` doc ID): text 한 개, 약 37.7KB다. YAML front matter 다음에 Markdown이 오며, 제목은 `# google_compute_instance`, `## Example Usage`, `## Argument Reference`, `## Attributes Reference`, `## Timeouts`, `## Import` 순서다. 버전 지정은 search 단계의 `provider_version`으로 하고, details는 doc ID만 받는다.
- `get_latest_provider_version`(`hashicorp/google`): text 한 개, 버전 문자열만 들어 있다(예: `8.11.0`).
- 세 응답 모두 `isError`가 없었다.

### 추가 발견: 자식 프로세스의 두 번째 token 경로

token이 없으면 서버는 `$HOME/.terraform.d/credentials.tfrc.json`을 읽으려 한다. `HOME`을 지정하지 않으면 `/.terraform.d/...`, `HOME=/nonexistent`이면 `/nonexistent/.terraform.d/...`를 찾는다. Task 7에서 자식 env를 최소화할 때 `HOME`을 넘기지 않거나 존재하지 않는 경로로 고정해야 한다. 그래야 이미지나 마운트에 우연히 놓인 credentials 파일이 token 출처가 되지 않는다. 이 경로를 테스트(`TestStart_ChildEnvIsMinimal`)에서 함께 확인할 것을 권한다.

### 결정 사항

**`mcpToolAllowlist` 제안(논리 이름 → 실제 이름).** Task 9와 10이 호출하는 tool과 진단용 tool 2개만 넣는다.

| 논리 이름 | 실제 tool 이름 | 사용처 | token 필요 |
| --- | --- | --- | --- |
| `searchProviderDocs` | `search_provider_docs` | Task 9 `Lookup` | 아니오 |
| `getProviderDetails` | `get_provider_details` | Task 9 `Lookup` | 아니오 |
| `getLatestProviderVersion` | `get_latest_provider_version` | Task 9 `ResolveVersion` | 아니오 |
| `getRunDetails` | `get_run_details` | Task 10 `VerifyRunSHA`, 상태 확인 | 예 |
| `getPlanJSONOutput` | `get_plan_json_output` | Task 10 `Aggregate` 입력 | 예 |
| `getPlanLogs` | `get_plan_logs` | Task 10 `ExtractErrorBlocks` 입력 | 예 |
| `whoami` | `whoami` | Task 14 진단 | 예 |
| `getTokenPermissions` | `get_token_permissions` | Task 14 진단 | 예 |

- `get_plan_details`, `list_runs`는 넣지 않는다. B 항목에서 `get_plan_json_output`이 run ID 대신 plan ID를 요구하는 것으로 확인되면 `get_run_details` 응답에서 plan ID를 얻을 수 있는지 먼저 본다. 그것으로 안 될 때만 추가를 검토한다. 추가하게 되면 "허용 tool 확대"에 해당하므로 사용자와 합의해야 한다.
- run 식별은 commit status의 `target_url`로 하므로 `list_runs`는 필요 없다.

**정확한 `--tools` 값(Task 7 argv).**

```text
terraform-mcp-server stdio --tools=search_provider_docs,get_provider_details,get_latest_provider_version,get_run_details,get_plan_json_output,get_plan_logs,whoami,get_token_permissions
```

argv는 위 그대로 고정하고 `--toolsets`는 함께 쓰지 않는다. `--log-level`, `--log-file`도 넘기지 않는다(stderr는 폐기). 실제 구현에서는 설정의 allowlist 값으로 이 문자열을 만들고 정렬 순서는 상관없다.

**Task 7 `VerifyTools`.** token이 있는 실행에서는 `tools/list`에 8개가 모두 나와야 한다. token이 없거나 무효면 HCP tool 5개(`get_run_details`, `get_plan_json_output`, `get_plan_logs`, `whoami`, `get_token_permissions`)가 빠진다. 따라서 `VerifyTools` 실패는 "이름 오타"와 "TFE 인증 실패" 둘 다를 뜻한다. 오류 메시지에는 빠진 이름 목록을 넣는다(값은 넣지 않음). token이 있을 때 8개가 실제로 나오는지는 B 항목이다.

---

## A2. ADK Go v2 API

### 버전 확인

```sh
curl -s https://proxy.golang.org/google.golang.org/adk/v2/@v/list
curl -s https://proxy.golang.org/google.golang.org/adk/v2/@latest
curl -s https://proxy.golang.org/google.golang.org/adk/v2/@v/<ver>.mod   # go 지시어 확인
```

- 모듈 경로는 `google.golang.org/adk/v2`이고 저장소는 `github.com/google/adk-go`다. 전역 제약의 import 경로와 일치한다.
- 최신은 `v2.8.0`(2026-11-16 태그)이다. v1 계열(`google.golang.org/adk`)의 최신은 `v1.9.0`이다.
- 버전별 `go` 지시어:

| ADK 버전 | `go` 지시어 | 동반 genai |
| --- | --- | --- |
| v2.0.0 | 1.25.0 | v1.57.0 |
| v2.1.0, v2.2.0 | 1.26.5 | — |
| v2.3.0 ~ v2.6.0 | 1.26.6 | — |
| v2.7.0, v2.7.1, v2.8.0 | 1.26.10 | v1.77.0 (v2.8.0) |

### A2-1. Go 버전 충돌 (결정 필요)

전역 제약은 Go 1.26.4이고 저장소 `go.mod`도 `go 1.26.4`다. 로컬 `GOTOOLCHAIN=auto` 환경에서 `go get google.golang.org/adk/v2@v2.8.0`을 실행하자 `requires go >= 1.26.10; switching to go1.26.11`이 출력됐다. 툴체인이 자동으로 내려받아졌고 스파이크의 `go.mod`가 `go 1.26.10`으로 올라갔다. Go 1.26.4로 빌드할 수 있는 v2 릴리스는 **v2.0.0뿐**이다.

두 조합을 모두 빌드하고 테스트했다.

- v2.8.0 + go1.26.10(auto toolchain): `go vet`, `go build`, `go test` 통과.
- v2.0.0 + `GOTOOLCHAIN=local` go1.26.4: `go vet`, `go build`, `go test` 통과. 의존성 중 Go 1.26.4를 넘는 것은 없었다.

두 버전 사이의 API 차이(이번 스파이크 범위): **`agent.RunConfig.MaxLLMCalls` 필드가 v2.0.0에는 없다.** 나머지 사용 API(`llmagent.Config`, `OutputSchema`, `runner.New`, `session.InMemoryService`, `gemini.NewModel`, `model.LLM`)는 같은 코드로 컴파일되고 같은 동작을 했다.

| 선택지 | 장점 | 비용 |
| --- | --- | --- |
| (가) 저장소를 `go 1.26.10` 이상으로 올리고 ADK v2.8.0 사용 | 최신 수정과 `MaxLLMCalls`(호출당 LLM 호출 상한) 사용 가능. genai도 최신 | 전역 제약 "Go 1.26.4" 변경. 로컬은 auto toolchain이 해결하지만 CI와 빌드 이미지의 Go 버전도 1.26.10 이상으로 맞춰야 함. 이미 커밋된 Task 1 코드 영향은 `go.mod` 한 줄 |
| (나) Go 1.26.4를 유지하고 ADK v2.0.0에 고정 | 전역 제약 그대로 | 9개 마이너 버전 뒤처짐. `MaxLLMCalls` 없음(tool이 없는 단일 호출이라 실질 영향은 작음). 이후 보안 수정을 받으려면 결국 Go를 올려야 함 |

권고: (가). 근거는 두 가지다. tool 없는 단일 호출 구조라 어느 쪽이든 동작한다. 그리고 Go 패치 버전 상향은 되돌리기 쉬운 반면 오래된 ADK 고정은 나중에 이전 비용이 쌓인다. 다만 전역 제약 변경이라 controller와 사용자 결정 사항으로 남긴다. **이번 작업에서 저장소 `go.mod`는 수정하지 않았다.**

### 확인한 API (v2.8.0, `go doc`과 모듈 소스로 확인)

| 용도 | import 경로 | 식별자와 시그니처 |
| --- | --- | --- |
| 에이전트 | `google.golang.org/adk/v2/agent/llmagent` | `func New(cfg Config) (agent.Agent, error)` |
| 에이전트 설정 | 같은 패키지 | `Config{Name, Description string; Model model.LLM; Instruction string; GenerateContentConfig *genai.GenerateContentConfig; OutputSchema *genai.Schema; Tools []tool.Tool; Toolsets []tool.Toolset; IncludeContents IncludeContents; OutputKey string; Mode Mode; ...}` |
| 모델 인터페이스 | `google.golang.org/adk/v2/model` | `type LLM interface { Name() string; GenerateContent(ctx context.Context, req *LLMRequest, stream bool) iter.Seq2[*LLMResponse, error] }` |
| 요청·응답 | 같은 패키지 | `LLMRequest{Model string; Contents []*genai.Content; Config *genai.GenerateContentConfig; Tools map[string]any}`, `LLMResponse{Content *genai.Content; UsageMetadata *genai.GenerateContentResponseUsageMetadata; TurnComplete bool; ErrorCode, ErrorMessage string; FinishReason genai.FinishReason; ...}` |
| Gemini 모델 | `google.golang.org/adk/v2/model/gemini` | `func NewModel(ctx context.Context, modelName string, cfg *genai.ClientConfig) (model.LLM, error)` |
| 러너 | `google.golang.org/adk/v2/runner` | `func New(cfg Config) (*Runner, error)`, `Config{AppName string; Agent agent.Agent; SessionService session.Service; AutoCreateSession bool; ...}`, `func NewInMemory(appName string, a agent.Agent) (*Runner, error)`(문서상 개발·테스트용) |
| 실행 | 같은 패키지 | `func (r *Runner) Run(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig, opts ...RunOption) iter.Seq2[*session.Event, error]` |
| 세션 | `google.golang.org/adk/v2/session` | `func InMemoryService() Service`, `Service.Create(ctx, *CreateRequest) (*CreateResponse, error)`, `CreateRequest{AppName, UserID, SessionID string; State map[string]any}`, `CreateResponse{Session Session}`, `Session.ID()` |
| 이벤트 | 같은 패키지 | `Event{model.LLMResponse; Author string; ...}`, `func (e *Event) IsFinalResponse() bool` |
| 실행 설정 | `google.golang.org/adk/v2/agent` | `RunConfig{StreamingMode StreamingMode; SaveInputBlobsAsArtifacts bool; MaxLLMCalls int}` (`MaxLLMCalls`는 v2.8.0만 있음) |
| genai | `google.golang.org/genai` (v2.8.0 기준 v1.77.0) | `ClientConfig{Backend Backend; Project, Location string; Credentials *auth.Credentials; HTTPClient *http.Client; HTTPOptions HTTPOptions; ...}`, `BackendVertexAI`, `(*ClientConfig).UseDefaultCredentials() error` |

### 구조화 출력의 실제 동작

- `llmagent.Config.OutputSchema`(`*genai.Schema`)를 설정한다.
- **tool이 없으면** 프레임워크(`internal/llminternal/basic_processor.go`)가 요청 config에 `ResponseSchema = OutputSchema`, `ResponseMIMEType = "application/json"`을 직접 넣는다. 기반은 `GenerateContentConfig`의 복제본이라 원본은 변하지 않는다.
- tool이 있으면 `set_model_response` tool을 주입하는 우회 방식으로 바뀐다(`outputschema_processor.go`). 이 프로젝트는 tool을 등록하지 않으므로 해당하지 않는다. Task 11에서 tool을 0개로 유지하는 것이 이 경로를 보장한다.
- `ResponseJsonSchema`는 쓰지 않는다.

### 가짜 모델 주입: 가능

`llmagent.Config.Model`이 `model.LLM` 인터페이스라서 직접 만든 구현을 넣을 수 있다. 스파이크 테스트 2개가 모두 통과했다(v2.8.0, v2.0.0 공통).

```sh
go -C <scratchpad>/adk-spike vet ./... && go -C <scratchpad>/adk-spike test -v -count=1 ./...
GOTOOLCHAIN=local go -C <scratchpad>/adk-spike-v200 test -v -count=1 ./...
```

- `TestRunOnce_FakeModelThroughRunner`: 가짜 모델이 요청을 기록하고 미리 정한 JSON 텍스트를 반환한다. 다음을 확인했다.
  - runner의 최종 응답(`IsFinalResponse()` 이벤트의 텍스트)이 그 JSON과 같다.
  - 모델은 1회만 호출된다.
  - 요청 config에 `ResponseMIMEType == "application/json"`과 `ResponseSchema`(`findings` 속성 포함)가 들어 있다.
  - `GenerateContentConfig.Temperature`가 전달된다.
  - 요청에 tool이 0개다.
  - `Instruction`은 `SystemInstruction`으로 들어간다.
- `TestRunOnce_SessionIsolatedPerCall`: 호출마다 새 in-memory session을 만들면 두 번째 호출 요청의 `Contents`가 1개뿐이다. 이전 대화가 섞이지 않는다.

### Task 11 레시피(설계 수준)

- `adk.go`는 생성자로 `model.LLM`을 받는다. 운영 코드는 `gemini.NewModel`의 결과를, 테스트는 가짜를 넘긴다. 이렇게 하면 `adk.go`도 가짜로 단위 테스트할 수 있다(계획보다 커버리지 측면에서 유리).
- 호출마다 `session.InMemoryService()` → `runner.New(runner.Config{AppName, Agent, SessionService})` → `svc.Create(...)` → `r.Run(ctx, userID, sessionID, genai.NewContentFromText(prompt, genai.RoleUser), agent.RunConfig{...})` 순서로 진행한다. iterator에서 오류가 나오면 맥락을 붙여 반환한다. `IsFinalResponse()` 이벤트의 텍스트를 모아 `ParseFindings`에 넘긴다. `NewInMemory`는 문서에 개발·테스트용으로 명시돼 있으므로 쓰지 않는다.
- 에이전트는 `Tools` 없이 `OutputSchema`와 `GenerateContentConfig`(temperature 등)만 설정한다.
- 토큰 사용량은 최종 이벤트의 `UsageMetadata`에서 읽을 수 있다(MR당 비용 측정용, 값은 로그에 숫자만).

### Vertex 모델 생성 시 주의(genai v1.77.0 소스 확인)

- `genai.NewClient`는 `Backend == BackendVertexAI`이고 `Credentials`, `APIKey`, `HTTPClient`가 모두 비어 있을 때만 ADC(`credentials.DetectDefault`)를 찾는다. **`HTTPClient`를 직접 주면 인증을 붙이지 않는다.** 그래서 프록시를 무시하는 `http.Client`(`Transport.Proxy = nil`)를 쓰려면 `cfg.UseDefaultCredentials()`를 먼저 호출해야 한다. 이 함수가 ADC를 찾고 그 client에 인증 미들웨어를 붙인다. 스파이크에서 이 구성이 컴파일되는 것만 확인했고, 실제 호출은 A3에 포함된다.
- genai는 env에서 `GOOGLE_GENAI_USE_VERTEXAI`, `GOOGLE_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`, `GOOGLE_CLOUD_REGION`, `GOOGLE_VERTEX_BASE_URL` 등을 읽는다. override로 env를 바꿀 수 있는 위협 모델이므로 다음을 지킨다.
  - `Backend`, `Project`, `Location`을 설정 파일 값으로 명시한다(명시값이 env보다 우선).
  - **`HTTPOptions.BaseURL`을 명시한다**(예: `https://<location>-aiplatform.googleapis.com/`). 비워 두면 `GOOGLE_VERTEX_BASE_URL` env가 요청 주소를 바꿀 수 있다.
  - `APIKey`를 비워 두고 project와 location을 주면, env의 API key는 경고와 함께 무시된다.
- `gemini.NewModel`은 `genai.NewClient`를 그대로 호출하므로 생성 시점에 ADC 탐색이 일어난다. ADC가 없으면 생성이 실패한다. 테스트 경로에서는 호출하지 않는다.

---

## A3. Gemini 실제 호출: 사용자 게이트로 미룸

이번에는 호출하지 않았다. Task 11의 "실제 모델 1회 호출 확인" 단계에서 사용자 환경(ADC)으로 수행하고 이 문서에 결과를 추가한다.

| 항목 | 값 |
| --- | --- |
| 모델 ID | `gemini-3.8-flash` (`-latest` 별칭 금지) |
| API 표면 | Vertex AI `generateContent` (genai `BackendVertexAI`, ADK `model/gemini`) |
| 인증 | ADC. 로컬은 `gcloud auth application-default login`, job은 부착 SA. 역할 `roles/aiplatform.user` |
| project | 대상 Google Cloud project ID (사용자 제공) |
| location | **미정. 사용자 결정 필요.** 후보: `global`(BaseURL `https://aiplatform.googleapis.com/`) 또는 특정 리전(BaseURL `https://<region>-aiplatform.googleapis.com/`). 해당 리전에서 `gemini-3.8-flash`를 제공하는지가 확인 대상 |
| 호출 방법 | 위 레시피의 `newVertexModel(ctx, project, location, "gemini-3.8-flash")`로 `model.LLM`을 만들어 `OutputSchema`를 설정한 agent에 넣는다. 작은 입력 한 건으로 `runner.Run`을 1회 실행한다 |
| 확인할 것 | (1) 모델 사용 가능 여부(404/권한 오류 없음) (2) 응답이 `OutputSchema`를 따르는 JSON인지(`ResponseSchema` + `application/json` 지원) (3) `UsageMetadata`의 입력·출력 토큰 수(MR당 비용 추정) |

---

## B 항목 (사용자 협조 필요)

| 항목 | 상태 | A 결과로 좁혀진 점 |
| --- | --- | --- |
| `get_plan_json_output`의 응답 형태(본문 또는 URL)와 크기, admin team token으로 동작 | 미수행 (사용자 협조 필요) | 이름은 유효함. 입력 인자(run ID 또는 plan ID)도 token이 있어야 확인 가능 |
| TFE token을 준 상태의 `list_tools`(HCP tool 포함), rate limit(stdio 연속 호출 11회) | 미수행 (사용자 협조 필요) | 제안 `--tools` 값으로 실행해 8개 모두 나오는지, HCP tool 5개의 입력 스키마를 함께 기록 |
| MR head commit status의 `target_url` 형태, status가 head SHA에 붙는지 merge ref에 붙는지 | 미수행 (사용자 협조 필요) | — |
| MR pipeline ID token claim | 미수행 (사용자 협조 필요) | — |
| note 작성에 필요한 GitLab 토큰 최소 권한, quick action 무력화 필요 여부 | 미수행 (사용자 협조 필요) | — |
| 파일럿 repository의 private module 사용과 `.terraform.lock.hcl` 커밋 여부 | 미수행 (사용자 협조 필요) | — |
| job outbound 통신(gitlab.com, app.terraform.io, registry.terraform.io, Vertex)이 조직 정책에 막히지 않는지 | 미수행 (사용자 협조 필요) | 로컬에서 registry.terraform.io 조회가 되는 것만 확인함(조직 네트워크 아님) |
| 더미 Cloud Run Job으로 WIF → SA impersonation → 실행 override(args·env·task 수), 예약 env 덮어쓰기 가능 여부 | 미수행 (사용자 협조 필요) | — |

## 중단 조건 현황

| 중단 조건 | 현황 | 근거 |
| --- | --- | --- |
| plan JSON tool 불가 | 미확인 | `get_plan_json_output`은 v1.4.1의 유효한 이름이다. 실제 동작과 응답 형태는 B |
| run 식별 불가 | 미확인 | commit status `target_url`(B)에 달림 |
| 허용 tool 확대 필요 | 잠정 해당 없음 | 계획이 쓰는 이름은 모두 실재하고, 제안 allowlist 8개로 충분해 보인다. 단 `get_plan_json_output`이 plan ID를 요구하는데 `get_run_details`로 얻을 수 없으면 `get_plan_details` 추가가 필요해진다(B에서 확정) |
| override(env·task 수)를 무력화할 수 없음 | 미확인 | 더미 job(B). A에서 추가로 찾은 env 경로 두 개는 코드로 막을 수 있음: genai의 `GOOGLE_VERTEX_BASE_URL` 등(→ 명시 설정), MCP 자식의 `HOME`(→ 자식 env 최소화) |
| outbound 차단 | 미확인 | 더미 job(B) |

A 결과만으로는 해당하는 중단 조건이 없다. 별도로 **Go 1.26.4와 ADK 최신 버전의 충돌**(A2-1)은 중단 조건은 아니지만 Task 11 전에 결정이 필요하다.
