# Quiet Exec streams: protocol research and local reproduction

## Conclusion and scope

**A conforming Runner plugin must tolerate arbitrarily quiet stdout/stderr until completion, caller cancellation/deadline, environment lifetime or a genuine transport/runtime failure. There is no output-heartbeat requirement.** The pinned protobuf explicitly warns that commands can emit infrequent output and requires cancellation detection independent of failed output writes.

**Proven plugin-side compatibility defect:** alpha.9 starts `grpc.NewServer()` without a keepalive enforcement policy. Runner v13.2.0 sends HTTP/2 keepalive PINGs every **30 seconds** during active RPCs; the server's default minimum is **5 minutes**. Excess PINGs cause `GOAWAY ENHANCE_YOUR_CALM` with debug data **`too_many_pings`**, close the connection and cancel Exec. Sending response data resets the server's ping-strike counter, explaining why periodic output can mask this configuration mismatch. This is not an intentional Exec output-liveness policy and not a `chunkWriter` output timer.

The approved correction is `plugin.NewGRPCServer()`, used by the executable and protocol tests. It accepts the existing pinned Runner's transport PINGs; it adds no stdout/stderr, protobuf heartbeat, timer in Exec, environment setting or protocol field. The research-only phase did not publish a release; the operator subsequently authorized the narrowly scoped correction for the next immutable prerelease. Real direct-smoke integration validation remains the separate agents' responsibility.

## Final enforcement-policy review

```go
grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
    MinTime:             30 * time.Second,
    PermitWithoutStream: false,
}))
```

- **Exact option:** `grpc.KeepaliveEnforcementPolicy`, governing received client PINGs, not the server's outgoing `KeepaliveParams`.
- **MinTime remains exactly 30 seconds.** Runner v13.2.0's configured interval is 30 seconds. grpc-go checks `lastPingAt.Add(MinTime).After(now)`; equality is permitted, not a strike. The client's keepalive scheduling uses last inbound activity (including ACKs) when scheduling subsequent PINGs. No extra margin is required by these semantics or the deterministic reproduction.
- **PermitWithoutStream is explicitly false**, preserving its previous effective/default value. A quiet Exec with an open response stream still counts as an active RPC. The pinned Runner also sets client `PermitWithoutStream=false`, so it stops generating keepalive PINGs when there are no active RPCs; server permission for no-stream PINGs is unnecessary.
- No other enforcement or server keepalive option is needed. Server outgoing PING interval/ACK timeout, connection age/idle limits and strike enforcement remain grpc-go defaults. This is not a blanket keepalive-enforcement exemption: overly frequent active-stream PINGs can still accumulate strikes; the no-active-stream restrictions remain.
- Idle connections are acceptable without application output or no-stream PING permission. A virtual-time test retains the same connection over 10 idle minutes and successfully makes another RPC. After 35 further idle minutes, grpc-go's default **30-minute client channel idling** closes the unused transport and the next RPC reconnects normally. Client idling checks active call count, so it does not treat a quiet active Exec as an idle channel (see `dialoptions.go` and `internal/idle/idle.go` in grpc-go v1.84.0).

Only this repository, the already-pinned Runner source, dependency sources and public Kubernetes source were inspected. No Forgejo instance, application repository, pi-wright repository, cluster-IaC repository or cluster was accessed. Supplied integration observations are evidence from another agent, not independently retrieved results. Their signal-free 15-second output wrapper is diagnostic evidence **only**, not a recommended workaround. DinD and all security settings are unchanged. No source/evidence here attributes this failure to `kill(2)` or the separate AppArmor signaling issue.

The local reproduction proves a real defect and a mechanism consistent with the supplied observation. It does **not** prove the precise cause of every earlier real smoke failure without the failing run's transport error/logs.

## Inspected versions and source references

- Baseline plugin: `v0.1.0-alpha.9`, commit `5657d01e2e2b6821a7d7a80f573bcf2695cc011e`. In that commit, `cmd/forgejo-runner-kubernetes/main.go:73` creates the default gRPC server; `internal/plugin/transport.go:74–139` implements Exec.
- Runner: **v13.2.0**, commit **`df6b843fb929bb04b933b09c6bf208774d42480b`**, local read-only checkout `/tmp/fj-runner-v13` (HEAD verified).
- Protobuf SHA256 verified: **`961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`**.
- Runner declares grpc-go **v1.83.2**; this plugin links **v1.84.0**. Both versions' keepalive enforcement defaults and ping-strike logic were inspected and agree. Tests run with the plugin's linked version, the exact generated Runner v13.2.0 protobuf and Runner's exact default keepalive parameters; they do not launch the Runner daemon.
- Plugin Kubernetes dependencies: client-go/streaming **v0.37.1**; SPDY framing **github.com/moby/spdystream v0.5.1**. Kubernetes **v1.37.1** server source is a versioned source reference, **not an assertion about the integration cluster's server version/configuration**.

Commit-relative Runner references (all under [the pinned commit](https://code.forgejo.org/forgejo/runner/src/commit/df6b843fb929bb04b933b09c6bf208774d42480b)):

| Location | Evidence |
| --- | --- |
| `act/plugin/proto/v1alpha/plugin.proto:24–39,58–70,173–213` | RPC lifecycle/cancellation contract and Exec messages. |
| `act/plugin/proto/v1alpha/plugin_grpc.pb.go:151–165` | Generated client opens server stream, sends one request and half-closes request direction. |
| `act/plugin/client.go:23–34,91–124,156–180` | Initialization-only dial deadline; 30s/10s keepalive; no per-Exec call deadline option. |
| `act/plugin/adapter.go:202–215,310–393` | Environment lifetime calculation; Exec child context, Recv loop, completion/error processing. |
| `act/runner/run_context.go:1059–1103,1289–1304` | Plugin connection, log writer, execution environment and job timeout. |
| `act/runner/step_run.go:42` and `step.go:218–229` | Shell step invokes environment Exec with step context; optional step timeout. |
| `internal/app/run/runner.go:153–176,333–364` | Configured task deadline and maximum environment lifetime. |
| `internal/pkg/config/config.go:522–524` | Defaults: task timeout 3h, fetch timeout 30s, reporting interval 1s. |
| `internal/pkg/report/reporter.go:493–526` | Remote CANCELLED/FAILURE result cancels the job; no silence test. |
| `internal/app/cmd/daemon.go:93–101`, `internal/app/poll/poller.go:156–170` | Shutdown grace and forced job-context cancellation. |
| `act/runner/job_executor.go:24,146–158,189–194` | Result reporting 1m; cleanup/post-cancellation contexts 30m, separate from normal Exec. |
| `act/common/line_writer.go:17–43` | Line buffering/command handlers, no idle timer. |
| `act/plugin/testplugin/server.go:138–212,358–376`; `testplugin/cmd/main.go:27` | Process-based Exec/writer reference; its server also uses default gRPC enforcement. |

## Protocol and Runner semantics

`Exec(ExecRequest) returns (stream ExecOutput)` is **one unary request with server-streaming responses**, not client-streaming or bidirectional. The request contains `environment_id`, argv `command`, map `env`, optional `user`, and `workdir`. There is **no stdin field**.

Each response is one of:

- `DataChunk`: STDOUT or STDERR plus bytes;
- `ExecComplete`: process exit code, including nonzero codes;
- `ExecFailed`: failure to execute, distinct from a command's nonzero exit.

RPC/gRPC errors are another failure channel. The adapter blocks in `Recv()` without a timer, forwards data to the appropriate writer under a mutex, and finishes on a completion/failure message. Nonzero completion becomes `ExecError`. EOF **without** completion is an error (`stream ended before completion signal`); EOF is not how successful process completion is represented. A silent but open stream is valid.

There are **no Exec heartbeat, progress, keepalive, idle-timeout or timeout fields**. `CreateRequest.environment_timeout` is an environment lifetime field, not an output-idle deadline. HTTP/2 PING/PONG is below the protobuf layer.

The adapter creates `context.WithCancel(ctx)` and defers cancellation on return; it does not create an additional deadline. It inherits task/job/step deadlines. A cancelled parent, deadline, failed RPC/Recv, unexpected chunk, or normal completion ends the receive loop and cancels its child stream. The generated client `CloseSend()` half-closes the **request** direction after the single request; it does not cancel the response stream or require data to arrive.

The Runner task context has configured `runner.timeout` (default 3h); optional workflow job/step `timeout-minutes` add absolute deadlines. They are not reset by output. `NewClient` uses a 10s initialization timeout only when its caller has no deadline; the health/Capabilities calls may instead inherit an existing task deadline. That initialization context is **not reused as Exec's lifetime**. Startup calculates environment lifetime from remaining context/configured maximum, sent to Create; this plugin makes it Pod `activeDeadlineSeconds`, rounded up, also independent of output.

Remote task CANCELLED/FAILURE reporting cancels the task. The reporter updates state periodically even when there are no new log rows; report scheduling is not an Exec heartbeat requirement. Shutdown can also cancel jobs after the configured shutdown grace. Plugin connection closure, server/Pod termination, or network errors can end RPCs. Cleanup and result-reporting contexts are separate from normal command execution.

## Reference/test plugin

The pinned test plugin uses `exec.CommandContext(stream.Context(), ...)`, assigns mutex-serialized `execStreamWriter`s to stdout/stderr, and calls `cmd.Run()`.

- Immediate output is sent when the process writes.
- Silence leaves process waiting/pipe readers open; no writer call, polling or output timer is required.
- Output after silence is sent normally.
- Exit 0/nonzero becomes `ExecComplete(0/nonzero)` after `Run` returns; launch errors become `ExecFailed`.
- Context cancellation causes Go's default CommandContext cancellation to kill the direct process, independent of output. It does not guarantee descendant/process-group cleanup. Inherited output descriptors held by descendants can delay EOF/Wait; this is not an idle timeout.

The reference executable also uses plain `grpc.NewServer()`. Thus its **transport setup has the same keepalive mismatch** with the pinned Runner client. Its process implementation establishes that silence is semantically valid, not that its default transport setup handles long silence correctly. No detail requires this plugin to copy the reference's local process/signaling model.

## Current plugin path and goroutine/EOF behavior

```text
Runner step context -> plugin adapter WithCancel -> generated Exec request
  -> HTTP/2 gRPC -> Server.Exec(stream.Context()) -> get Pod, construct argv
  -> execPod POST pods/<id>/exec, container=job, stdin=false, TTY=false
  -> SPDY StreamWithContext -> stdout/stderr io.Copy goroutines
  -> chunkWriter.Write -> mutex-serialized gRPC DataChunk Send
  -> Runner Recv -> log/command line writer
SPDY status + output stream completion -> execPod return
  -> ExecComplete(exit code) or ExecFailed(execution/transport error)
```

`transport.go:99–109` `chunkWriter` is a synchronous `io.Writer`, not a timer/channel/pipe monitor. It skips zero-length writes (without closing anything), copies each nonempty buffer to avoid reuse, sends it, and returns byte count or send error. The shared mutex serializes stdout/stderr gRPC sends. No output means **no calls** to Write, not an EOF or process-progress judgment.

`Server.Exec` holds the environment lock, looks up the Pod, constructs argv/env/cwd, then synchronously waits for `execPod`. It forwards stream context directly into `remotecommand.StreamWithContext`. No additional Exec timeout is created. On context cancellation it returns the context error rather than emitting a successful completion. A Kubernetes exit error becomes the numeric `ExecComplete` code; another error becomes `ExecFailed`. Normal handler return closes the gRPC response stream. There is no plugin-owned stdout/stderr `io.Pipe`; the io.Pipes elsewhere in `transport.go` are **copy** paths, not Exec.

Client-go copies stdout and stderr in separate goroutines; idle `Read` blocks safely. Modern negotiated protocols wait for **both output EOFs** and the status/error stream. A descendant that holds stdout/stderr open can delay completion; closing an output stream early can alter protocol completion behavior. Neither is absence-of-output detection. A blocked gRPC Send or Runner log writer creates backpressure, not a silence timeout. Mutex acquisition is not context-aware, so a preceding operation that never releases its environment lock can stall a subsequent RPC; it does not explain output rescuing an already-running Exec and is not changed here.

On cancellation, StreamWithContext returns `ctx.Err()` and closes the SPDY connection, unblocking transport readers. That does **not** prove the remote PID is killed on every CRI runtime: explicit Runner Remove tears down the Pod. This pre-existing remote-process cancellation limitation is distinct from the proven premature gRPC connection closure; no signaling changes are made.

## Relevant timers and transport-close conditions

| Layer | Timer/deadline/close path | Output requirement? |
| --- | --- | --- |
| Runner task | `runner.timeout`, default 3h, plus inherited daemon/job context cancellation | No. Absolute task lifetime. |
| Runner job/step | Optional `timeout-minutes`; earliest inherited deadline wins | No. Absolute execution deadline. |
| Runner plugin initialization | 10s only if no caller deadline; health/Capabilities scope | No. Not an Exec timeout. |
| Runner fetch/report/cleanup | Fetch 30s; report every 1s; result reporting 1m; cleanup 30m; shutdown configured grace | Not output-liveness timers on Exec. |
| Environment | Create lifetime -> Pod active deadline | No. Pod can expire regardless of output. |
| Plugin Start/Remove | Startup 3m / deletion 30s | No. Not normal Exec deadlines. |
| gRPC client | PING after 30s without inbound activity; 10s response/activity allowance; no pings without active RPC | Transport liveness, not stdout. Server must accept valid PINGs. |
| gRPC server **before correction** | Minimum client PING interval 5m; more than 2 strikes closes connection with `too_many_pings` | **Response writes reset strikes**, so output can mask the incompatible policy. |
| gRPC server after correction | Minimum 30s, no idle-without-RPC permission; all other defaults unchanged | Allows pinned Runner's transport keepalive without application output. |
| Other gRPC defaults | Connection establishment 120s; server PING interval 2h/ACK allowance 20s; max connection idle/age/grace infinite by default | Handshake/transport health, not stdout-idle or process deadlines. |
| gRPC termination | RST_STREAM/caller cancellation, disconnect/GOAWAY, transport errors, server stop | Cancels stream context independently of output. |
| Kubernetes REST | InClusterConfig supplies no overall REST timeout (0); caller context bounds requests | No output timer. Other supplied REST configs can differ. |
| SPDY setup | Default dial timeout 30s if no dialer configured; CreateStream reply wait 30s | Connection/stream establishment only. |
| SPDY keepalive | Client-go supplies a 5s PING period | Frames, independent of stdout/stderr. |
| CRI URL-token setup | Reference request-cache token TTL 1m, consumed once before starting the session | Not a deadline on an already executing process. |
| Kubernetes CRI reference server | Default connection idle timeout 4h; stream-creation deadline 30s | Connection idle, not output idle; SPDY frames/PINGs reset activity. Actual deployment settings unknown. |
| API server | Normal request timeout bypassed for `exec`/other long-running subresources | Not a normal short-request timeout on a healthy exec stream. |
| Alternative WebSocket executor | PING 5s; pong/read allowance 61s; write deadline 60s | Transport deadlines, not required application progress. **Not selected by this plugin.** |
| Output/status readers | io.Copy/ReadAll wait for bytes, EOF or errors; no output timer | Silence is valid; incomplete/failed transport is not. |

### Exact gRPC mechanism

Sources: [grpc-go v1.84.0 defaults](https://github.com/grpc/grpc-go/blob/v1.84.0/internal/transport/defaults.go), [http2_server.go](https://github.com/grpc/grpc-go/blob/v1.84.0/internal/transport/http2_server.go) (`handlePing:880–933`, `resetPingStrikes`, header/data write callbacks), [keepalive API](https://github.com/grpc/grpc-go/blob/v1.84.0/keepalive/keepalive.go), [client keepalive](https://github.com/grpc/grpc-go/blob/v1.84.0/internal/transport/http2_client.go) (`keepalive`), [server connection setup](https://github.com/grpc/grpc-go/blob/v1.84.0/server.go). Equivalent defaults/handlePing were checked in v1.83.2.

`handlePing` ACKs client PINGs, checks the minimum interval against `lastPingAt`, and increments violations. With `maxPingStrikes=2`, the third strike emits `too_many_pings` and closes the connection. Response headers/data set `resetPingStrikes`; the next PING clears strikes. Healthy PING ACKs do **not** count as application output and do not remove the incompatible minimum. The client can automatically double its keepalive interval after rejection, but the active RPC has already failed; this is not a correctness solution.

Failure timing depends on prior response writes, transport BDP PINGs, scheduling and reconnections, **not a fixed 90-second Exec timeout**. The local control happened at 91 simulated seconds. Periodic output every 15 seconds masked the default-server failure at the same process duration. This matches the diagnostic shape of the supplied wrapper without establishing that every earlier integration error had this cause.

### Kubernetes/client-go source evidence

- [client-go v0.37.1 `tools/remotecommand/spdy.go`](https://github.com/kubernetes/client-go/blob/v0.37.1/tools/remotecommand/spdy.go): `newConnectionAndStream`, `StreamWithContext`; one streaming goroutine, buffered result/panic channels, select on result/panic/context; connection closed on return.
- [`v2.go`](https://github.com/kubernetes/client-go/blob/v0.37.1/tools/remotecommand/v2.go), [`v4.go`](https://github.com/kubernetes/client-go/blob/v0.37.1/tools/remotecommand/v4.go), [`v5.go`](https://github.com/kubernetes/client-go/blob/v0.37.1/tools/remotecommand/v5.go), [`errorstream.go`](https://github.com/kubernetes/client-go/blob/v0.37.1/tools/remotecommand/errorstream.go): copies/EOF/status; V4 JSON status decodes nonzero exits; V5 reuses V4 behavior. Older V1–V3 fallbacks also have no silence timer (older exit/status semantics differ).
- [client-go SPDY transport](https://github.com/kubernetes/client-go/blob/v0.37.1/transport/spdy/spdy.go): `RoundTripperFor` configures 5s PINGs; [streaming SPDY connection](https://github.com/kubernetes/streaming/blob/v0.37.1/pkg/httpstream/spdy/connection.go): periodic PING goroutine, creation-response deadline; [roundtripper](https://github.com/kubernetes/streaming/blob/v0.37.1/pkg/httpstream/spdy/roundtripper.go): default dial setup.
- [spdystream v0.5.1 `connection.go`](https://github.com/moby/spdystream/blob/v0.5.1/connection.go): `idleAwareFramer.ReadFrame/WriteFrame` reset idle activity for frames, not just stdout.
- [client-go WebSocket executor](https://github.com/kubernetes/client-go/blob/v0.37.1/tools/remotecommand/websocket.go): heartbeat/PONG deadlines; no WebSocket fallback is constructed in this plugin.
- Kubernetes v1.37.1 [CRI streaming server](https://github.com/kubernetes/kubernetes/blob/v1.37.1/staging/src/k8s.io/cri-streaming/pkg/streaming/server.go), [httpstream](https://github.com/kubernetes/kubernetes/blob/v1.37.1/staging/src/k8s.io/cri-streaming/pkg/streaming/remotecommand/httpstream.go), [ServeExec](https://github.com/kubernetes/kubernetes/blob/v1.37.1/staging/src/k8s.io/cri-streaming/pkg/streaming/remotecommand/exec.go): idle/creation timeouts; runtime executor called with timeout 0, completion status sent after execution returns. The [request cache](https://github.com/kubernetes/kubernetes/blob/v1.37.1/staging/src/k8s.io/cri-streaming/pkg/streaming/request_cache.go) has a 1m URL-token TTL, consumed before the active session starts.
- Kubernetes [API server long-running classification](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/controlplane/apiserver/config.go) includes `exec`; [timeout filter](https://github.com/kubernetes/kubernetes/blob/v1.37.1/staging/src/k8s.io/apiserver/pkg/server/filters/timeout.go) bypasses long-running requests.

**No stdout/stderr idle-output requirement was found in the selected client-go executor or versioned Kubernetes reference Exec semantics.** This does not assert that real API servers, CRI implementations, proxies, load balancers or networks have no connection idle limits. Transport PINGs must work, and actual runtime/proxy configuration remains a separate integration question.

## Local reproduction and validation

`internal/plugin/exec_liveness_test.go` uses `testing/synctest`, bufconn, real gRPC HTTP/2 streams and Runner's 30s/10s client settings. Only the Kubernetes executor is replaced with a controllable fake returning process-like results and observing context cancellation. Virtual time advances over real keepalive logic without wall-clock sleeps or a cluster. This tests the actual `Server.Exec`/chunkWriter path, not real Kubernetes SPDY or process killing.

The regression was run **before** changing production server construction:

| Case (180s simulated command unless stated) | Default server before fix | Corrected server |
| --- | --- | --- |
| Before marker, quiet, after marker, exit 0 | Unavailable / too_many_pings at ~91s | ExecComplete(0) at 180s |
| Periodic stdout every 15s, same duration | Success at 180s | Success at 180s |
| Entirely quiet, exit 23 | Unavailable / too_many_pings at ~91s | ExecComplete(23) at 180s |
| No initial output, delayed stdout+stderr, exit 0 | Unavailable / too_many_pings at ~91s | Both streams received, ExecComplete(0) at 180s |
| Deliberate caller cancellation at 45s while quiet | Canceled | Canceled; executor context observes cancellation, no completion |
| Explicit caller deadline at 45s while quiet | DeadlineExceeded | DeadlineExceeded; no completion |

The retained legacy-control tests use default `grpc.NewServer()` to reproduce rejection of **each quiet case** (success with markers, completely silent success, nonzero exit, delayed output) and demonstrate noisy masking alongside the corrected cases. An additional fully silent success case completes **10 simulated minutes** on the corrected server. `TestRunnerIdleConnection` also verifies idle connection reuse and normal client idle/reconnect behavior with no active-RPC PING permission. Cancellation alone does not remove the environment; explicit Remove remains successful. These are no-output **lifetime** tests, not tests requiring the process to demonstrate progress.

Run `go test ./internal/plugin -run TestExecOutputLiveness -v -count=1` for detailed results. Validation passed: `go test ./...`, `go test -race ./...`, `go vet ./...`, and `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/forgejo-runner-kubernetes-arm64 ./cmd/forgejo-runner-kubernetes`. The output was verified as a statically linked Linux AArch64 ELF. Repeated focused race tests also passed. A prerelease is now authorized after complete local and release-CI validation; no real Forgejo integration run is performed here.

## Approved correction and separate integration handoff

**Classification:** proven plugin gRPC keepalive compatibility defect, not intentional Runner/protobuf semantics; no writer-level silence timer found. The server policy correction is required for the supported pinned Runner and approved for release. No application heartbeat, client retry workaround, transport rewrite, command killing, AppArmor/seccomp/capability changes or DinD changes are proposed.

A separate integration agent can establish whether this caused the particular earlier failure by capturing:

1. Exact untruncated Runner error and Runner/plugin logs surrounding failure, especially `GOAWAY`, `ENHANCE_YOUR_CALM`, `too_many_pings`, gRPC status and disconnect/reset wording. Redact credentials/env values.
2. Timestamp of command start, last **raw streamed bytes** (not merely complete visible log lines), failure, and any prior RPC activity/reconnection.
3. Actual Runner version/build, plugin digest and keepalive/server policy deployed; configured runner/job/step timeouts, and whether a cancellation/restart occurred.
4. If signatures differ or failures persist: Pod/container state/events (active deadline, eviction, OOM, termination), plugin restart/termination timestamps, API-server/kubelet/CRI exec errors and relevant proxy/transport idle settings.
5. Deploy the new immutable plugin digest, retaining the existing alpha.9 DinD configuration. Run both existing smoke scripts **directly**, without the output wrapper, preserving exact-image ID checks before and after each smoke. Do not re-prove DinD architecture or change application scripts to emit heartbeats.

The deliberately small real integration shape, executed by the separate agents, is:

```sh
test "$(docker image inspect --format '{{.Id}}' "$VALIDATION_IMAGE")" = "$VALIDATION_IMAGE_ID"
node "$SMOKE_SCRIPT" "$VALIDATION_IMAGE" arm64
test "$(docker image inspect --format '{{.Id}}' "$VALIDATION_IMAGE")" = "$VALIDATION_IMAGE_ID"
```

Successful direct execution of both existing real smoke suites constitutes real integration validation. The application heartbeat wrapper is not part of the intended model and need not be retained after that succeeds. If `ENHANCE_YOUR_CALM` / `too_many_pings` persists, the correction is not effective in the deployed path: verify the actual digest/configuration. A different failure signature is separate evidence, not grounds to extend this fix speculatively. The local finding justifies this narrow release; the extra logs attribute specific supplied integration failures and confirm the deployed fix, not another plugin timeout. Stop after the immutable release; no integration environment is accessed here.
