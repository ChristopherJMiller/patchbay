# Phone Messages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Text pushed to the Cisco desk phones (current messages newest first, a chime on a new one), held during person-to-person calls, quiet hours and while nobody is home, from Home Assistant and the CLI; patchbay 0.4.0 plus the luma-homeops wiring.

**Architecture:** The controller decides (a pure `Decide` over Messages, house state and the agent's observations) and sends a `push` action; the agent renders nothing, it stores the page the controller sent, serves it on device HTTP, POSTs `/CGI/Execute` to the phone with Basic auth, and answers the phone's `authenticationURL` check. Home Assistant reaches a token-protected endpoint in the controller (ClusterIP only) that creates `Message` resources and records quiet/away. Phase A ships a bench build (0.4.0-rc.1) that only proves push on the real 7945; Phase C (idle-screen fallback) is built only if the bench says push is refused.

**Tech Stack:** Go 1.26, controller-runtime, kubebuilder markers + controller-gen, Helm chart, envtest, kind e2e (CI only), Asterisk 22 AMI, Home Assistant YAML packages (luma-homeops), SealedSecrets + git-crypt.

**Spec:** `docs/superpowers/specs/2026-10-26-phone-messages-design.md` (approved 2026-10-26). Read it first; this plan argues from it.

## Global Constraints

- Default expiry: **1 hour** after the message was sent.
- Quiet hours **22:00–08:00**, owned by Home Assistant (an HA `schedule` in luma-homeops); presence is `binary_sensor.house_occupied`.
- With no house state ever received, patchbay delivers (not quiet, someone home).
- A **call** that holds messages is a person-to-person call, ringing included; a **Stream call does not hold**.
- Urgent skips quiet and away, **never** a call in progress.
- The screen shows every current message, newest first, with the time each was sent; only a newly shown message chimes; when none is left the screen is cleared.
- Fridge/freezer messages are withdrawn when the door closes; leak messages are not (they expire).
- Leak is sent urgent, fridge/freezer not.
- Nothing new is reachable from the LAN: the messages endpoint is a ClusterIP Service; device HTTP gains only `/authenticate` and `/messages/<device>.xml`.
- No token, password or message text with a credential in any log, Event, error or status.
- patchbay rules: pure Compile/Render/Decide (no I/O, `now` passed in), golden and row-by-row table tests, only the controller decides and writes status, placeholders for secrets, the `exhaustive` linter, contracts changed deliberately and listed in `CLAUDE.md`.
- Local builds and tests on 4 threads only: every command below runs as `taskset -c 0-3 env GOMAXPROCS=4 GOFLAGS=-p=4 nix develop --cores 4 --max-jobs 1 -c <cmd>`, written **`nd <cmd>`**. Docker, kind e2e and `make asterisk-check` run in GitHub CI only (push to main, watch `ci-ok`).
- Commit per task, directly on `main` (the owner's preference); push at the end of each phase; never `--no-verify`.
- Every commit message ends with the session's two trailer lines (`Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: …`).

## Review Focus

1. **The agent restarts while messages are current.** Its page store is memory, so the phone's next fetch of `/messages/<id>.xml` would 404. Expected: the controller sees the agent's `Page` observation is empty and re-sends the same page without a chime. Test: `TestDecideResendsAfterAgentRestart` (Task 12).
2. **The phone's address changes** (DHCP: the Desk moved .101 → .100 once). Expected: the push goes to the live contact's address, else the last served fetch's source, never a stale one. Test: `TestTargetPrefersLiveContact` (Task 4).
3. **More messages than the screen holds.** CiscoIPPhoneText's `Text` is at most 4000 bytes. Expected: newest kept, oldest dropped, a final "+N older" line, never a truncated UTF-8 character. Test: `TestRenderMessagesOverflow` (Task 3).
4. **A message already expired when created** (sender's clock, or `--expires 0s`). Expected: Expired at once, never pushed, never chimes. Test: row `expired-on-arrival` in `TestDecide` (Task 12).
5. **Home Assistant sends garbage house state** (`"unavailable"`, a number, an empty body). Expected: 400, the stored state unchanged. Test: `TestHouseRejectsBadState` (Task 11).

---

## File structure

patchbay (new files bold):
- `api/v1alpha1/`: **`message_types.go`** (Message CRD); `common_types.go` (message states/reasons); `groupversion_info.go`.
- `internal/compile/`: `compile.go` (`Settings.PushSecret`, `PushAccount`, `Bundle.Push`), `configmap.go` (`Index.Push`), `projected.go` (`SecretRefs`), `layout.go` (`AuthenticatePath`, `MessagesPathPrefix`, `ChimeFile`), **`chime.go`** (the chime samples).
- `internal/drivers/`: `drivers.go` (`RenderContext.AuthenticationURL`, `MessageLine`, `MessageRenderer`); `cisco79x5/sep.go`, `render.go` (SEP fields), **`cisco79x5/messages.go`** (`RenderMessages`).
- `internal/agent/`: `api/api.go` (push wire types, `Page`, `StreamCalls`); **`push/`** (`Pusher`, `Pages`); `devicehttp/devicehttp.go` (`/authenticate`, `/messages/`); `observe/observe.go` (`Target`, stream classification, `Page`); `ami/ami.go`, `ami/client.go` (channel fields); `intake/resolve.go`, `apply/apply.go` (push credentials); `server.go` (push route).
- `internal/controller/`: `install/` (names, keys, generator, Events); `secrets/secrets.go`; `bundle/bundle.go`; `agentview/agentview.go`, `agentclient/agentclient.go` (`Push`); **`messages/`** (`decide.go`, `reconciler.go`, **`api/`** wire types, `server.go`); `setup.go`, `manager.go`.
- `internal/cli/`: **`push.go`** (`PhoneTestPush`), **`messages.go`** (`PhoneMessage`, `PhoneWithdraw`, `Messages`); `status.go`.
- `cmd/patchbay/`: `client.go`, `root.go`, `controller.go`, `agent.go`.
- `charts/patchbay/`: `values.yaml`, `templates/controller.yaml`, **`templates/messages.yaml`** (Service), `_helpers.tpl`, `NOTES.txt`, `ci/*-values.yaml`; `test/chart/`.
- `test/e2e/`: `fakephone/web.go`, `fakephone/config.go`, `fakephone/script/script.go`, `deskloop_test.go`, `values.yaml`.
- Docs: `CLAUDE.md`, `docs/operations.md`, `docs/e2e.md`, `README.md`.

luma-homeops: `cluster/telephony/messages-token.secret.yaml` (+ sealed), `cluster/home-assistant/ha-secrets.secret.yaml` (+ reseal), **`scripts/check-messages-token.sh`** + `.pre-commit-config.yaml`, **`cluster/home-assistant/packages/patchbay.yaml`**, `packages/home_alerts.yaml`, a dashboard, `cluster/applications/telephony-release.yaml`, `docs/telephony.md`.

---

## Phase A: the bench build (0.4.0-rc.1)

Proves, on the real Desk, that an authenticated push works, and answers the spec's bench questions. Everything here is also the final code; nothing is throwaway.

### Task 1: Push credentials, generated and delivered to the agent

**Files:**
- Modify: `internal/controller/install/install.go`, `install/secretgen.go`, `internal/controller/secrets/secrets.go`, `internal/controller/bundle/bundle.go` (`WatchesSecret`), `internal/compile/compile.go`, `configmap.go`, `projected.go`, `internal/compile/settings.go` (`checkSecretRef` use), `internal/agent/intake/resolve.go`, `internal/agent/apply/apply.go`, `charts/patchbay/templates/_helpers.tpl`, `templates/NOTES.txt`, `internal/controller/settings/testdata/chart-defaults.json`.
- Test: `install/install_test.go`, `install/secretgen_test.go`, `secrets/secrets_test.go`, `compile/projected_test.go`, `intake/resolve_test.go`, `apply/apply_test.go`, compile goldens (`bundle.json` gains `push`).

**Interfaces:**
- Produces: `install.Names.PushSecret` (= fullname+"-push"), `install.PushPasswordKey = "password"`, `func (n Names) PushRef() compile.SecretRef`, `func GeneratePushPassword(r io.Reader) (string, error)` (16 characters of `[A-Za-z0-9]`: the 7945 refused a 32-character SIP password, and the phone may echo it in a query string); `compile.PushUsername = "patchbay"`; `type PushAccount struct { Username string \`json:"username"\`; Password SecretRef \`json:"password"\` }`; `Settings.PushSecret SecretRef \`json:"pushSecret"\``; `Bundle.Push PushAccount \`json:"push"\``; `Index.Push PushAccount \`json:"push"\``; `intake.Resolved.PushPassword string`; `func (a *Applier) PushCredentials() (user, password string, ok bool)` (the live version's, ok false before the first apply).

- [ ] **Step 1: Failing tests.** `TestNames` expects `PushSecret == "patchbay-push"` and `PushRef() == SecretRef{Name: "patchbay-push", Key: "password"}`; `TestGeneratePushPassword` expects length 16 and only alphanumerics over 1000 draws; `TestEnsureCreatesPushSecret` expects `Ensure` to create `patchbay-push` once, keep an existing value, and return it under `PushRef()`; `TestSecretRefsIncludePush` expects `Bundle.SecretRefs()` to contain `b.Push.Password`; `TestResolvePushPassword` expects `Resolved.PushPassword` from the projected key; `TestCheckSettingsPushSecret` expects `CheckSettings` to refuse a `PushSecret` other than `n.PushRef()`; `TestApplierPushCredentials` expects `("patchbay", <value>, true)` after an apply and `ok == false` before.
- [ ] **Step 2:** `nd go test ./internal/controller/install/ ./internal/controller/secrets/ ./internal/compile/ ./internal/agent/intake/ ./internal/agent/apply/` — FAIL (undefined names).
- [ ] **Step 3: Implement**, copying the AMI account path exactly (`Settings.AMISecret` → `Bundle.AMI` → `SecretRefs` → `Resolve` → `apply.live.account`). `Compile` sets `Bundle.Push = PushAccount{Username: PushUsername, Password: s.PushSecret}`. The chart renders `pushSecret` next to `amiSecret` in `patchbay.settingsJSON`; update `chart-defaults.json`. Add the Secret to `WatchesSecret`, the NOTES.txt uninstall line and the Role comment in `controller.yaml`.
- [ ] **Step 4:** the Step 2 command, then `nd go test ./internal/compile -run Golden -update` and review the `bundle.json` diffs (only `push` and versions change), then `nd make test` — PASS.
- [ ] **Step 5: Commit** `push: generate the phones' push credentials and give them to the agent`.

### Task 2: Device HTTP answers the phone's authentication check

**Files:**
- Modify: `internal/compile/layout.go` (`AuthenticatePath = "/authenticate"`), `internal/agent/devicehttp/devicehttp.go`, `cmd/patchbay/agent.go`.
- Test: `internal/agent/devicehttp/authenticate_test.go`.

**Interfaces:**
- Consumes: `apply.Applier.PushCredentials` (Task 1).
- Produces: `devicehttp.Config.Credentials func() (user, password string, ok bool)`; the route `GET|POST /authenticate`.

Facts (usecallmanager.nz/phone-services.html): the phone forwards the user and password it received in the `/CGI/Execute` Basic auth to `authenticationURL`; the answer must be exactly `AUTHORIZED` with no newline. The method and parameter names are **unconfirmed** (Cisco's SDK: GET with `UserID`, `Password`, `devicename`).

- [ ] **Step 1: Failing tests.** `TestAuthenticateQuery`: `GET /authenticate?UserID=patchbay&Password=<pw>&devicename=SEPD0C78915E8C8` → 200, body exactly `AUTHORIZED`. `TestAuthenticateForm`: the same as a POST form body → `AUTHORIZED`. `TestAuthenticateBasic`: credentials only in an `Authorization: Basic` header → `AUTHORIZED`. `TestAuthenticateCaseInsensitiveNames`: `userid`/`password` → `AUTHORIZED`. `TestAuthenticateWrong`: a wrong password, a missing one, and `ok == false` from Credentials → 200, body exactly `UN-AUTHORIZED`. `TestAuthenticateNeverLogsValues`: with a recording logr sink, no log line holds the password or the user; one Info line holds the method, the sorted parameter **names** and the result. `TestAuthenticateNotAFetch`: no `sensor.FetchEvent` is recorded.
- [ ] **Step 2:** `nd go test ./internal/agent/devicehttp/ -run Authenticate` — FAIL.
- [ ] **Step 3: Implement** in `ServeHTTP` before the GET/HEAD check (this path alone also takes POST). Compare both user and password with `subtle.ConstantTimeCompare` on SHA-256 digests (as `match` does); bound the form body at 4 KiB. Wire `Credentials: applier.PushCredentials` in `runAgent`.
- [ ] **Step 4:** the Step 2 command, then `nd go test ./internal/agent/...` — PASS.
- [ ] **Step 5: Commit** `devicehttp: answer the phone's authenticationURL`.

### Task 3: The phone side: SEP fields, the chime, the message page

**Files:**
- Modify: `internal/drivers/drivers.go`, `internal/drivers/cisco79x5/sep.go`, `render.go`, `profile.go` (comment only), `internal/compile/compile.go` (`renderContext`), `internal/compile/layout.go`.
- Create: `internal/compile/chime.go`, `internal/drivers/cisco79x5/messages.go`.
- Test: `cisco79x5/render_test.go` (+ `-update` goldens), `cisco79x5/messages_test.go` (+ goldens under `testdata/messages/`), `compile/chime_test.go`, compile goldens.

**Interfaces:**
- Produces: `RenderContext.AuthenticationURL string` (http://<pbx>:8088/authenticate); `compile.MessagesPathPrefix = "/messages/"`; `compile.ChimeFile = TreeTFTP + "/patchbay-chime.raw"`, rendered into every bundle; `type MessageLine struct { Sent time.Time; Title, Text string; Urgent bool }`; `type MessageRenderer interface { RenderMessages(model string, lines []MessageLine, loc *time.Location) ([]byte, error) }` (an optional driver interface, like `SettingsChecker`); `cisco79x5.Driver` implements it.

- [ ] **Step 1: Failing tests.**
  - `TestRenderGolden` (cisco79x5) expects `<authenticationURL>http://192.168.0.240:8088/authenticate</authenticationURL>` right after `<directoryURL>` and `<phonePersonalization>1</phonePersonalization>`; `TestRenderErrors` gains "256-character authentication URL" (error) and a 255-character pass case.
  - `TestChime`: `len(chime()) == 2400` (0.3 s at 8 kHz, within the phone's 240–16080 samples), deterministic, first and last 80 samples quieter than the middle (10 ms fades).
  - `TestRenderMessages` golden cases: one message; three messages (newest first); an urgent one; a hostile one (`R&D <x> "y"`, `@@secret:a/b@@`); each is a `CiscoIPPhoneText` with `<Title>patchbay</Title>`, `<Prompt>` "1 message" / "3 messages", `<Text>` lines `15:04  Title: text` (urgent: `15:04  URGENT Title: text`; no title: `15:04  text`) joined by `\n`, times in `loc`, every string through `drivers.UserText`, and one `<SoftKeyItem><Name>Exit</Name><URL>SoftKey:Exit</URL><Position>1</Position></SoftKeyItem>`.
  - `TestRenderMessagesOverflow`: 60 lines of 100 bytes → `Text` at most 4000 bytes, valid UTF-8, newest kept, last line `+N older`.
- [ ] **Step 2:** `nd go test ./internal/drivers/cisco79x5/ ./internal/compile/` — FAIL.
- [ ] **Step 3: Implement.** Add `AuthenticationURL` to `sepDevice` after `DirectoryURL`; set `PhonePersonalization: 1` with the comment "pushed XML on /CGI/Execute is authenticated by the agent (authenticationURL)"; `checkRender` checks the new URL against `maxURL`. `chime()` synthesises 880 Hz for 120 ms then 1320 Hz for 180 ms at half scale, 10 ms linear fades, G.711 µ-law encoded (write the µ-law encoder; no new dependency). Update `TestRenderGolden`'s doc comment (authenticationURL is no longer release C; phonePersonalization 1). Leave `FeaturePushNotification` Untested until the bench (Task 7).
- [ ] **Step 4:** `nd go test ./internal/drivers/cisco79x5 -run Golden -update`, `nd go test ./internal/compile -run Golden -update`; review the diffs: every Cisco SEP file changes in exactly the two fields, every bundle gains `tftp/patchbay-chime.raw`. Then `nd make test` — PASS.
- [ ] **Step 5: Commit** `cisco79x5: authenticate pushes, render the message page, add the chime`.

### Task 4: The agent's push action

**Files:**
- Create: `internal/agent/push/push.go`, `push/pages.go`, `push/push_test.go`, `push/pages_test.go`.
- Modify: `internal/agent/api/api.go`, `internal/agent/server.go`, `internal/agent/observe/observe.go`, `internal/agent/devicehttp/devicehttp.go`, `cmd/patchbay/agent.go`; fakes in `internal/agent/seams_test.go`, `server_test.go`.
- Test: `api/api_test.go` (round trip), `server_test.go` (`TestPushRoute`), `observe/observe_test.go` (`TestTargetPrefersLiveContact`), `devicehttp/messages_test.go`.

**Interfaces:**
- Consumes: `PushCredentials` (Task 1), `compile.MessagesPathPrefix`, `compile.ChimeFile` (Task 3).
- Produces (wire contract, `internal/agent/api`):
  - `PathActionPush = "/v1/actions/push"`, `MaxPushBody = 16 << 10`.
  - `type PushRequest struct { Device string; PageVersion string; Page string; Beep bool; Clear bool; Execute []string }` (json lower camel; `Execute` is the bench's raw URIs: at most 3, each starting `Init:`, `Key:`, `SoftKey:` or `Play:`; `Page` at most 8 KiB).
  - `type PushResponse struct { Sent bool; Error string; Phone string }` (`Phone`: the phone's answer, bounded to 200 bytes: `CiscoIPPhoneResponse` status or `CiscoIPPhoneError Number=N`).
  - `DeviceObservations.Page string \`json:"page,omitempty"\`` (the page version the agent holds for the device, "" for none).
- Produces (agent): `type Pages struct` with `Set(device, version string, page []byte)`, `Get(device string) (version string, page []byte, ok bool)`, `Clear(device string)`; `type Config struct { Credentials func() (user, password string, ok bool); Target func(device string) (netip.Addr, bool); DeviceHTTPURL string; Pages *Pages; Client *http.Client; Log logr.Logger }`; `func New(cfg Config) (*Pusher, error)`; `func (p *Pusher) Push(ctx context.Context, req api.PushRequest) api.PushResponse`; `func (s *Store) Target(device string) (netip.Addr, bool)` (the live contact's address, else the last attributed served fetch's source, the `ScrapeTargets` rule, refused by `scrapeable`); `agent.Options.Push interface{ Push(context.Context, api.PushRequest) api.PushResponse }` (nil answers 501).

- [ ] **Step 1: Failing tests.**
  - `TestPushPage` (httptest phone): a request with `Page`, `PageVersion "abc"`, `Beep` → the phone receives `POST /CGI/Execute`, form field `XML` = `<CiscoIPPhoneExecute><ExecuteItem Priority="0" URL="Play:patchbay-chime.raw"/><ExecuteItem Priority="0" URL="http://192.168.0.240:8088/messages/phone-desk.xml?v=abc"/></CiscoIPPhoneExecute>`, Basic auth `patchbay:<pw>`; `Pages.Get("phone-desk")` returns the page; the response is `Sent: true, Phone: "status 0"`.
  - `TestPushClear`: `Clear` → `Pages.Clear`, one item `Init:Services` (the bench confirms or replaces this URI: keep it one constant `ClearURI`).
  - `TestPushExecuteRaw`: `Execute: ["Key:Services"]` → exactly that item; `["Dial:911"]` and 4 items → `Sent: false`, nothing sent.
  - `TestPushPhoneError`: the phone answers `<CiscoIPPhoneError Number="4"/>` → `Sent: false, Phone: "error 4"`, the page still stored.
  - `TestPushRefusals`: no credentials, no target, an unscrapeable address (loopback) → `Sent: false` with a reason; no request leaves.
  - `TestPushNeverLogsCredentials`.
  - `TestTargetPrefersLiveContact`: a live contact at .100 and a served fetch from .101 → .100; no contact → .101; neither → false.
  - `TestServesMessagePage`: `GET /messages/phone-desk.xml?v=x` → the stored page, `text/xml; charset=utf-8`, `Cache-Control: no-store`; unknown device or none stored → the uniform 404.
  - `TestObservationsCarryPage`: `Document` sets `Page` to the stored version.
  - `TestPushRoute`: bearer auth, `DisallowUnknownFields`, 400 without a device, 501 with `Options.Push == nil`.
- [ ] **Step 2:** `nd go test ./internal/agent/...` — FAIL.
- [ ] **Step 3: Implement.** The client copies `scrape.defaultClient` (no proxy, no redirects, 5 s timeouts, no keep-alives); read at most 4 KiB of the answer; `Status="0"` in a `CiscoIPPhoneResponse` is success. Store the page before the POST, so the phone's immediate fetch finds it. Wire everything in `runAgent` (`Target: store.Target`, `Pages` shared with devicehttp and observe, `DeviceHTTPURL` from `--device-http-listen`).
- [ ] **Step 4:** `nd go test ./internal/agent/... ./cmd/...` — PASS.
- [ ] **Step 5: Commit** `agent: push a page to a phone over /CGI/Execute`.

### Task 5: `patchbay phone test-push`

A diagnostic that goes from the CLI to the agent directly (as `firmware load` does): the bench needs it before Messages exist, and it stays as the "is push working?" check. Record it in `CLAUDE.md`'s deviations as the second CLI→agent call.

**Files:**
- Create: `internal/cli/push.go`, `internal/cli/push_test.go`.
- Modify: `cmd/patchbay/client.go` (`newPhoneCmd`), `cmd/patchbay/main_test.go`, `CLAUDE.md`.

**Interfaces:**
- Consumes: `Env.Agent.OpenAgent`, `api.PushRequest`/`PushResponse` (Task 4), the driver's `MessageRenderer` (Task 3), `Env.Clock`.
- Produces: `type TestPush struct { Text string; Beep, Clear bool; Execute []string }`; `func PhoneTestPush(ctx context.Context, env *Env, name string, opts TestPush) error`. Usage: `patchbay phone test-push <phone> [--text "…"] [--beep] [--clear] [--execute URI]…` (default text "patchbay test push").

- [ ] **Step 1: Failing tests.** `TestPhoneTestPush`: against the fake agent, `--text hi --beep` sends a `PushRequest` whose `Page` is `RenderMessages` of one line sent at the fake clock's time, `PageVersion` = the first 16 hex characters of its SHA-256, `Beep: true`; prints "pushed to desk: status 0". `TestPhoneTestPushRefused`: `Sent: false` → error naming the reason and the phone's answer. `TestPhoneTestPushNotAPhone`: an unknown phone or a non-Cisco model → error before any request. The token is never printed (the existing `newTestEnv` cleanup check).
- [ ] **Step 2:** `nd go test ./internal/cli/ ./cmd/patchbay/` — FAIL.
- [ ] **Step 3: Implement**, modelled on `firmware load`'s agent connection.
- [ ] **Step 4:** the Step 2 command — PASS.
- [ ] **Step 5: Commit** `cli: phone test-push`.

### Task 6: The fake phone takes pushes; e2e proves the chain

**Files:**
- Modify: `test/e2e/fakephone/config.go` (`sepXML`, `parseSEP`: `authenticationURL`), `fakephone/web.go`, `fakephone/run.go` (`snapshot`), `fakephone/script/script.go` (`Config.AuthenticationURL`, `State.Pushes`, `NewState`, package doc), `test/e2e/deskloop_test.go`, `docs/e2e.md`.
- Test: `fakephone/web_test.go` (`TestExecuteChecksAuthentication`), `fakephone/config_test.go`.

**Interfaces:**
- Produces: `script.PushRecord{ At time.Time; Authorized bool; Items []string; Fetched string; Status int }`; `State.Pushes []PushRecord`.

The fake phone behaves as the real one is documented to: on `POST /CGI/Execute` it takes the Basic credentials, `GET`s its SEP's `authenticationURL` with `UserID`, `Password` and `devicename`, refuses (`<CiscoIPPhoneError Number="4"/>`, HTTP 401) unless the body is exactly `AUTHORIZED`, then answers `<CiscoIPPhoneResponse><ResponseItem Status="0" Data="" URL=""/></CiscoIPPhoneResponse>`, records the items, and fetches the first `http` item (recording its body's `<Text>` in `Fetched`).

- [ ] **Step 1: Failing tests.** `TestExecuteChecksAuthentication` (unit, with an httptest authenticator): right credentials → Status 0 and a record with `Authorized: true` and both items; wrong → Number 4, `Authorized: false`; no SEP read yet → 503. `TestParseSEP` expects `AuthenticationURL` from the house golden.
- [ ] **Step 2:** `nd go test ./test/e2e/fakephone/...` — FAIL.
- [ ] **Step 3: Implement.** Add a `TestDeskLoop` Assess **"push: test-push reaches desk's screen"** after "provision": run `patchbay phone test-push desk --text e2e --beep`; `h.waitPhone` until a push record is `Authorized` with items `Play:patchbay-chime.raw` and the page URL, and `Fetched` contains `e2e`; then `--clear` gives a record with the item `Init:Services`. Add the bold sub-item to `docs/e2e.md`'s TestDeskLoop row.
- [ ] **Step 4:** `nd go test ./test/e2e/fakephone/...` and `nd make vet` — PASS (the e2e itself runs in CI).
- [ ] **Step 5: Commit** `fakephone: /CGI/Execute checks authenticationURL; e2e test-push`.

### Task 7: Release 0.4.0-rc.1, deploy, bench (with the owner)

- [ ] **Step 1:** `nd make lint`, `nd make test`, `nd make check-generated`, `nd make check-hygiene`; push to main; watch CI until `ci-ok` (includes e2e and asterisk-check).
- [ ] **Step 2:** tag `v0.4.0-rc.1` (`git tag -a v0.4.0-rc.1 -m "patchbay 0.4.0-rc.1"`), push; watch the release run; verify the chart's two image digests are attested by `ci.yml@refs/tags/v0.4.0-rc.1` and anonymously pullable (`docs/releasing.md`).
- [ ] **Step 3:** luma-homeops: `cluster/applications/telephony-release.yaml` `targetRevision: 0.4.0-rc.1`; check no active calls first (`asterisk -rx 'core show channels count'`); commit inside `nix develop` (pre-commit), push; watch Argo Synced/Healthy, PBX 3/3, Desk back to Registered with ConfigCurrent True (one resync).
- [ ] **Step 4: Bench, with the owner at the Desk.** Run and record each answer:
  - B-P1 `patchbay phone test-push desk --text "bench 1" --beep`: is it accepted (CLI says status 0)? The agent log line from Task 2 shows the method and parameter names the phone used for `authenticationURL`.
  - B-P2 did the chime play? Repeat during a Stream call (dial 750): what happens to the stream's audio?
  - B-P3 with the screen dark (power-save, 1 min idle): does the push wake it?
  - B-P4 a second push: does it replace the shown text? `--clear` (Init:Services): back to the idle screen? If not, try `--execute Key:Services`, `--execute SoftKey:Exit`, `--execute Init:Messages` until one clears; set `ClearURI` to it.
  - B-P5 press Exit: does the text close?
- [ ] **Step 5:** write the answers into `docs/operations.md` as a dated "Bench" note, and set the driver profile's `FeaturePushNotification` to Supported if B-P1 is yes, else Unsupported (with the date in its comment). **If B-P1 is no, do Phase C before Phase B's Task 12.** Commit `cisco79x5: push bench results`.

---

## Phase B: messages

### Task 8: The agent tells Stream calls from person calls

**Files:**
- Modify: `internal/agent/ami/ami.go` (`Channel`), `ami/client.go` (`Channels`), `internal/agent/observe/observe.go`, `internal/agent/api/api.go`.
- Test: `ami/conn_test.go`, `observe/observe_test.go`, `internal/agent/ami/asterisk_client_test.go` (tag `asterisk`, CI), `api/api_test.go`.

**Interfaces:**
- Produces: `ami.Channel{ Name, Endpoint, Application, ApplicationData, BridgeID string }`; `DeviceObservations.StreamCalls int \`json:"streamCalls,omitempty"\`` (of `Calls`, how many are Stream calls; person calls are `Calls - StreamCalls`). The agent learns no stream names or numbers.

- [ ] **Step 1: Failing tests.** `TestChannelsReadsBridgeFields` (fake AMI `CoreShowChannel` with `Application`, `ApplicationData`, `BridgeId`). `TestStreamCalls`: a desk channel with `ApplicationData` `AudioSocket/patchbay-streams…` → `Calls 1, StreamCalls 1`; a desk channel sharing `BridgeId` with an `AudioSocket/…` channel (a transferred stream) → `StreamCalls 1`; a desk channel dialing `PJSIP/softphone-chris-laptop,30` → `Calls 1, StreamCalls 0`; ringing (no bridge, `Application` empty) → `StreamCalls 0`. `TestRealAsteriskStreamChannel` (tag `asterisk`): a call to a stream number on the real Asterisk image shows the caller channel classified as a stream.
- [ ] **Step 2:** `nd go test ./internal/agent/...` — FAIL.
- [ ] **Step 3: Implement** the classification in `Store.Poll`; the cisco79x5 resync hold keeps using `Calls` (a resync still waits for any channel).
- [ ] **Step 4:** `nd go test ./internal/agent/...` — PASS.
- [ ] **Step 5: Commit** `agent: count Stream calls apart`.

### Task 9: The Message resource

**Files:**
- Create: `api/v1alpha1/message_types.go`, `config/samples/patchbay_v1alpha1_message.yaml`.
- Modify: `api/v1alpha1/common_types.go`, `groupversion_info.go`, `types_test.go`, `config/samples/kustomization.yaml`, `internal/controller/crd_smoke_test.go` (sample count 5), generated CRD + deepcopy (`nd make generate`).

**Interfaces:**
- Produces: `Message` (namespaced, `categories=patchbay`), one phone per Message (the endpoint and the CLI fan a target list out):
  - `MessageSpec{ Phone LocalName (required); Title string (0–32, no control characters); Text string (1–256, no control characters); Urgent bool; ExpiresAt metav1.Time (required); Key string (optional, DNS label, ≤ 63); Withdrawn bool }`. CEL: every field but `withdrawn` is immutable; `withdrawn` may only go false → true.
  - `MessageStatus{ State string; Reason string; Message string (≤ 2048); ShownAt *metav1.Time; FinishedAt *metav1.Time; ObservedGeneration int64 }`.
  - States: `MessageStateHeld "Held"`, `MessageStateShown "Shown"`, `MessageStateExpired "Expired"`, `MessageStateWithdrawn "Withdrawn"`, `MessageStateReplaced "Replaced"`, `MessageStateFailed "Failed"` (typed `MessageState`, an enum).
  - Reasons: `ReasonInCall "InCall"`, `ReasonQuietHours "QuietHours"`, `ReasonAway "Away"`, `ReasonPhoneMissing "PhoneMissing"`, `ReasonPushUnsupported "PushUnsupported"`, `ReasonPushFailing "PushFailing"`, `ReasonPhoneUnreachable "PhoneUnreachable"`.
  - Print columns: Phone, State, Reason, Key, Urgent, Expires, Age.

- [ ] **Step 1: Failing tests.** `TestAddToSchemeRegistersEveryKind` lists Message; `TestSchemaRejects` cases: text 257 characters, a control character in text, `withdrawn` true → false, a changed `text`, a key with `/`; `TestSamplesAreValid` with 5 samples.
- [ ] **Step 2:** `nd go test ./api/... ./internal/controller/ -run 'Scheme|Schema|Samples'` — FAIL.
- [ ] **Step 3: Implement**, `nd make generate`.
- [ ] **Step 4:** `nd make check-generated` and the Step 2 command — PASS.
- [ ] **Step 5: Commit** `api: Message`.

### Task 10: Wire contract and house state

**Files:**
- Create: `internal/controller/messages/api/api.go`, `api/api_test.go`.
- Modify: `internal/controller/install/install.go` (`HouseConfigMap` = fullname+"-house", key `HouseKey = "house.json"`; `MessagesService` = fullname+"-messages"), `install/events.go`.

**Interfaces:**
- Produces (`messagesapi`, a contract package): `DefaultPort = 9441`; `PathMessages = "/v1/messages"` (POST), `PathWithdraw = "/v1/withdraw"` (POST), `PathHouse = "/v1/house"` (GET, PUT); `MessageRequest{ To []string; Title, Text string; Urgent bool; Expires string /* Go duration, default "1h", 0s–24h */; Key string }` → 201 `MessageResponse{ Names []string }`; `WithdrawRequest{ Key string; To []string /* empty: every phone */ }` → 200 `WithdrawResponse{ Withdrawn int }`; `HouseUpdate{ Quiet *bool; Away *bool }` (at least one) → 200 `House`; `House{ Quiet, Away bool; UpdatedAt time.Time }` (also the ConfigMap's JSON); `Error{ Error string }`. Event reasons `MessageShown`, `MessageHeld`, `MessageExpired`, `MessageWithdrawn`, `MessageReplaced`, `MessageFailed`, `PushFailed`.

- [ ] **Step 1: Failing tests.** `TestRoundTrip` for every type; `TestValidate`: `MessageRequest` without `to` or `text`, `expires` "-1h" or "25h" or "x", a key with `/` → error; `HouseUpdate{}` → error.
- [ ] **Step 2:** `nd go test ./internal/controller/messages/... ./internal/controller/install/` — FAIL.
- [ ] **Step 3: Implement** `func (r MessageRequest) Validate() error`, `func (u HouseUpdate) Validate() error`, `func ParseHouse(raw []byte) (House, error)` (strict).
- [ ] **Step 4:** the Step 2 command — PASS.
- [ ] **Step 5: Commit** `messages: the endpoint's wire contract and house state`.

### Task 11: The messages endpoint

**Files:**
- Create: `internal/controller/messages/server.go`, `server_test.go`.
- Modify: `internal/controller/manager.go` (`Options.MessagesAddr`, `Options.MessagesToken`), `internal/controller/setup.go`, `cmd/patchbay/controller.go` (`--messages-bind-address` default "" = off, `--messages-token-secret=<name>/<key>`).

**Interfaces:**
- Consumes: `messagesapi` (Task 10), `install.Names`.
- Produces: `type ServerConfig struct { Client client.Client; Namespace string; Names install.Names; TokenSecret types.NamespacedName; TokenKey string; Addr string; Now func() time.Time; Log logr.Logger }`; `func NewServer(cfg ServerConfig) (*Server, error)`; `(*Server).Start(ctx) error`, `NeedLeaderElection() bool { return false }` (every replica serves).

- [ ] **Step 1: Failing tests** (fake client): `TestMessagesCreates`: a valid request `to: [desk, kitchen]` → 201, two Messages `generateName: msg-`, `ExpiresAt = now + expires`, labels `ManagedLabels()`; `TestMessagesAuth`: no or wrong bearer → 401, token read from the Secret on every request (a rotation works without a restart), a missing Secret → 503; `TestWithdrawSetsSpec`: two Messages with key `fridge-door`, one already Expired → `Withdrawn: 1`, only the live one gets `spec.withdrawn: true`; `TestHousePut`: `{"quiet": true}` → ConfigMap `patchbay-house` created with `quiet` true, `away` unchanged (false), `updatedAt` now; a later `{"away": true}` keeps `quiet`; `TestHouseRejectsBadState`: `{"quiet": "unavailable"}`, `{"away": 1}`, `{}`, an empty body → 400 and the ConfigMap unchanged; `TestServerLimits`: a body over 8 KiB → 413; unknown fields → 400; `TestServerNeverLogsToken`.
- [ ] **Step 2:** `nd go test ./internal/controller/messages/` — FAIL.
- [ ] **Step 3: Implement** with the agent control API's shape (`agent/server.go`: Go mux patterns, `MaxBytesReader`, `DisallowUnknownFields`, `subtle.ConstantTimeCompare`, `netlimit` 64/16, `ReadHeaderTimeout` 5 s, read/write 30 s, idle 15 s). Conflicts on the ConfigMap retry (`retry.RetryOnConflict`). Add it in `controller.Setup` with a compile-time `manager.Runnable` check; not started when `MessagesAddr` is empty.
- [ ] **Step 4:** the Step 2 command and `nd go test ./internal/controller/ ./cmd/patchbay/` — PASS.
- [ ] **Step 5: Commit** `messages: the endpoint Home Assistant posts to`.

### Task 12: Decide and the reconciler

**Files:**
- Create: `internal/controller/messages/decide.go`, `decide_test.go`, `reconciler.go`, `envtest_test.go`, `main_test.go`.
- Modify: `internal/controller/agentview/agentview.go` (`Actor.Push`), `agentclient/agentclient.go`, `internal/controller/setup.go`, fakes (`devices/helpers_test.go`), `internal/controller/metrics/metrics.go` (optional collector).

**Interfaces:**
- Consumes: Message (Task 9), `House` (Task 10), `DeviceObservations.Calls/StreamCalls/Page` (Tasks 4, 8), `MessageRenderer` (Task 3), `Profile.Supports(FeaturePushNotification)`.
- Produces:
  - `func (c *Client) Push(ctx context.Context, req agentapi.PushRequest) (agentapi.PushResponse, error)` and the same on `agentview.Actor`.
  - `type PhoneView struct { Name, DeviceID, Model string; Exists bool; Push drivers.Support; Reachable bool /* agent reachable and a live contact */; PersonCalls int; HeldPage string; Renderer drivers.MessageRenderer }`.
  - `type Input struct { Messages []v1alpha1.Message; House messagesapi.House; Phones map[string]PhoneView; Location *time.Location; Now time.Time }`.
  - `type PagePush struct { Phone, DeviceID, PageVersion string; Page []byte; Beep, Clear bool }`.
  - `type Verdict struct { Status map[string]v1alpha1.MessageStatus /* by Message name, only changed ones */; Pushes []PagePush; Delete []string; RequeueAfter time.Duration; Events []Event }`.
  - `func Decide(in Input) Verdict` (pure). `RetainFinished = 24 * time.Hour`; `PushRetry = 30 * time.Second`.

Rules, in order, per Message (first match wins; each a `TestDecide` row):
1. `finished-retained`: final state and `FinishedAt` older than `RetainFinished` → Delete.
2. `final`: already Expired/Withdrawn/Replaced/Failed → unchanged.
3. `expired` / `expired-on-arrival`: `Now >= ExpiresAt` → Expired (never pushed).
4. `withdrawn`: `spec.withdrawn` → Withdrawn.
5. `replaced`: a newer Message (creationTimestamp, then name) with the same non-empty key and phone, not withdrawn → Replaced.
6. `phone-missing`: no such Phone → Failed `PhoneMissing`.
7. `push-unsupported`: `Push == Unsupported` and no fallback (Phase C changes this row) → Failed `PushUnsupported`.
8. `held-in-call`: `PersonCalls > 0` → Held `InCall` (urgent too).
9. `held-quiet`: not urgent and `House.Quiet` → Held `QuietHours`.
10. `held-away`: not urgent and `House.Away` → Held `Away`.
11. `unreachable`: not `Reachable` → Held `PhoneUnreachable`.
12. otherwise → current: Shown once a push of a page holding it succeeded (`ShownAt` set by the shell from the push result).

Per phone, after the Messages: the current set newest first renders through `Renderer` (`loc` = settings timezone) to a page; `PageVersion` = first 16 hex of its SHA-256. If the phone is in a person call → no push of any kind. Else if the current set is empty and `HeldPage != ""` → `PagePush{Clear: true}`. Else if `PageVersion != HeldPage` → `PagePush{Page, Beep: some current Message has no ShownAt}`. `RequeueAfter` = the earliest future `ExpiresAt` or `FinishedAt + RetainFinished`, capped at `PushRetry` while anything is Held for `PhoneUnreachable` or a push failed.

- [ ] **Step 1: Failing tests.** `TestDecide` with every row above, plus: `shown-stays-current` (a Shown message is in the page until it expires); `urgent-in-quiet` (urgent shows during quiet, a non-urgent one stays Held); `stream-call-does-not-hold` (`PersonCalls 0` with `StreamCalls 1` → pushed); `newest-first`; `beep-only-new` (an expiry re-render → `Beep: false`); `clear-when-none-left`; `TestDecideResendsAfterAgentRestart` (`HeldPage ""` with a Shown message → a push of the same page, `Beep: false`); `TestRuleOrder`; `TestDecideTotal` (random inputs: never a push during a person call, never a Shown message past its expiry). Envtest `TestMessagesEnvtest`: a Message for `desk` with a fake Actor → status Shown and one Push; a `HeldPage` change re-sends; the house ConfigMap `quiet: true` holds a non-urgent one; a finished one older than 24 h is deleted.
- [ ] **Step 2:** `nd go test ./internal/controller/messages/ ./internal/controller/agentclient/` — FAIL.
- [ ] **Step 3: Implement.** The reconciler uses one request key per namespace (`messages`): `EnqueueRequestsFromMapFunc` maps every Message, Phone, the house ConfigMap and the poller's Phone changes onto it. It reads Phones and the profile from the registry, the snapshot from `agentview.Observer`, the house from the ConfigMap (missing = not quiet, not away), calls `Decide`, sends each `PagePush` (a failure: Warning Event `PushFailed`, Held `PushFailing` with the agent's reason, retry after `PushRetry`), sets `ShownAt` on the Messages a successful push showed, writes changed statuses, deletes per `Verdict.Delete`, and emits Events with distinct notes (`install.EventNote`).
- [ ] **Step 4:** the Step 2 command, `nd make test` — PASS.
- [ ] **Step 5: Commit** `messages: decide and deliver`.

### Task 13: CLI: message, withdraw, list, status

**Files:**
- Create: `internal/cli/messages.go`, `messages_test.go`.
- Modify: `internal/cli/status.go` (+ `testdata/status.golden`), `internal/cli/env.go` (doc: Kube also creates and updates Messages), `fakes_test.go` (`WithStatusSubresource(&v1alpha1.Message{})`), `cmd/patchbay/client.go`, `cmd/patchbay/root.go`, `main_test.go`.

**Interfaces:**
- Produces: `type MessageOpts struct { Title string; Urgent bool; Expires time.Duration; Key string }`; `func PhoneMessage(ctx, env *Env, phone, text string, o MessageOpts) error` (creates the Message, prints its name); `func PhoneWithdraw(ctx, env *Env, phone, key string) error`; `func Messages(ctx, env *Env) error` (a table: name, phone, state, reason, key, urgent, sent, expires; finished ones last). Commands: `patchbay phone message <phone> <text> [--title] [--urgent] [--expires 1h] [--key]`, `patchbay phone withdraw <phone> --key <key>`, `patchbay messages`. `patchbay status` gains a Messages section listing Held ones with their reason.

- [ ] **Step 1: Failing tests.** `TestPhoneMessage` (fake clock: `ExpiresAt = now + 1h` by default; `--expires 0s` allowed; text over 256 characters refused before the API call); `TestPhoneWithdraw`; `TestMessagesTable`; the status golden with a Held message.
- [ ] **Step 2:** `nd go test ./internal/cli/ ./cmd/patchbay/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** the Step 2 command — PASS.
- [ ] **Step 5: Commit** `cli: phone message, phone withdraw, messages`.

### Task 14: Chart, docs, e2e

**Files:**
- Create: `charts/patchbay/templates/messages.yaml` (Service `<fullname>-messages`, ClusterIP, port `messages` 9441 → controller pods).
- Modify: `charts/patchbay/values.yaml` (`messages: {enabled: false, port: 9441, tokenSecret: {name: "", key: token}}`, `required` name when enabled), `templates/controller.yaml` (container port `messages`, args, Role), `ci/all-features-values.yaml`, `test/chart/*` (`TestChartControllerRole`, `TestChartMessagesService`), `test/e2e/values.yaml`, `test/e2e/deskloop_test.go`, `docs/e2e.md`, `docs/operations.md`, `README.md`, `CLAUDE.md`.

Role additions (and `TestChartControllerRole` updated to them): `messages` get/list/watch/create/update/delete; `messages/status` update; ConfigMap `<fullname>-house` in the update `resourceNames` (create already allowed). `CLAUDE.md` deviations: the controller now creates Messages (from the endpoint) and deletes finished ones after 24 h, the only `delete` it has, on `messages` only; a Message names one phone; the messages endpoint; `test-push` is the second CLI→agent call; contracts list gains `messagesapi`, the push wire types and `Channel`/`StreamCalls`.

- [ ] **Step 1: Failing tests.** `TestChartControllerRole` with the new rules; `TestChartMessagesService` (enabled: Service, port, args `--messages-bind-address=:9441 --messages-token-secret=<name>/token`; disabled: none; enabled without a name: render fails). e2e `TestDeskLoop` Assess **"messages"**: a token Secret in the e2e namespace; `patchbay phone message desk "dinner"` → desk's fake phone gets an authorized push whose page holds `dinner`, the Message is Shown; during the existing **change-during-call** open call a second message is Held `InCall`, and is pushed (with `Beep`) after the BYE; a tool pod PUTs `{"quiet": true}` to the Service and a non-urgent message stays Held `QuietHours` while an `--urgent` one is pushed; `patchbay phone withdraw desk --key k` → a page without it, or a clear.
- [ ] **Step 2:** `nd go test ./test/chart/` — FAIL.
- [ ] **Step 3: Implement**; write `docs/operations.md` "Messages" (how to send, the states and reasons, the hold rules, bench results, troubleshooting a refused push).
- [ ] **Step 4:** `nd make lint`, `nd make test`, `nd make check-generated` — PASS; push; CI `ci-ok` green.
- [ ] **Step 5: Commit** `chart: the messages endpoint; docs; e2e`.

---

## Phase C (only if bench B-P1 is no): the idle-screen fallback

### Task 15: Deliver by idleURL

**Files:** `internal/drivers/cisco79x5/sep.go`, `render.go`, `profile.go`, `internal/drivers/drivers.go` (`RenderContext.MessagesURLPrefix`), `internal/agent/api/api.go` (`PushRequest.StoreOnly bool`), `internal/agent/push/push.go`, `internal/controller/messages/decide.go`, goldens, `docs/operations.md`.

- [ ] **Step 1: Failing tests.** `TestRenderGolden` for a phone whose profile has push Unsupported: `<idleURL>http://192.168.0.240:8088/messages/phone-desk.xml</idleURL>`, `<idleTimeout>60</idleTimeout>`, no `authenticationURL`, `phonePersonalization` 0. `TestPushStoreOnly`: `StoreOnly` stores the page, sends nothing. `TestDecide` row 7 becomes `push-unsupported-fallback`: messages are current as usual, pushes carry `StoreOnly: true` and never `Beep`; an empty current set stores an empty page (`RenderMessages` of no lines renders "No messages"), so the idle URL never 404s.
- [ ] **Step 2:** `nd go test ./internal/drivers/cisco79x5/ ./internal/agent/push/ ./internal/controller/messages/` — FAIL.
- [ ] **Step 3: Implement**: the SEP picks `idleURL` + `idleTimeout` or `authenticationURL` + personalization from the profile's push support; `-update` the goldens and review them.
- [ ] **Step 4:** `nd make test` — PASS.
- [ ] **Step 5: Commit** `cisco79x5: the idle-screen fallback for phones that refuse pushes`.

---

## Phase D: release 0.4.0

### Task 16: Release and deploy

- [ ] **Step 1:** CI green on main; tag `v0.4.0`; release run green; digests attested and pullable; write release notes (what's new, upgrading from 0.3.0: every Cisco phone resyncs once; set `messages.enabled` and `messages.tokenSecret`; the bench results).
- [ ] **Step 2:** the galaxy bump happens in Phase E Task 18 (it needs the token Secret first).

---

## Phase E: luma-homeops

Work in `/home/chris/Repos/luma-homeops` under its `CLAUDE.md` (S1–S9; commit inside `nix develop` so pre-commit runs; secrets via git-crypt + `./sign.sh`; never print secret values; `git diff --no-textconv` for anything that leaves the terminal). HA work follows the `ha-config` skill and `hactl`.

### Task 17: The shared token

**Files:**
- Create: `cluster/telephony/messages-token.secret.yaml` (Secret `patchbay-messages-token`, ns `telephony`, key `token`: 32 random base64url bytes), its sealed `messages-token.yaml` (`./sign.sh`), `scripts/check-messages-token.sh`.
- Modify: `cluster/telephony/kustomization.yaml`, `cluster/home-assistant/ha-secrets.secret.yaml` (key `patchbay_messages_auth: "Bearer <token>"`: HA's `!secret` replaces a whole value, so the secret holds the full header), its reseal (`rm ha-secrets.yaml; ./sign.sh`), `.pre-commit-config.yaml`.

- [ ] **Step 1:** generate the token into a shell variable (never echoed), write both `.secret.yaml` files from it, seal.
- [ ] **Step 2:** `scripts/check-messages-token.sh` compares the SHA-256 of the two values (decrypted locally, never printed) and fails with "the HA copy of the patchbay messages token differs; update both and reseal"; add it to pre-commit beside `mirror-family-list`.
- [ ] **Step 3:** `nd pre-commit run --all-files` passes; `git diff --no-textconv --stat` shows only the expected files. Commit `telephony: the messages token, shared with Home Assistant`.

### Task 18: patchbay 0.4.0 on galaxy

**Files:** `cluster/applications/telephony-release.yaml` (`targetRevision: 0.4.0`, `messages: {enabled: true, tokenSecret: {name: patchbay-messages-token}}`), `docs/telephony.md` (Messages section).

- [ ] **Step 1:** S1 checks (Argo, Ceph, no active calls); commit; push; watch Argo, PBX 3/3, the Desk Registered + ConfigCurrent, the `patchbay-messages` Service.
- [ ] **Step 2:** `patchbay phone message desk "galaxy 0.4.0"` → the Desk shows it; `kubectl -n telephony get messages` shows Shown.

### Task 19: Home Assistant: notify, house state, alerts, typed messages

**Files:**
- Create: `cluster/home-assistant/packages/patchbay.yaml`.
- Modify: `cluster/home-assistant/packages/home_alerts.yaml`, `cluster/home-assistant/kustomization.yaml` (`ha-packages` list), a dashboard under `cluster/home-assistant/dashboards/` (the card), `packages/config_revision.yaml` (`hactl revision --write`).

Content:
- `patchbay.yaml`:
  - `notify:` platform `rest`, name `desk_phone`, `resource: http://patchbay-messages.telephony.svc.cluster.local:9441/v1/messages`, `method: POST_JSON`, header `Authorization: !secret patchbay_messages_auth`, body `{"to": ["desk"], "title": title, "text": message, "urgent": data.urgent (default false), "expires": data.expires (default "1h"), "key": data.key (default "")}`.
  - **Note (final review): the body HA actually sends.** HA's `rest` notify platform puts the message under `message` (and the title under `title`) by default; set `message_param_name: text` and `title_param_name: title`, or build the whole body in `data`. Templates in `data` render to strings, so a templated `urgent` arrives as `"True"`/`"False"`. The endpoint's decoder is strict: an unknown field (`message`) and a string `urgent` are both a 400, and HA only logs it. So `urgent` must reach the endpoint as a JSON boolean: a literal `urgent: true` in a second notify target (say `desk_phone_urgent`, whose `data` holds no template for it), or a `rest_command` whose payload template writes the JSON itself (`"urgent": {{ urgent | default(false) | tojson }}`, `"text": {{ message | tojson }}`). `expires` and `key` are strings, so templates are fine for them.
  - `rest_command.patchbay_house` (PUT `/v1/house`, payload from its `quiet`/`away` variables) and `rest_command.patchbay_withdraw` (POST `/v1/withdraw`, `{"key": key, "to": ["desk"]}`), same header.
  - `schedule.quiet_hours`: every day 22:00–24:00 and 00:00–08:00.
  - Automation `patchbay_house_quiet`: on `schedule.quiet_hours` change → `rest_command.patchbay_house` `quiet` = state "on".
  - Automation `patchbay_house_presence`: on `binary_sensor.house_occupied` changing between `"on"` and `"off"` only (never from or to unavailable) → `away` = state "off".
  - Automation `patchbay_house_sync`: on HA start and every hour → both, skipping `away` while `house_occupied` is unavailable.
  - `input_text.desk_phone_message` (max 200) and `script.desk_phone_send` (sends it with `notify.desk_phone`, then clears the input). The script may call `notify.*`: `hactl lint`'s rule only checks automations.
- `home_alerts.yaml` (every `notify.*` call in an automation lives here):
  - `alert_leak_detected`: a second action `notify.desk_phone` with title "Leak detected", the same message, `data: {urgent: true, expires: "4h"}`. (expires 4 h: builder's call; a leak needs a look.) The `urgent: true` here reaches the endpoint as a boolean only by one of the routes in the note above.
  - `alert_fridge_freezer_ajar`: a second action `notify.desk_phone` with `data: {key: "{{ trigger.entity_id | replace('binary_sensor.', '') | replace('_', '-') }}"}`. A key is a DNS label: `binary_sensor.fridge_door_contact` must become `fridge-door-contact`, since `messagesapi` refuses `fridge_door_contact` (400).
  - New `alert_fridge_freezer_closed`: on either door `to: "off"` → `rest_command.patchbay_withdraw` with the same key, `"{{ trigger.entity_id | replace('binary_sensor.', '') | replace('_', '-') }}"` (the withdraw refuses an underscore too).
- The dashboard card: an entities card with `input_text.desk_phone_message` and a button running `script.desk_phone_send`.

- [ ] **Step 1:** write the files; quote `"on"`/`"off"` and times (`"22:00:00"`).
- [ ] **Step 2:** `nd hactl lint` and `nd hactl check` (offline config check) — clean; `hactl revision --write`.
- [ ] **Step 3:** commit (inside `nix develop`), push; Argo applies; `ha-reload` hook runs.
- [ ] **Step 4: Verify live** (with the owner): `hactl` calls `notify.desk_phone` with "HA test" → the Desk shows it; assert that the real call was answered **201**, not only that nothing failed in HA's UI (the rest notify platform only logs a non-2xx): `patchbay messages -n telephony` lists the new Message and HA's log has no `rest` notify error for it. Do it twice, once plain and once as the leak alert sends it (title, `urgent: true`, `expires: "4h"`, a key), since the wrong field name or a string `urgent` is a 400; `kubectl -n telephony get configmap patchbay-house -o jsonpath='{.data.house\.json}'` shows the current quiet/away; the typed-message card sends; trip the fridge contact for 5 minutes (or `hactl` set state in a test) and close it → shown, then withdrawn.
- [ ] **Step 5:** update `docs/telephony.md` (Messages: HA services, the schedule, how to add an event) and the memory notes; commit.
