---
name: debug-remote
description: |
  Debugging a Go binary running under a remote DAP server (e.g. a dlv DAP server in a Kubernetes pod or other container) using mcp-dap-server.
  TRIGGER when: user asks to debug a process in a remote container/pod, connect to a remote dlv, debug over a port-forward, or passes an 'address' (host:port) for the debugger instead of running it locally.
  DO NOT TRIGGER when: the debugger runs on the same machine as the program (use debug-source, debug-binary, or debug-attach), or analyzing a crash dump (use debug-core-dump).
---

# Remote Debug Workflow (Go binary in a container)

Connect to a DAP server that is already running on another host instead of
spawning a local debugger, by passing `address` (`host:port`) to the `debug`
tool. The remote runs `dlv dap`; you reach it over a tunnel such as
`kubectl port-forward`. Once connected, the full tool surface (breakpoints,
stepping, variables, eval) works exactly as in a local session.

## Pre-flight checklist

Before starting, confirm:
1. **The debug image runs a DAP server.** Entrypoint (or a sidecar command):
   `dlv dap --listen=:40000 --only-same-user=false --api-version=2`
   - `--only-same-user=false` is required because the connection arrives over TCP.
   - Build the binary with debug info: `go build -gcflags=all="-N -l" -o /app/server .`
2. **The port is reachable from this machine.** Typically:
   `kubectl port-forward pod/<name> 40000:40000` → reachable at `127.0.0.1:40000`.
3. **Source path mapping.** The binary's *build* paths (from CI) usually differ
   from your local checkout — you will need `substitutePath` (step 2).
4. **What are you debugging?** A binary the server should launch (`binary` mode),
   or an already-running in-container process by PID (`attach` mode).

## Important notes

- No local debugger is spawned and **no local `dlv` is required** — the MCP
  server only needs TCP to the remote.
- `path` / `processId` are interpreted on the **remote** filesystem.
- `stop()` on a remote session **detaches by default** (it will not kill the
  remote workload). Use `stop(terminate=true)` to force termination.
  - ⚠️ **In `attach` mode, `stop(terminate=true)` kills the in-container
    process you attached to.** If that is PID 1 (the container's main process),
    the container/pod dies. For attach sessions use plain `stop()` (detach)
    unless you truly intend to kill the workload.
- **Every tool call is bounded — nothing hangs, even on a flaky tunnel.** If the
  port-forward drops or the remote goes unresponsive, calls return a
  `timed out … waiting for response` error (or, for `continue`/`step`, a
  pause-not-confirmed error) within seconds rather than blocking. Treat that
  error as "the connection is gone": call `stop()` to free the session, restore
  the tunnel (`kubectl port-forward …`), and reconnect with `debug(address=…)`.
  A bad `address` likewise fails the `debug` call within ~10s (dial timeout), not
  forever. You never need to kill the MCP server or the port-forward to recover
  from a stuck call — but doing so at the pod/tunnel level is always a safe
  last-resort backstop.
- `dlv dap` serves a single connection and exits when the session ends. For
  reconnect / detach-and-reattach, or to keep the app running independently of
  the debugger, run the remote as
  `dlv --headless --listen=:40000 --accept-multiclient ... exec /app/server`.

---

## How `continue`/`step` waiting works — read before you debug a live system

You are driving an **asynchronous protocol (DAP) through a synchronous tool
call**. Under the hood DAP does not "return when the program stops": the resume
request is acknowledged immediately and the *stopped* event arrives later, on
its own. The `continue`/`step` tools paper over that by running a **bounded wait
loop** for you: they wait up to `timeoutSeconds` for the stop, and if it does
not come they **pause the program** and return `Still running … paused at
<file:line>`. The call therefore always returns — it cannot hang.

**This makes the timeout your single most important lever, and choosing it is
your job — reason about it every time:**

- **The timeout is a budget for "how long am I willing to sit blind."** While the
  program runs you can see *nothing* — DAP only exposes the stack and variables
  once it is stopped, and `dlv` rejects inspection requests with
  `DebuggeeIsRunning` until then. A long timeout is dead time where you cannot
  observe, react, or be interrupted cheaply.
- **Prefer SHORTER timeouts.** A short wait that returns "still running, paused
  at X" is not a failure — it is a checkpoint. You get control back, you can see
  where execution currently is, and you decide: drive the trigger, adjust a
  breakpoint, or wait again. Short, repeated waits beat one long blind wait.
- **Match the timeout to what you are actually waiting for:**
  - Stepping or a breakpoint you expect to hit immediately → **1–2s**.
  - A breakpoint on a code path you are about to trigger yourself (you are about
    to `curl` the endpoint / send the message) → **2–5s**, then trigger, then
    `continue` again.
  - A breakpoint that fires only on **organic** traffic to a live service → still
    keep each wait modest (**5–15s**) and **loop**: issue the trigger or wait for
    traffic, `continue`, inspect, repeat. Do **not** reach for a 5-minute timeout
    "to be safe" — that just blinds you for 5 minutes.
- **Default is 10s** if you omit `timeoutSeconds`. Override it deliberately:
  ```json
  continue(timeoutSeconds=3)
  ```
- **The pause is harmless and reversible.** When a wait times out the program is
  left paused at its current point; `continue` again to resume. Nothing is lost.

Rule of thumb: if you find yourself wanting a big timeout, what you actually want
is a **shorter timeout in a loop** plus an action (trigger the request) between
iterations.

---

## Step-by-Step Workflow

### 1. Connect to the remote DAP server

Launch the remote binary under debug:
```json
debug(mode="binary", address="127.0.0.1:40000", path="/app/server")
```

Or attach to the already-running in-container process (often PID 1):
```json
debug(mode="attach", address="127.0.0.1:40000", processId=1)
```

If the connection fails:
- Confirm the port-forward is up: `curl -s 127.0.0.1:40000` should hang (open socket), not refuse.
- Confirm the remote started `dlv dap` with `--only-same-user=false`.
- Confirm the pod is running and the binary path exists in the container.

### 2. Map remote build paths to local paths (substitutePath)

A breakpoint set by your **local** file path only binds if that path matches the
**build** path compiled into the remote binary. In CI these usually differ, so
without a mapping breakpoints silently fail to verify.

First, discover the remote build path: after connecting, run `context()` and read
the `File:` line — that is the path the binary knows (e.g. `/build/main.go`). Map
your local root onto it. Then reconnect with the mapping (or pass it on the first
`debug` call):

```json
debug(mode="binary", address="127.0.0.1:40000", path="/app/server",
      substitutePath=[{"from": "/build", "to": "/Users/me/project"}])
```

Now a local breakpoint at `/Users/me/project/main.go:42` resolves to the remote
`/build/main.go:42`. Omit `substitutePath` entirely when the paths already match
(e.g. the remote debugger shares your filesystem).

### 3. Set breakpoints (local paths)

```json
breakpoint(file="/Users/me/project/main.go", line=42)
```

If a breakpoint reports "not verified", the path mapping is wrong — re-check the
`File:` path from `context()` and adjust `from`/`to`.

### 4. Run and inspect — identical to a local session

```json
continue()
context()
evaluate(expression="req.Header")
step(mode="over")
```

Use `info(kind="threads")` to inspect goroutines, exactly as for a local Go process.

`continue`/`step` wait at most `timeoutSeconds` (default 10) for a stop, then
pause and return `Still running … paused at <file:line>`. On a live service the
breakpoint may not hit immediately — that is expected. Drive the triggering
request (curl the endpoint, send the message), then `continue` so the breakpoint
hits while you wait, and keep each wait short. See **"How `continue`/`step`
waiting works"** above for choosing the timeout — shorter, in a loop, is almost
always better.

### 5. Disconnect safely

Detach (default for remote — leaves the workload running):
```json
stop()
```

Force-terminate the remote debuggee:
```json
stop(terminate=true)
```

---

## Decision Tree

```
Need to debug code running in a remote container
    │
    ├─ Remote runs `dlv dap --listen` + port-forwarded?
    │      → debug(mode="binary"/"attach", address=..., substitutePath=[...])
    │
    ├─ Breakpoint "not verified"?
    │      → substitutePath wrong: read File: from context(), remap from/to
    │
    ├─ `continue` returned "Still running … paused"?
    │      → normal: breakpoint not hit yet. Trigger the request, continue again
    │        (short timeout, in a loop). Do NOT escalate the timeout blindly.
    │
    ├─ A call returned "timed out … waiting for response"?
    │      → the tunnel/remote is unresponsive. stop(), restore port-forward,
    │        reconnect with debug(address=...)
    │
    └─ Need to keep app running across reconnects?
           → run remote as `dlv --headless --accept-multiclient` instead
```

## How to present findings

> **Diagnosis:** In pod `api-7f9c`, `ProcessOrder` at `/build/order.go:88` reads a
> nil `order.Customer` because the upstream cache returned a partial record.
> **Evidence:** `context()` shows `order.Customer == nil`; the breakpoint bound via
> `substitutePath` (`/build` → local checkout).
> **Fix:** validate the cache record before dereferencing, or repopulate on miss.
