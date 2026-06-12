# Design: Remote debugging — dial a remote DAP server (k8s containers)

Date: 2026-06-12
Status: **Accepted — implemented (v1: remote DAP direct-dial)**

## 1. Problem & Goal

Today the `debug` tool always **spawns a local DAP adapter** (`dlv dap` / `gdb -i dap`)
and either launches a program or attaches to a **local process by PID**.

We want to debug Go binaries running in **remote k8s containers**. The MCP host
reaches the container's debug port via `kubectl port-forward` (so from the MCP's
point of view it dials a `host:port`). The user **builds the debug images**, so we
can choose what Delve command runs remotely.

## 2. The two remote topologies (confirmed against `dlv dap --help`)

| | A. Remote `dlv dap --listen` | B. Remote `dlv --headless` + `attach mode:remote` |
|---|---|---|
| Wire protocol to MCP | **DAP** (dial it directly) | DAP, via a **local `dlv dap` bridge** |
| `dlv` needed on MCP host | **No** | Yes (the bridge) |
| Hops | 1 | 2 |
| Multiclient / reconnect | **No** (`dlv dap` rejects `--accept-multiclient`; exits when session ends) | **Yes** (`--accept-multiclient`) |
| App runs without a debugger attached | Only if started separately + we attach to its pid | Yes (entrypoint runs app under dlv) |
| Debug primitives exposed over DAP | Full | Full — **identical** |
| Architecture change in this repo | **Minimal** (skip Spawn, dial address) | Larger (new mode, bridge, attach-remote args) |

**Key conclusion:** there is **no difference in debugging functionality** between the
two — same Delve engine, same DAP surface. The difference is *session lifecycle*
(reconnect / survive-detach / independent workload), which only **B** provides.

`dlv dap --help` verbatim: *"This server does not accept multiple client connections
(--accept-multiclient). Use 'dlv [command] --headless' instead and a DAP client with
attach + remote config."* It supports `launch` (exec/debug/test/replay/core) and
`attach + local`, has no `--continue` (use `stopOnEntry`), and `--only-same-user`
defaults **true** (must be disabled for remote TCP).

## 3. Decision

**v1 = Topology A (dial a remote `dlv dap --listen`).** Rationale:
- Smallest architecture change — reuses every existing mode and the whole
  post-connect flow.
- MCP server stays self-contained: **no `dlv` dependency on the MCP host**.
- Covers the on-demand k8s debug-pod workflow fully (launch the binary, or attach
  to the in-container pid).

**Phase 2 (deferred) = Topology B.** Implement only if multiclient / reconnect /
"app keeps running while undebugged" is needed. It layers on as a separate
`connect` mode (local bridge + `attach mode:remote`) without reworking v1. Design
retained in §8.

## 4. v1 design — remote DAP transport

### 4.1 Core idea

Add an `address` parameter. When present, the `debug` tool **does not spawn a local
adapter**; it dials `address` with the existing `newDAPClient` and runs the normal
DAP handshake. `mode` still selects what to do once connected, interpreted on the
**remote** side:

- `mode:"binary"`, `path:"/app/bin"` → `launch` + `exec` the remote binary.
- `mode:"attach"`, `processId:1` → `attach` + `local` to the in-container pid.
- `mode:"source"`, `path:"/src/..."` → `launch` + `debug` (only if the image has the
  Go toolchain; usually not for slim debug images — document as discouraged).
- `mode:"core"` → unchanged remote semantics.

Everything downstream — `initialize`, breakpoint set, `configurationDone`,
stopped/terminated handling, session tool registration, `context` — is reused as-is.

### 4.2 API changes (`tools.go`)

`DebugParams`:

```go
Address        string `json:"address,omitempty" mcp:"remote DAP server address as host:port (e.g. dlv dap --listen). When set, connect to it instead of spawning a local debugger."`
SubstitutePath []struct {
    From string `json:"from"` // path as compiled on the remote/build host
    To   string `json:"to"`   // local path the MCP client uses
} `json:"substitutePath,omitempty" mcp:"source path mappings so breakpoints set by local path bind to remote build paths"`
```

- The existing `Port` field stays meaningful only for the *local-spawn* path
  (ignored when `address` is set). Documented.
- `SubstitutePath` is passed through in the Delve launch/attach args (Delve DAP
  supports `substitutePath`). Optional but strongly recommended for k8s where CI
  build paths ≠ local paths (see §7).

### 4.3 `debug()` flow change

Single new branch near the spawn step:

```go
if params.Address != "" {
    // Remote DAP: do not spawn; dial the remote adapter.
    client, err := newDAPClient(params.Address)   // existing TCP dialer
    if err != nil { return ... }
    ds.client = client
    ds.cmd = nil                                   // cleanup already tolerates nil
} else {
    // existing: ds.backend.Spawn(...) + transport switch
}
```

- Backend is still selected (for `AdapterID`, `LaunchArgs`, `AttachArgs`). For a
  remote DAP endpoint we assume it speaks the selected backend's dialect; default
  `delve`. (gdbserver/remote is out of scope.)
- `cleanup()` already handles `ds.cmd == nil` and just closes the client — we
  **never** kill the remote. Good.
- `SubstitutePath`, if set, is merged into the launch/attach args map built by the
  backend (add a parameter or post-merge in `debug()`).

### 4.4 Validation

- `address` is mutually informative with `mode`: still require the mode's usual
  field (`path` for binary, `processId` for attach). Reject `source` mode for
  remote unless explicitly allowed (toolchain requirement) — at minimum warn.
- Validate `address` with `net.SplitHostPort`.

### 4.5 Stop / detach safety

For remote sessions (`address != ""`), default `stop` to **detach**
(`disconnect terminateDebuggee=false`) so we don't kill a remote workload we merely
attached to. Note: with `dlv dap` the server exits on disconnect regardless, but
detach still avoids terminating the *debuggee* process when we attached to a
running pid. Documented in the `stop` description.

## 5. Docs & surfaces

- `debugToolDescription`: document `address` (+ that it skips local spawn) and the
  recommended remote command:
  `dlv dap --listen=:PORT --only-same-user=false --api-version=2`.
- README: tools/params table + a short "Remote (k8s) debugging" section with the
  `kubectl port-forward` recipe.
- `prompts.go`: a `debug-remote` prompt (required `address`, optional `path` or
  `processId`, `breakpoints`).
- `docs/debugging-workflows.md`: add the remote-DAP row + a mermaid for the k8s flow.

## 6. Testing plan

Unit:
- `address` parsing/validation; `SubstitutePath` merged into launch/attach args.
- `cleanup` with `cmd == nil` leaves no orphan and closes the client.

Integration (real `dlv` 1.26.3 is installed):
- Helper `startRemoteDlvDap(t, binaryPath)`: runs
  `dlv dap --listen=127.0.0.1:0 --only-same-user=false`, parses the listen address
  from stdout (same parsing the backend already does).
- Call `debug` with `address` + `mode:"binary"` + a breakpoint; assert `context`.
- Also cover `mode:"attach"` against a separately-started process + `processId`.
- Teardown: `stop` (detach) then kill the dlv dap process.

## 7. Gotcha: source-path resolution (applies to both topologies)

Breakpoints set by **local** file path must match the **remote build** paths baked
into the binary. In k8s the CI build paths usually differ from the MCP client's
local checkout, so breakpoints silently fail to bind. Fix = Delve `substitutePath`
(§4.2). Recommend shipping it in v1.

## 8. Phase 2 (deferred): headless + `attach mode:remote`

If multiclient/reconnect/independent-workload is needed:
- Remote runs `dlv --headless --listen=:PORT --accept-multiclient --api-version=2 exec /app/bin`.
- Add a `connect` mode + `ConnectArgs(host, port, stopOnEntry)` on `DebuggerBackend`
  → `{request:"attach", mode:"remote", host, port, stopOnEntry}`.
- Still spawn a **local** `dlv dap` bridge (needs dlv on the MCP host), then send
  the remote-attach request; reuse the existing initialized/breakpoint/config flow
  and the core-mode "read until StoppedEvent" reporting.
- Pairs naturally with the existing `stop detach=true`.

## 9. File-by-file (v1)

| File | Change |
|---|---|
| `tools.go` | `DebugParams.Address` + `SubstitutePath`; remote-dial branch in `debug()` (skip Spawn); merge substitutePath into launch/attach args; remote-aware `stop` detach default; `net` import |
| `prompts.go` | `debug-remote` prompt |
| `README.md`, `docs/debugging-workflows.md` | docs incl. k8s recipe |
| `tools_test.go` | `startRemoteDlvDap` helper + remote binary/attach integration tests |

## 10. Resolved decisions

1. v1 = remote DAP direct-dial (`address` → dial `dlv dap`). **Done.**
2. `substitutePath` ships in v1 (Delve-gated). **Done.**
3. Remote `stop` defaults to detach; `terminate=true` forces termination. **Done.**
4. No near-term need for Phase 2, so the param shape stays flat (`address`). Phase 2
   (headless/multiclient via a `connect` mode) remains deferred per §8.
