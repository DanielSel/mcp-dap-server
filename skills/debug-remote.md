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
- `dlv dap` serves a single connection and exits when the session ends. For
  reconnect / detach-and-reattach, or to keep the app running independently of
  the debugger, run the remote as
  `dlv --headless --listen=:40000 --accept-multiclient ... exec /app/server`.

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
    └─ Need to keep app running across reconnects?
           → run remote as `dlv --headless --accept-multiclient` instead
```

## How to present findings

> **Diagnosis:** In pod `api-7f9c`, `ProcessOrder` at `/build/order.go:88` reads a
> nil `order.Customer` because the upstream cache returned a partial record.
> **Evidence:** `context()` shows `order.Customer == nil`; the breakpoint bound via
> `substitutePath` (`/build` → local checkout).
> **Fix:** validate the cache record before dereferencing, or repopulate on miss.
