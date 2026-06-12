# MCP DAP Server

A Model Context Protocol (MCP) server that provides debugging capabilities through the Debug Adapter Protocol (DAP). This server enables AI assistants and other MCP clients to interact with debuggers for various programming languages.

## Overview

The MCP DAP Server acts as a bridge between MCP clients and DAP-compatible debuggers, allowing programmatic control of debugging sessions. It provides a comprehensive set of debugging tools that can be used to:

- Start and stop debugging sessions
- Set breakpoints (line-based and function-based)
- Control program execution (continue, step in/out/over, pause)
- Inspect program state (threads, stack traces, variables, scopes)
- Evaluate expressions
- Attach to running processes
- Handle exceptions

## Demos

- [Basic demo with multiple prompts](https://youtu.be/q0pNfhxWAWk?si=hzJWCyXnNsVKZ3Z4)
- [Autonomous agentic debugging pt.1](https://youtu.be/k5Z51et_rog?si=Z7VZWK8QQ94Pzptu)
- [Autonomous agentic debugging pt.2](https://youtu.be/8PcfLbU_EQM?si=I8y_RLjaWeT3B4I8)

## Features

- **Unified Debug Launch**: Single `debug` tool handles source, binary, and attach modes
- **Automatic Context**: Execution control tools return full state (location, stack, variables)
- **Streamlined API**: 13 tools cover all debugging operations
- **Breakpoint Management**: Set line and function breakpoints, run-to-cursor
- **Full State Inspection**: Stack traces, scopes, and variables in one call
- **Expression Evaluation**: Evaluate and modify variables in context
- **Process Attachment**: Attach to running processes
- **Remote Debugging**: Connect to a remote DAP server (e.g. `dlv dap` in a container) instead of spawning one locally
- **Disassembly Support**: View disassembled code at memory addresses

## Installation

### Prerequisites
- Go 1.24.4 or later
- A DAP-compatible debugger for your target language

### Building from Source

```bash
git clone https://github.com/go-delve/mcp-dap-server
cd mcp-dap-server
go build -o bin/mcp-dap-server
```

## Usage

### Connecting via MCP

The server uses stdio transport, allowing AI agents to spawn it on-demand. Configure your MCP client with the path to the binary.

### Example MCP Client Configuration

This configuration works with [Gemini CLI](https://developers.google.com/gemini-code-assist/docs/use-agentic-chat-pair-programmer#configure-mcp-servers) and similar MCP clients:

```json
{
  "mcpServers": {
    "dap-debugger": {
      "command": "mcp-dap-server",
      "args": [],
      "env": {}
    }
  }
}
```

### Claude Code Configuration

```bash
claude mcp add mcp-dap-server /path/to/mcp-dap-server
```

## Available Tools

### Session Management

#### `debug`
Start a debugging session. Supports four modes:
- **source**: Compile and debug Go source code
- **binary**: Debug a pre-compiled executable
- **core**: Debug a core dump file
- **attach**: Attach to a running process

**Parameters**:
- `mode` (string, required): One of 'source', 'binary', 'core', or 'attach'
- `path` (string): Path to source file or binary (required for source/binary modes; optional for core mode with GDB, which can auto-detect it)
- `args` (array): Arguments to pass to the program
- `coreFilePath` (string): Path to core dump file (required for core mode)
- `processId` (number): Process ID (required for attach mode)
- `breakpoints` (array): Breakpoints to set before running (file:line or function name)
- `stopOnEntry` (boolean): Stop at program entry point
- `port` (number): Port for the locally-spawned DAP server (ignored when `address` is set)
- `address` (string): `host:port` of an already-running DAP server to connect to (e.g. a remote `dlv dap --listen`). When set, no local debugger is spawned; `mode`/`path`/`processId` are interpreted on the remote host. See [Remote debugging](#remote-debugging-kubernetes-containers).
- `substitutePath` (array): Source path mappings (Delve only), each `{ "from": "<remote build path>", "to": "<local path>" }`, so breakpoints set by local path bind to the remote binary's build paths.

Returns full context (location, stack trace, variables) when stopped.

#### `stop`
End the debugging session. Terminates the debuggee and stops the debugger.

#### `restart`
Restart the debugging session with optional new arguments.
- **Parameters**:
  - `arguments` (array, optional): New program arguments

### Breakpoints

#### `breakpoint`
Set a breakpoint at a file:line location or on a function.
- **Parameters** (one of):
  - `file` (string) + `line` (number): Source file and line number
  - `function` (string): Function name

#### `clear-breakpoints`
Remove breakpoints from a file or clear all breakpoints.
- **Parameters**:
  - `file` (string, optional): Clear breakpoints in this file
  - `all` (boolean, optional): Clear all breakpoints

### Execution Control

#### `continue`
Continue program execution. Optionally run to a specific location.
- **Parameters**:
  - `to` (object, optional): Run-to-cursor target (file+line or function)

Returns full context when stopped.

#### `step`
Step through code execution.
- **Parameters**:
  - `mode` (string, required): One of 'over', 'in', or 'out'

Returns full context at new location.

#### `pause`
Pause program execution.
- **Parameters**:
  - `threadId` (number): Thread ID to pause

### State Inspection

#### `context`
Get full debugging context including current location, stack trace, and all variables.
- **Parameters**:
  - `threadId` (number, optional): Thread ID
  - `frameId` (number, optional): Stack frame ID

#### `evaluate`
Evaluate an expression in the current debugging context.
- **Parameters**:
  - `expression` (string): Expression to evaluate
  - `frameId` (number, optional): Frame context
  - `context` (string, optional): Evaluation context ('watch', 'repl', 'hover')

#### `set-variable`
Modify a variable's value in the debugged program.
- **Parameters**:
  - `variablesReference` (number): Variables reference from context
  - `name` (string): Variable name
  - `value` (string): New value

### Program Information

#### `info`
Get program metadata.
- **Parameters**:
  - `type` (string, required): One of 'sources' or 'modules'

#### `disassemble`
Disassemble code at a memory address.
- **Parameters**:
  - `memoryReference` (string): Memory address
  - `instructionOffset` (number, optional): Offset from address
  - `instructionCount` (number): Number of instructions to disassemble

## Remote debugging (Kubernetes containers)

Instead of spawning a debugger locally, the `debug` tool can **connect to a DAP
server that is already running elsewhere** by passing `address` (`host:port`).
This is the recommended way to debug a Go binary running in a remote container:
the container runs `dlv dap`, you forward its port to your machine, and the MCP
server dials it. The full tool surface (breakpoints, stepping, variables, eval,
disassemble) works identically over the connection.

### 1. Run a DAP server in the debug image

```dockerfile
# Build the binary with debug info (no inlining/optimization):
#   go build -gcflags=all="-N -l" -o /app/server .
ENTRYPOINT ["dlv", "dap", "--listen=:40000", "--only-same-user=false", "--api-version=2"]
```

`--only-same-user=false` is required because the connection arrives over TCP.
`dlv dap` serves a single client and exits when the session ends — fine for an
on-demand debug pod.

### 2. Expose the port

```bash
kubectl port-forward pod/my-pod 40000:40000
```

### 3. Connect and debug

```json
{
  "mode": "binary",
  "path": "/app/server",
  "address": "127.0.0.1:40000",
  "substitutePath": [
    { "from": "/build", "to": "/Users/me/project" }
  ]
}
```

- `path` (and `processId` for `mode: "attach"`) refer to the **remote** filesystem.
- `stop` on a remote session **detaches by default** (it won't kill the remote
  workload); pass `terminate=true` to force termination.

### `substitutePath`: making breakpoints bind

A breakpoint set by your **local** file path only binds if that path matches the
**build path** compiled into the remote binary. In CI these usually differ, so
breakpoints silently fail to verify. `substitutePath` maps one to the other.

Example: CI built the binary under `/build`, your checkout is at
`/Users/me/project`:

```json
"substitutePath": [
  { "from": "/build", "to": "/Users/me/project" }
]
```

Now a local breakpoint at `/Users/me/project/main.go:42` resolves to the remote
`/build/main.go:42`. To discover the remote build path, connect first and look at
the `File:` path printed by `context()` — that is the path the binary knows; map
your local root onto it. Omit `substitutePath` entirely when the paths already
match (e.g. the remote debugger shares your filesystem).

> Need reconnect / detach-and-reattach, or the app to keep running whether or not
> a debugger is attached? Run the remote as
> `dlv --headless --listen=:40000 --accept-multiclient ... exec /app/server`
> instead. Direct-dial via `address` targets `dlv dap`; a future `connect` mode
> will target headless servers.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

MIT

## Acknowledgments

- Built with the [Model Context Protocol SDK for Go](https://github.com/modelcontextprotocol/go-sdk)
- Uses the [Google DAP implementation for Go](https://github.com/google/go-dap)
