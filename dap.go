package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/go-dap"
)

// errReadTimeout is returned by ReadMessageWithTimeout when no message arrives
// before the deadline. The blocking read itself is never interrupted: the
// background reader keeps the next complete message buffered for the following
// read, so no data is lost.
var errReadTimeout = errors.New("dap: read timed out")

// readWriteCloser combines separate reader and writer into io.ReadWriteCloser.
type readWriteCloser struct {
	io.Reader
	io.WriteCloser
}

// DAPClient is a synchronous Debug Adapter Protocol client.
// It manages a connection to a DAP server and provides methods for
// sending each DAP request type. Each request method returns the
// sequence number of the sent request, which callers use to match
// the corresponding response via request_seq.
type DAPClient struct {
	rwc    io.ReadWriteCloser
	reader *bufio.Reader
	// seq tracks the sequence number for each request sent to the server.
	seq int

	logMu     sync.Mutex
	logWriter io.Writer

	// The background reader (readLoop) appends every message it reads to an
	// unbounded queue and signals notify. It must never block handing a message
	// off, so the socket is drained the instant data arrives: if the reader
	// stalled (as a bounded channel would once full), the peer's writes would
	// block on a full socket buffer, and that in turn deadlocks our own next
	// write. dlv emits a burst of messages on every stop (proportional to the
	// number of goroutines), so this backlog is real, not theoretical.
	queueMu sync.Mutex
	queue   []readResult
	notify  chan struct{} // buffered(1); signals the queue became non-empty
}

// readResult is a single decoded message or the error that ended the stream.
type readResult struct {
	msg dap.Message
	err error
}

// dialTimeout bounds the TCP connect to a (possibly remote) DAP server so a
// black-holed address cannot hang the debug tool indefinitely.
const dialTimeout = 10 * time.Second

// initializeTimeout bounds the wait for the adapter's initialize response.
const initializeTimeout = 30 * time.Second

// newDAPClient creates a new Client over a TCP connection.
// Call Close to close the connection.
func newDAPClient(addr string) (*DAPClient, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connecting to DAP server at %s: %w", addr, err)
	}
	return newDAPClientFromRWC(conn), nil
}

// newDAPClientFromRWC creates a new Client with the given ReadWriteCloser.
// Call Close to close the underlying transport.
func newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient {
	c := &DAPClient{
		rwc:    rwc,
		reader: bufio.NewReader(rwc),
		seq:    1, // match VS Code numbering
		notify: make(chan struct{}, 1),
	}
	go c.readLoop()
	return c
}

// Close closes the client connection. This unblocks the background reader,
// which appends the resulting read error to the queue and exits.
func (c *DAPClient) Close() {
	c.rwc.Close()
}

// SetProtocolLogger sets a writer for logging all DAP messages sent and received.
func (c *DAPClient) SetProtocolLogger(w io.Writer) {
	c.logMu.Lock()
	c.logWriter = w
	c.logMu.Unlock()
}

// readLoop reads complete DAP messages off the wire and appends them to the
// unbounded queue, never blocking on a slow consumer. Running the blocking read
// in its own goroutine lets callers apply a timeout (see ReadMessageWithTimeout)
// without interrupting a read mid-message, which would desynchronize the stream.
// It exits after the first read error (e.g. once Close shuts the transport down).
func (c *DAPClient) readLoop() {
	for {
		msg, err := dap.ReadProtocolMessage(c.reader)
		if err == nil {
			c.logMu.Lock()
			w := c.logWriter
			c.logMu.Unlock()
			if w != nil {
				if data, merr := json.Marshal(msg); merr == nil {
					fmt.Fprintf(w, "RECV: <<<%s>>>\n", data)
				}
			}
		}
		c.enqueue(readResult{msg: msg, err: err})
		if err != nil {
			return
		}
	}
}

// enqueue appends a read result and wakes a waiting reader. The notify channel
// is buffered to 1 and the send is non-blocking, so a backlog of appends
// coalesces into a single pending signal; every reader re-checks the queue
// after each wake, so a coalesced signal never loses a message.
func (c *DAPClient) enqueue(r readResult) {
	c.queueMu.Lock()
	c.queue = append(c.queue, r)
	c.queueMu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// dequeue removes and returns the front of the queue, or ok=false if empty.
func (c *DAPClient) dequeue() (readResult, bool) {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if len(c.queue) == 0 {
		return readResult{}, false
	}
	r := c.queue[0]
	if len(c.queue) == 1 {
		c.queue = nil // release the backing array once drained
	} else {
		c.queue[0] = readResult{}
		c.queue = c.queue[1:]
	}
	return r, true
}

// InitializeRequest sends an 'initialize' request and returns the server's capabilities.
func (c *DAPClient) InitializeRequest(adapterID string) (dap.Capabilities, error) {
	req := c.newRequest("initialize")
	request := &dap.InitializeRequest{Request: *req}
	request.Arguments = dap.InitializeRequestArguments{
		AdapterID:                    adapterID,
		PathFormat:                   "path",
		LinesStartAt1:                true,
		ColumnsStartAt1:              true,
		SupportsVariableType:         true,
		SupportsVariablePaging:       true,
		SupportsRunInTerminalRequest: false,
		Locale:                       "en-us",
	}
	if err := c.send(request); err != nil {
		return dap.Capabilities{}, err
	}
	deadline := time.Now().Add(initializeTimeout)
	for {
		msg, err := c.ReadMessageWithTimeout(time.Until(deadline))
		if err != nil {
			if errors.Is(err, errReadTimeout) {
				return dap.Capabilities{}, fmt.Errorf("timed out after %s waiting for initialize response", initializeTimeout)
			}
			return dap.Capabilities{}, err
		}
		switch resp := msg.(type) {
		case *dap.InitializeResponse:
			if !resp.Success {
				return dap.Capabilities{}, fmt.Errorf("initialize failed: %s", resp.Message)
			}
			return resp.Body, nil
		case dap.EventMessage:
			// Skip events (e.g. OutputEvent) during initialization and keep reading
			continue
		default:
			return dap.Capabilities{}, fmt.Errorf("expected InitializeResponse, got %T", msg)
		}
	}
}

// ReadMessage returns the next DAP message, blocking until one is available
// or the stream ends.
func (c *DAPClient) ReadMessage() (dap.Message, error) {
	for {
		if r, ok := c.dequeue(); ok {
			return r.msg, r.err
		}
		<-c.notify
	}
}

// ReadMessageWithTimeout returns the next DAP message, or errReadTimeout if
// none arrives within d. On timeout the next message stays queued for a
// subsequent read, so no data is lost. A non-positive d means the deadline has
// already passed: it polls without blocking and returns errReadTimeout if no
// message is immediately available. It therefore never blocks indefinitely,
// even when a caller computes a remaining duration that has gone negative.
func (c *DAPClient) ReadMessageWithTimeout(d time.Duration) (dap.Message, error) {
	if r, ok := c.dequeue(); ok {
		return r.msg, r.err
	}
	if d <= 0 {
		return nil, errReadTimeout
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-c.notify:
			if r, ok := c.dequeue(); ok {
				return r.msg, r.err
			}
		case <-t.C:
			return nil, errReadTimeout
		}
	}
}

// LaunchRequest sends a 'launch' request with the specified args.
func (c *DAPClient) LaunchRequest(mode, program string, stopOnEntry bool, args []string) (int, error) {
	req := c.newRequest("launch")
	request := &dap.LaunchRequest{Request: *req}
	launchArgs := map[string]any{
		"request":     "launch",
		"mode":        mode,
		"program":     program,
		"stopOnEntry": stopOnEntry,
	}
	if len(args) > 0 {
		launchArgs["args"] = args
	}
	request.Arguments = toRawMessage(launchArgs)
	return req.Seq, c.send(request)
}

// CoreRequest sends a 'launch' request in core dump mode.
func (c *DAPClient) CoreRequest(program, coreFilePath string) (int, error) {
	req := c.newRequest("launch")
	request := &dap.LaunchRequest{Request: *req}
	request.Arguments = toRawMessage(map[string]any{
		"request":      "launch",
		"mode":         "core",
		"program":      program,
		"coreFilePath": coreFilePath,
	})
	return req.Seq, c.send(request)
}

// newRequest creates a new DAP request with the given command and an
// auto-incremented sequence number. The caller can read the assigned
// sequence number from the returned request's Seq field.
func (c *DAPClient) newRequest(command string) *dap.Request {
	request := &dap.Request{}
	request.Type = "request"
	request.Command = command
	request.Seq = c.seq
	c.seq++
	return request
}

// writeTimeout bounds a single request write. A sub-KB DAP write essentially
// never blocks, but a live-but-unresponsive peer (e.g. a wedged port-forward
// whose receive window has gone to zero) could in theory stall it forever. On a
// TCP transport we cap it with a write deadline; a timed-out write leaves the
// stream unusable, so the error is fatal to the session (the caller tears it
// down). Stdio transports (gdb) have no remote peer and are left unbounded.
const writeTimeout = 10 * time.Second

func (c *DAPClient) send(request dap.Message) error {
	if c.logWriter != nil {
		if data, err := json.Marshal(request); err == nil {
			fmt.Fprintf(c.logWriter, "SENT: <<<%s>>>\n", data)
		}
	}
	if conn, ok := c.rwc.(net.Conn); ok {
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	return dap.WriteProtocolMessage(c.rwc, request)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// SetBreakpointsRequest sends a 'setBreakpoints' request.
func (c *DAPClient) SetBreakpointsRequest(file string, lines []int) (int, error) {
	req := c.newRequest("setBreakpoints")
	request := &dap.SetBreakpointsRequest{Request: *req}
	request.Arguments = dap.SetBreakpointsArguments{
		Source: dap.Source{
			Name: file,
			Path: file,
		},
		Breakpoints: make([]dap.SourceBreakpoint, len(lines)),
	}
	for i, l := range lines {
		request.Arguments.Breakpoints[i].Line = l
	}
	return req.Seq, c.send(request)
}

// SetFunctionBreakpointsRequest sends a 'setFunctionBreakpoints' request.
func (c *DAPClient) SetFunctionBreakpointsRequest(functions []string) (int, error) {
	req := c.newRequest("setFunctionBreakpoints")
	request := &dap.SetFunctionBreakpointsRequest{Request: *req}
	request.Arguments = dap.SetFunctionBreakpointsArguments{
		Breakpoints: make([]dap.FunctionBreakpoint, len(functions)),
	}
	for i, f := range functions {
		request.Arguments.Breakpoints[i].Name = f
	}
	return req.Seq, c.send(request)
}

// ConfigurationDoneRequest sends a 'configurationDone' request.
func (c *DAPClient) ConfigurationDoneRequest() (int, error) {
	req := c.newRequest("configurationDone")
	request := &dap.ConfigurationDoneRequest{Request: *req}
	return req.Seq, c.send(request)
}

// ContinueRequest sends a 'continue' request.
func (c *DAPClient) ContinueRequest(threadID int) (int, error) {
	req := c.newRequest("continue")
	request := &dap.ContinueRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// NextRequest sends a 'next' request.
func (c *DAPClient) NextRequest(threadID int) (int, error) {
	req := c.newRequest("next")
	request := &dap.NextRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// StepInRequest sends a 'stepIn' request.
func (c *DAPClient) StepInRequest(threadID int) (int, error) {
	req := c.newRequest("stepIn")
	request := &dap.StepInRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// StepOutRequest sends a 'stepOut' request.
func (c *DAPClient) StepOutRequest(threadID int) (int, error) {
	req := c.newRequest("stepOut")
	request := &dap.StepOutRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// PauseRequest sends a 'pause' request.
func (c *DAPClient) PauseRequest(threadID int) (int, error) {
	req := c.newRequest("pause")
	request := &dap.PauseRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// ThreadsRequest sends a 'threads' request.
func (c *DAPClient) ThreadsRequest() (int, error) {
	req := c.newRequest("threads")
	request := &dap.ThreadsRequest{Request: *req}
	return req.Seq, c.send(request)
}

// StackTraceRequest sends a 'stackTrace' request.
func (c *DAPClient) StackTraceRequest(threadID, startFrame, levels int) (int, error) {
	req := c.newRequest("stackTrace")
	request := &dap.StackTraceRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	request.Arguments.StartFrame = startFrame
	request.Arguments.Levels = levels
	return req.Seq, c.send(request)
}

// ScopesRequest sends a 'scopes' request.
func (c *DAPClient) ScopesRequest(frameID int) (int, error) {
	req := c.newRequest("scopes")
	request := &dap.ScopesRequest{Request: *req}
	request.Arguments.FrameId = frameID
	return req.Seq, c.send(request)
}

// VariablesRequest sends a 'variables' request.
func (c *DAPClient) VariablesRequest(variablesReference int) (int, error) {
	req := c.newRequest("variables")
	request := &dap.VariablesRequest{Request: *req}
	request.Arguments.VariablesReference = variablesReference
	return req.Seq, c.send(request)
}

// EvaluateRequest sends an 'evaluate' request.
// We build the arguments as raw JSON instead of using dap.EvaluateArguments
// because go-dap uses omitempty on FrameId, which drops frameId=0 from the
// wire. GDB's native DAP uses 0-based frame IDs, so omitting frameId=0
// causes evaluation in global scope where local variables aren't visible.
func (c *DAPClient) EvaluateRequest(expression string, frameID int, context string) (int, error) {
	req := c.newRequest("evaluate")
	args := map[string]any{
		"expression": expression,
		"frameId":    frameID,
	}
	if context != "" {
		args["context"] = context
	}
	msg := struct {
		dap.Request
		Arguments map[string]any `json:"arguments"`
	}{Request: *req, Arguments: args}
	if c.logWriter != nil {
		if data, err := json.Marshal(&msg); err == nil {
			fmt.Fprintf(c.logWriter, "SENT: <<<%s>>>\n", data)
		}
	}
	return req.Seq, dap.WriteProtocolMessage(c.rwc, &msg)
}

// DisconnectRequest sends a 'disconnect' request.
func (c *DAPClient) DisconnectRequest(terminateDebuggee bool) (int, error) {
	req := c.newRequest("disconnect")
	request := &dap.DisconnectRequest{Request: *req}
	request.Arguments = &dap.DisconnectArguments{
		TerminateDebuggee: terminateDebuggee,
	}
	return req.Seq, c.send(request)
}

// ExceptionInfoRequest sends an 'exceptionInfo' request.
func (c *DAPClient) ExceptionInfoRequest(threadID int) (int, error) {
	req := c.newRequest("exceptionInfo")
	request := &dap.ExceptionInfoRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// SetVariableRequest sends a 'setVariable' request.
func (c *DAPClient) SetVariableRequest(variablesRef int, name, value string) (int, error) {
	req := c.newRequest("setVariable")
	request := &dap.SetVariableRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	request.Arguments.Value = value
	return req.Seq, c.send(request)
}

// RestartRequest sends a 'restart' request with specified arguments, if provided.
func (c *DAPClient) RestartRequest(arguments map[string]any) (int, error) {
	req := c.newRequest("restart")
	request := &dap.RestartRequest{Request: *req}
	if arguments != nil {
		request.Arguments = toRawMessage(arguments)
	}
	return req.Seq, c.send(request)
}

// TerminateRequest sends a 'terminate' request.
func (c *DAPClient) TerminateRequest() (int, error) {
	req := c.newRequest("terminate")
	request := &dap.TerminateRequest{Request: *req}
	return req.Seq, c.send(request)
}

// StepBackRequest sends a 'stepBack' request.
func (c *DAPClient) StepBackRequest(threadID int) (int, error) {
	req := c.newRequest("stepBack")
	request := &dap.StepBackRequest{Request: *req}
	request.Arguments.ThreadId = threadID
	return req.Seq, c.send(request)
}

// LoadedSourcesRequest sends a 'loadedSources' request.
func (c *DAPClient) LoadedSourcesRequest() (int, error) {
	req := c.newRequest("loadedSources")
	request := &dap.LoadedSourcesRequest{Request: *req}
	return req.Seq, c.send(request)
}

// ModulesRequest sends a 'modules' request.
func (c *DAPClient) ModulesRequest() (int, error) {
	req := c.newRequest("modules")
	request := &dap.ModulesRequest{Request: *req}
	return req.Seq, c.send(request)
}

// BreakpointLocationsRequest sends a 'breakpointLocations' request.
func (c *DAPClient) BreakpointLocationsRequest(source string, line int) (int, error) {
	req := c.newRequest("breakpointLocations")
	request := &dap.BreakpointLocationsRequest{Request: *req}
	request.Arguments.Source = dap.Source{
		Path: source,
	}
	request.Arguments.Line = line
	return req.Seq, c.send(request)
}

// CompletionsRequest sends a 'completions' request.
func (c *DAPClient) CompletionsRequest(text string, column int, frameID int) (int, error) {
	req := c.newRequest("completions")
	request := &dap.CompletionsRequest{Request: *req}
	request.Arguments.Text = text
	request.Arguments.Column = column
	request.Arguments.FrameId = frameID
	return req.Seq, c.send(request)
}

// DisassembleRequest sends a 'disassemble' request.
func (c *DAPClient) DisassembleRequest(memoryReference string, instructionOffset, instructionCount int) (int, error) {
	req := c.newRequest("disassemble")
	request := &dap.DisassembleRequest{Request: *req}
	request.Arguments.MemoryReference = memoryReference
	request.Arguments.InstructionOffset = instructionOffset
	request.Arguments.InstructionCount = instructionCount
	return req.Seq, c.send(request)
}

// SetExceptionBreakpointsRequest sends a 'setExceptionBreakpoints' request.
func (c *DAPClient) SetExceptionBreakpointsRequest(filters []string) (int, error) {
	req := c.newRequest("setExceptionBreakpoints")
	request := &dap.SetExceptionBreakpointsRequest{Request: *req}
	request.Arguments.Filters = filters
	return req.Seq, c.send(request)
}

// DataBreakpointInfoRequest sends a 'dataBreakpointInfo' request.
func (c *DAPClient) DataBreakpointInfoRequest(variablesRef int, name string) (int, error) {
	req := c.newRequest("dataBreakpointInfo")
	request := &dap.DataBreakpointInfoRequest{Request: *req}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	return req.Seq, c.send(request)
}

// SetDataBreakpointsRequest sends a 'setDataBreakpoints' request.
func (c *DAPClient) SetDataBreakpointsRequest(breakpoints []dap.DataBreakpoint) (int, error) {
	req := c.newRequest("setDataBreakpoints")
	request := &dap.SetDataBreakpointsRequest{Request: *req}
	request.Arguments.Breakpoints = breakpoints
	return req.Seq, c.send(request)
}

// SourceRequest sends a 'source' request.
func (c *DAPClient) SourceRequest(sourceRef int) (int, error) {
	req := c.newRequest("source")
	request := &dap.SourceRequest{Request: *req}
	request.Arguments.SourceReference = sourceRef
	return req.Seq, c.send(request)
}

// AttachRequest sends an 'attach' request.
func (c *DAPClient) AttachRequest(mode string, processID int) (int, error) {
	req := c.newRequest("attach")
	request := &dap.AttachRequest{Request: *req}
	request.Arguments = toRawMessage(map[string]any{
		"request":   "attach",
		"mode":      mode,
		"processId": processID,
	})
	return req.Seq, c.send(request)
}
