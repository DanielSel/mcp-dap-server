package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerPrompts registers all debugging workflow prompts with the MCP server.
func registerPrompts(server *mcp.Server) {
	server.AddPrompt(&mcp.Prompt{
		Name:        "debug-source",
		Description: "Structured workflow for debugging a program from source code",
		Arguments: []*mcp.PromptArgument{
			{Name: "path", Required: true, Description: "Path to the source file or directory to debug"},
			{Name: "language", Required: false, Description: "Language: 'go' (default) or 'c'/'cpp'"},
			{Name: "breakpoints", Required: false, Description: "Comma-separated file:line pairs, e.g. 'main.go:42,server.go:100'"},
		},
	}, promptDebugSource)

	server.AddPrompt(&mcp.Prompt{
		Name:        "debug-attach",
		Description: "Structured workflow for attaching to a running process",
		Arguments: []*mcp.PromptArgument{
			{Name: "pid", Required: true, Description: "Process ID to attach to"},
			{Name: "program", Required: false, Description: "Description of what the program does (for context)"},
		},
	}, promptDebugAttach)

	server.AddPrompt(&mcp.Prompt{
		Name:        "debug-core-dump",
		Description: "Structured workflow for post-mortem analysis of a core dump",
		Arguments: []*mcp.PromptArgument{
			{Name: "binary_path", Required: true, Description: "Path to the executable that crashed"},
			{Name: "core_path", Required: true, Description: "Path to the core dump file"},
			{Name: "language", Required: false, Description: "Language: 'go' (default) or 'c'/'cpp'"},
		},
	}, promptDebugCoreDump)

	server.AddPrompt(&mcp.Prompt{
		Name:        "debug-binary",
		Description: "Structured workflow for assembly-level debugging of a compiled binary",
		Arguments: []*mcp.PromptArgument{
			{Name: "path", Required: true, Description: "Path to the compiled binary to debug"},
		},
	}, promptDebugBinary)

	server.AddPrompt(&mcp.Prompt{
		Name:        "debug-remote",
		Description: "Structured workflow for debugging a Go binary in a remote container via a remote dlv DAP server",
		Arguments: []*mcp.PromptArgument{
			{Name: "address", Required: true, Description: "host:port of the remote DAP server (e.g. a kubectl port-forwarded 'dlv dap --listen'), e.g. '127.0.0.1:40000'"},
			{Name: "remote_path", Required: false, Description: "Path to the binary on the remote host (for 'binary' mode), e.g. '/app/server'"},
			{Name: "pid", Required: false, Description: "Remote process ID to attach to (for 'attach' mode), e.g. '1'"},
			{Name: "local_source", Required: false, Description: "Local source root the MCP client uses, e.g. '/Users/me/project'"},
			{Name: "remote_source", Required: false, Description: "Source root compiled into the remote binary, e.g. '/build'"},
			{Name: "breakpoints", Required: false, Description: "Comma-separated file:line pairs (local paths), e.g. 'main.go:42'"},
		},
	}, promptDebugRemote)
}

func promptDebugSource(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	path := req.Params.Arguments["path"]
	language := req.Params.Arguments["language"]
	breakpoints := req.Params.Arguments["breakpoints"]

	if language == "" {
		language = "go"
	}

	debugger := "delve"
	mode := "source"
	compileNote := ""
	if language == "c" || language == "cpp" {
		debugger = "gdb"
		mode = "binary"
		compileNote = fmt.Sprintf(`
> **Note:** GDB does not support 'source' mode. Compile first with debug symbols:
> - C: `+"`"+`gcc -g -O0 -o myprogram %s`+"`"+`
> - C++: `+"`"+`g++ -g -O0 -o myprogram %s`+"`"+`
> Then use 'binary' mode with the compiled output path.`, path, path)
	}

	bpSection := ""
	if breakpoints != "" {
		bpSection = fmt.Sprintf(`
### Optional: Pre-set breakpoints
The following breakpoints were requested: `+"`"+`%s`+"`"+`

Include them in the debug call:
`+"```"+`json
{
  "mode": "%s",
  "path": "%s",
  "debugger": "%s",
  "breakpoints": [
    {"file": "/abs/path/to/file.go", "line": 42}
  ]
}
`+"```", breakpoints, mode, path, debugger)
	}

	content := fmt.Sprintf(`## Live Source Debug Session

You are debugging a **%s** program from source.%s

**Program path:** `+"`"+`%s`+"`"+`

---

### Step 1: Start the debug session

Call: `+"`"+`debug(mode="%s", path="%s", debugger="%s")`+"`"+`
%s
Expected output: The debugger starts and either stops at entry or waits at a breakpoint. You will see a stack trace and local variables.

**If the debug tool fails:**
- Check that `+"`"+`%s`+"`"+` is installed and in `+"`"+`$PATH`+"`"+`
- For Go: ensure the path points to a .go file or directory with a `+"`"+`main`+"`"+` package
- For C/C++: compile with -g -O0 first, then use binary mode

---

### Step 2: Set strategic breakpoints

Before running, identify the most useful locations:
- Entry points to the function or logic area of interest
- Error handling paths
- State transitions

Call: `+"`"+`breakpoint(file="/abs/path/to/file", line=N)`+"`"+`
Or by function: `+"`"+`breakpoint(function="packageName.FunctionName")`+"`"+`

---

### Step 3: Run to the first interesting point

Call: `+"`"+`continue()`+"`"+`

Expected: Stops at your breakpoint. Output includes:
- **Location**: file, line, function name
- **Stack trace**: full call chain
- **Variables**: locals and their current values

`+"`"+`continue`+"`"+` waits up to `+"`"+`timeoutSeconds`+"`"+` (default 10) for a stop, then **pauses
the program** and returns "Still running … paused" instead of blocking forever.
That's a normal checkpoint, not an error — call `+"`"+`continue()`+"`"+` again to keep going,
or use a shorter budget like `+"`"+`continue(timeoutSeconds=3)`+"`"+`. You can only inspect
while the program is stopped, never while it runs.

**What to look for:**
- Are variable values what you expect at this point?
- Is the call stack reasonable, or is something unexpected calling this function?

---

### Step 4: Inspect state

Call: `+"`"+`context()`+"`"+` at any time to refresh the current location, stack, and variables.

Call: `+"`"+`evaluate(expression="variableName")`+"`"+` to inspect specific values:
- Struct fields: `+"`"+`evaluate(expression="user.Address.City")`+"`"+`
- Slice/array elements: `+"`"+`evaluate(expression="items[0]")`+"`"+`
- Computed expressions: `+"`"+`evaluate(expression="len(queue)")`+"`"+`

---

### Step 5: Step through logic

- `+"`"+`step(mode="over")`+"`"+` — execute current line, staying in the same function
- `+"`"+`step(mode="in")`+"`"+` — step into a function call
- `+"`"+`step(mode="out")`+"`"+` — run until the current function returns

**Decision guide:**
- See an unexpected value? Step _in_ to the function that produced it
- At a line that doesn't matter? Step _over_ it
- Deep in a call you don't care about? Step _out_

---

### Step 6: Check threads (if concurrent program)

Call: `+"`"+`info(kind="threads")`+"`"+`

Look for: goroutines/threads in unexpected states, goroutines blocked on channels or mutexes.

---

### Step 7: Identify the root cause

By now you should be able to answer:
1. What is the actual (incorrect) value, and where did it come from?
2. What condition or code path led to this state?
3. Is this a logic error, a missing nil check, an off-by-one, a race condition, or something else?

State your findings clearly: **"The bug is at [file:line]. [Variable] has value [X] when it should be [Y] because [reason]."**

---

### Step 8: Clean up

Call: `+"`"+`stop()`+"`"+`
`,
		language, compileNote,
		path,
		mode, path, debugger,
		bpSection,
		func() string {
			if debugger == "delve" {
				return "dlv"
			}
			return "gdb (native DAP)"
		}(),
	)

	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Live source debugging workflow for %s", path),
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: content}},
		},
	}, nil
}

func promptDebugAttach(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	pid := req.Params.Arguments["pid"]
	program := req.Params.Arguments["program"]

	programDesc := ""
	if program != "" {
		programDesc = fmt.Sprintf("\n**Program description:** %s\n", program)
	}

	content := fmt.Sprintf(`## Live Attach Debug Session

You are attaching to a **running process** to diagnose its live behavior.%s

**Target PID:** `+"`"+`%s`+"`"+`

> **Important:** The process continues running after you attach. You are observing live state.
> Be careful: setting breakpoints in a production process will pause it for all users.

---

### Step 1: Attach to the process

Call: `+"`"+`debug(mode="attach", processId=%s)`+"`"+`

Expected: The debugger attaches and the process pauses. You will see the current execution location.

**If attach fails:**
- Verify the PID is correct: `+"`"+`ps aux | grep <process-name>`+"`"+`
- Check permissions: you may need to run as root or set `+"`"+`ptrace_scope`+"`"+`
- The process may have already exited

---

### Step 2: Understand what the process was doing

Immediately after attach, call: `+"`"+`context()`+"`"+`

Look for:
- **Current location**: What function/file is it in?
- **Stack trace**: What sequence of calls led here?
- **Variables**: What are the current local values?

If the process was in a system call or blocking operation, the stack will show that.

---

### Step 3: Check all threads

Call: `+"`"+`info(kind="threads")`+"`"+`

This is especially important for concurrent programs. Look for:
- Threads blocked on the same mutex (potential deadlock)
- Threads in unexpected functions
- Unexpectedly high thread counts

For each suspicious thread, inspect it:
Call: `+"`"+`context(threadId=<ID>)`+"`"+`

---

### Step 4: Set targeted breakpoints (carefully)

Only set breakpoints if you have a specific hypothesis to test.

Call: `+"`"+`breakpoint(function="packageName.FunctionName")`+"`"+`

Then resume: `+"`"+`continue()`+"`"+`

`+"`"+`continue`+"`"+` waits up to `+"`"+`timeoutSeconds`+"`"+` (default 10) for the breakpoint, then
**pauses the process** and returns "Still running … paused" rather than blocking.
For an attached (possibly production) process that pause **freezes it for all
users until you continue again** — so use a short budget and resume promptly:
`+"`"+`continue(timeoutSeconds=3)`+"`"+`, inspect, then `+"`"+`continue()`+"`"+` again.

The process resumes and runs until your breakpoint is hit. Inspect state at that point.

---

### Step 5: Diagnose the issue

Common attach scenarios and what to look for:

**High CPU usage:**
- Pause several times with `+"`"+`pause()`+"`"+` + `+"`"+`context()`+"`"+`
- Which function keeps appearing in the stack? That's likely the hot path.
- Look for tight loops, repeated work, or missing caches.

**Memory leak:**
- Check variables for large collections, caches without eviction, or accumulating slices.
- Use `+"`"+`evaluate()`+"`"+` to check sizes: `+"`"+`evaluate(expression="len(cache)")`+"`"+`

**Deadlock / hang:**
- All threads should show where they're blocked
- Look for goroutines stuck in channel operations or mutexes
- Check if any goroutine holds a lock that another is waiting for

**Unexpected behavior:**
- Set a breakpoint at the function exhibiting the behavior
- Inspect inputs and internal state

---

### Step 6: Conclude and detach

State your findings: **"The process is [doing X] because [reason]. The issue is [description]."**

To **terminate** the process: `+"`"+`stop()`+"`"+`
To **detach** (leave the process running): `+"`"+`stop(detach=true)`+"`"+`

> Use detach when you want to observe without disrupting the process long-term.
`,
		programDesc, pid, pid,
	)

	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Live attach debugging workflow for PID %s", pid),
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: content}},
		},
	}, nil
}

func promptDebugCoreDump(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	binaryPath := req.Params.Arguments["binary_path"]
	corePath := req.Params.Arguments["core_path"]
	language := req.Params.Arguments["language"]

	if language == "" {
		language = "go"
	}

	debugger := "delve"
	if language == "c" || language == "cpp" {
		debugger = "gdb"
	}

	signalGuide := `
**Signal interpretation:**
- ` + "`" + `SIGSEGV` + "`" + ` (segfault) — nil pointer dereference, use-after-free, buffer overflow, stack overflow
- ` + "`" + `SIGABRT` + "`" + ` — explicit abort, assertion failure, double-free (C/C++), runtime panic (Go)
- ` + "`" + `SIGFPE` + "`" + ` — arithmetic error: division by zero, integer overflow
- ` + "`" + `SIGBUS` + "`" + ` — misaligned memory access, unmapped file region
- ` + "`" + `SIGILL` + "`" + ` — illegal CPU instruction (often compiler bug or corrupted binary)
- ` + "`" + `SIGPIPE` + "`" + ` — write to closed pipe/socket with no signal handler`

	content := fmt.Sprintf(`## Post-Mortem Core Dump Analysis

You are analyzing a **crash captured in a core dump**. Execution is frozen at the moment of the crash — you cannot step forward, but you have full access to the crashed state.

**Program:** `+"`"+`%s`+"`"+`
**Core dump:** `+"`"+`%s`+"`"+`
**Debugger:** %s

> This is read-only analysis. You cannot change execution — only observe the crashed state.

---

### Step 1: Start the core dump session

Call: `+"`"+`debug(mode="core", path="%s", coreFilePath="%s", debugger="%s")`+"`"+`

Expected: The debugger loads the core dump and positions at the crash frame. You will see the crash location, stack trace, and local variables.

**If loading fails:**
- Ensure the binary matches the core dump exactly (same build, not rebuilt after crash)
- Check file paths are absolute and readable
- For Go: ensure Delve is installed (`+"`"+`dlv version`+"`"+`)
- For C/C++: ensure GDB 14+ is installed (`+"`"+`gdb -i dap`+"`"+`)

---

### Step 2: Understand the crash location

Call: `+"`"+`context()`+"`"+`

This is your most important call. Look for:
- **Crash function**: What function was executing when the crash occurred?
- **File and line**: Exact source location of the crash
- **Local variables**: What values were present at the crash frame?
- **Stack trace**: The full call chain leading to the crash
%s

---

### Step 3: Examine the crash frame variables

Look at every variable in the crash frame:
- Are any pointers nil? A nil dereference causes SIGSEGV.
- Are indices within bounds? Out-of-bounds access can cause SIGSEGV.
- Are any values nonsensical (e.g., negative size, huge number)? Suggests corruption.

Use `+"`"+`evaluate(expression="varName")`+"`"+` to inspect variables not shown in context, or to drill into nested fields:
- `+"`"+`evaluate(expression="err.Error()")`+"`"+`
- `+"`"+`evaluate(expression="request.Body")`+"`"+`
- `+"`"+`evaluate(expression="items[idx]")`+"`"+`

---

### Step 4: Walk up the call stack

Each frame in the stack trace tells part of the story. For each suspicious frame:

1. Use `+"`"+`context(frameId=<N>)`+"`"+` to inspect variables in that frame
2. Look for: what argument was passed to the crashing function? Was it already invalid?
3. Ask: could the caller have passed a nil/invalid value?

Work backwards from the crash until you find where the bad value originated.

---

### Step 5: Check other threads / goroutines

Call: `+"`"+`info(kind="threads")`+"`"+`

In multi-threaded programs, the crash may be triggered by another thread's action (race condition). Look for:
- Other threads at suspicious locations
- Threads that may have corrupted shared state

For each interesting thread: `+"`"+`context(threadId=<ID>)`+"`"+`

---

### Step 6: Formulate your conclusion

Answer these questions:
1. **What crashed?** (function, file, line)
2. **Why did it crash?** (nil pointer, invalid index, assertion failure, etc.)
3. **What bad value caused it?** (which variable, what value it had)
4. **Where did that bad value come from?** (trace back through the call stack)
5. **What is the root cause?** (missing nil check, logic error, race condition, etc.)

**Pattern recognition:**
- `+"`"+`SIGSEGV`+"`"+` on field access → check if the containing struct pointer is nil
- `+"`"+`SIGSEGV`+"`"+` in memory allocation → heap corruption (often from earlier use-after-free)
- `+"`"+`SIGABRT`+"`"+` in Go → runtime panic (check the panic message in variables)
- `+"`"+`SIGABRT`+"`"+` in C → double-free, assert(), or explicit abort()
- Very deep stack → infinite recursion (check for missing base case)
- Stack frame count = 1 in a goroutine → goroutine was running C code or in a syscall

---

### Step 7: Clean up

Call: `+"`"+`stop()`+"`"+`
`,
		binaryPath, corePath, debugger,
		binaryPath, corePath, debugger,
		signalGuide,
	)

	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Post-mortem core dump analysis for %s", binaryPath),
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: content}},
		},
	}, nil
}

func promptDebugBinary(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	path := req.Params.Arguments["path"]

	// Infer likely language/debugger from path or note that both are supported
	debuggerNote := `Use 'delve' for Go binaries, 'gdb' for C/C++/Rust binaries.
> - Go binary: ` + "`" + `debug(mode="binary", path="...", debugger="delve")` + "`" + `
> - C/C++ binary: ` + "`" + `debug(mode="binary", path="...", debugger="gdb")` + "`" + ``

	content := fmt.Sprintf(`## Binary / Assembly-Level Debug Session

You are debugging a **compiled binary without source context**. You will work primarily with disassembled instructions, registers, and memory addresses.

**Binary:** `+"`"+`%s`+"`"+`

> %s

---

### Step 1: Start the binary debug session

Call: `+"`"+`debug(mode="binary", path="%s", stopOnEntry=true)`+"`"+`

Using `+"`"+`stopOnEntry=true`+"`"+` pauses immediately so you can orient yourself before execution proceeds.

Expected: The debugger stops at the binary entry point or `+"`"+`main`+"`"+`. You will see the current instruction address and possibly a stack trace.

---

### Step 2: Get your bearings

Call: `+"`"+`context()`+"`"+`

Even without source, this shows:
- **Current location**: function name and address (e.g., `+"`"+`main.main at 0x004012a0`+"`"+`)
- **Stack trace**: the call chain with instruction pointer addresses
- **Variables**: any debug info that exists in the binary

Note the `+"`"+`instructionPointerReference`+"`"+` (memory address) from the stack trace — you will use this for disassembly.

---

### Step 3: Disassemble around the current location

Call: `+"`"+`disassemble(address="0x<instructionPointerReference>", count=30)`+"`"+`

Read the assembly output:
- Identify the instruction sequence (what is the function doing?)
- Look for `+"`"+`call`+"`"+` instructions (function calls)
- Look for `+"`"+`cmp`+"`"+`/`+"`"+`test`+"`"+` + conditional jumps (branch logic)
- Look for `+"`"+`mov`+"`"+` with memory operands (data access)

---

### Step 4: Set breakpoints by address or function

By function name (if debug symbols exist):
Call: `+"`"+`breakpoint(function="main.processRequest")`+"`"+`

By address (no symbols needed):
Call: `+"`"+`breakpoint(function="*0x00401234")`+"`"+`

> For GDB: you can use `+"`"+`*0xADDR`+"`"+` syntax to break at an absolute address.

---

### Step 5: Step through assembly

Use `+"`"+`step(mode="in")`+"`"+` to step one instruction at a time (at assembly level, each step may be one machine instruction).

After each step, call `+"`"+`context()`+"`"+` to see the new instruction pointer location.

**What to track:**
- Register values (available via `+"`"+`evaluate(expression="$rax")`+"`"+` in GDB sessions)
- Memory at specific addresses: `+"`"+`evaluate(expression="*(int*)0xADDR")`+"`"+`
- Stack pointer and frame pointer movement

---

### Step 6: Inspect memory and registers

Call: `+"`"+`evaluate(expression="$rsp")`+"`"+` — current stack pointer
Call: `+"`"+`evaluate(expression="$rip")`+"`"+` — current instruction pointer
Call: `+"`"+`evaluate(expression="$rax")`+"`"+` — return value register (x86-64)

For memory: `+"`"+`evaluate(expression="*(long*)0x<address>")`+"`"+`

**Calling convention reminders (x86-64 System V):**
- Function args: rdi, rsi, rdx, rcx, r8, r9 (in order)
- Return value: rax
- Preserved: rbx, rbp, r12-r15
- Scratch: rax, rcx, rdx, rsi, rdi, r8-r11

---

### Step 7: Use disassembly to understand logic

When you see a branch or call you want to understand:
1. Disassemble the target: `+"`"+`disassemble(address="0x<target>", count=40)`+"`"+`
2. Set a breakpoint there and `+"`"+`continue()`+"`"+`
3. Inspect register/memory state at that point

> `+"`"+`continue`+"`"+` waits up to `+"`"+`timeoutSeconds`+"`"+` (default 10), then **pauses** and
> returns "Still running … paused" instead of blocking. If an address breakpoint
> never binds (wrong address, code path not taken) you get that paused summary,
> not a hang — fix the address and continue again. Inspect only while stopped.

**Common patterns:**
- `+"`"+`call malloc`+"`"+` → memory allocation; check return in rax for NULL
- `+"`"+`test rax, rax; je`+"`"+` → null check
- `+"`"+`cmp [rsp+N], 0; jle`+"`"+` → bounds or sign check
- `+"`"+`rep stos`+"`"+` → memset-like loop

---

### Step 8: Formulate findings

After analysis, state:
1. **What the binary does** at the function/address of interest
2. **Where the issue is** (address, instruction, logic)
3. **What the expected vs actual behavior is**

---

### Step 9: Clean up

Call: `+"`"+`stop()`+"`"+`
`,
		path, debuggerNote, path,
	)

	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Assembly-level binary debugging workflow for %s", path),
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: content}},
		},
	}, nil
}

func promptDebugRemote(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	address := req.Params.Arguments["address"]
	remotePath := req.Params.Arguments["remote_path"]
	pid := req.Params.Arguments["pid"]
	localSource := req.Params.Arguments["local_source"]
	remoteSource := req.Params.Arguments["remote_source"]
	breakpoints := req.Params.Arguments["breakpoints"]

	// Decide which mode the worked example uses.
	mode := "binary"
	target := fmt.Sprintf(`"path": "%s"`, remotePath)
	if remotePath == "" {
		remotePath = "/app/server"
	}
	if pid != "" {
		mode = "attach"
		target = fmt.Sprintf(`"processId": %s`, pid)
	} else {
		target = fmt.Sprintf(`"path": "%s"`, remotePath)
	}

	// Build the substitutePath example. Use the provided roots if any, else
	// illustrative placeholders, so the user always sees a concrete mapping.
	// Delve's direction: from = local path, to = the path baked into the binary.
	localRoot := localSource
	buildRoot := remoteSource
	if localRoot == "" {
		localRoot = "/Users/me/project"
	}
	if buildRoot == "" {
		buildRoot = "/build"
	}

	content := fmt.Sprintf(`## Remote Debug Session (Go binary in a container)

You are debugging a Go program that runs under a **remote dlv DAP server** — for
example a Kubernetes pod whose debug image runs `+"`"+`dlv dap --listen=:40000 --only-same-user=false`+"`"+`,
reached from this machine via `+"`"+`kubectl port-forward`+"`"+`.

**Remote DAP address:** `+"`"+`%s`+"`"+`

> No local debugger is spawned. The MCP server dials the address and drives the
> remote dlv. `+"`"+`path`+"`"+`/`+"`"+`processId`+"`"+` are interpreted on the **remote** host.

---

### Step 0 (outside this tool): expose the remote port

`+"```"+`bash
# Run the debug image so dlv waits for a DAP client:
#   ENTRYPOINT ["dlv","dap","--listen=:40000","--only-same-user=false","--api-version=2"]
kubectl port-forward pod/my-pod 40000:40000
`+"```"+`

This makes the remote DAP server reachable at `+"`"+`127.0.0.1:40000`+"`"+`.

---

### Step 1: Connect and start debugging

Call: `+"`"+`debug(mode="%s", address="%s", %s, substitutePath=[{"from": "%s", "to": "%s"}])`+"`"+`

JSON form:
`+"```"+`json
{
  "mode": "%s",
  "address": "%s",
  %s,
  "substitutePath": [
    { "from": "%s", "to": "%s" }
  ]
}
`+"```"+`

**Why `+"`"+`substitutePath`+"`"+` matters:** breakpoints you set by your *local* file
path must be translated to the *build* paths compiled into the remote binary.
The direction is Delve's: **`+"`"+`from`+"`"+` = your local path, `+"`"+`to`+"`"+` = the path baked
into the binary** (get it backwards and *both* path forms fail to bind). If your
checkout lives at `+"`"+`%s`+"`"+` but CI built the binary under `+"`"+`%s`+"`"+`, the mapping above
lets a local breakpoint like `+"`"+`%s/main.go:42`+"`"+` bind to the binary's
`+"`"+`%s/main.go:42`+"`"+`. Omit it only if the paths are already identical.

> To find the build path (your `+"`"+`to`+"`"+`): connect **without** `+"`"+`substitutePath`+"`"+`,
> set a *function* breakpoint (resolves by symbol, no path needed), `+"`"+`continue`+"`"+`,
> and read the `+"`"+`File:`+"`"+` line from `+"`"+`context()`+"`"+` — that is the path the binary
> knows. Your local checkout root is your `+"`"+`from`+"`"+`.

---

### Step 2: Set breakpoints (local paths)
%s
Call: `+"`"+`breakpoint(file="%s/main.go", line=42)`+"`"+`

With `+"`"+`substitutePath`+"`"+` in place, the breakpoint resolves on the remote binary.
If a breakpoint reports "not verified", the path mapping is likely wrong — verify
the `+"`"+`File:`+"`"+` path shown by `+"`"+`context()`+"`"+` and adjust `+"`"+`from`+"`"+`/`+"`"+`to`+"`"+`.

---

### Step 3: Run and inspect

Call: `+"`"+`continue()`+"`"+`, then `+"`"+`context()`+"`"+`, `+"`"+`evaluate(...)`+"`"+`, and
`+"`"+`step(...)`+"`"+` exactly as in a local session — the full tool surface works
identically over the remote connection.

On a live service the breakpoint may not hit immediately. `+"`"+`continue`+"`"+` waits up to
`+"`"+`timeoutSeconds`+"`"+` (default 10), then **pauses** and returns "Still running …
paused" rather than blocking. Keep the budget short and **loop**: trigger the
request that exercises the breakpoint (curl the endpoint, send the message),
`+"`"+`continue(timeoutSeconds=5)`+"`"+`, inspect, repeat. Reaching for a huge timeout just
blinds you — you can only inspect while the program is stopped.

---

### Step 4: Disconnect safely

Call: `+"`"+`stop()`+"`"+`

For a remote (`+"`"+`address`+"`"+`) session this **detaches by default** so the remote
workload is not killed. To force termination of the remote debuggee, call
`+"`"+`stop(terminate=true)`+"`"+`.

> Note: `+"`"+`dlv dap`+"`"+` serves a single connection and exits when the session ends.
> To reconnect repeatedly or keep the app running independently of the debugger,
> run the remote as `+"`"+`dlv --headless --accept-multiclient`+"`"+` instead (a future
> 'connect' mode will target that setup).
`,
		address,
		mode, address, target, localRoot, buildRoot,
		mode, address, target, localRoot, buildRoot,
		localRoot, buildRoot, localRoot, buildRoot,
		func() string {
			if breakpoints != "" {
				return fmt.Sprintf("\nRequested breakpoints: `%s`\n", breakpoints)
			}
			return ""
		}(),
		localRoot,
	)

	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Remote debugging workflow via %s", address),
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: content}},
		},
	}, nil
}
