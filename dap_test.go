package main

import (
	"bufio"
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/google/go-dap"
)

// TestReaderDrainsBurstWithoutConsumer reproduces the burst deadlock. dlv emits
// a burst of messages on every stop (proportional to the number of goroutines).
// The background reader must drain them off the socket immediately even with no
// active consumer; otherwise the peer's writes block on a full socket buffer,
// which in turn deadlocks our own next write (observed in the field as a write
// i/o timeout on the first request after a stop in a busy process).
func TestReaderDrainsBurstWithoutConsumer(t *testing.T) {
	clientReader, serverWriter := io.Pipe() // server writes -> client reads
	_, clientWriter := io.Pipe()            // client write side (unused here)

	client := newDAPClientFromRWC(&readWriteCloser{
		Reader:      clientReader,
		WriteCloser: clientWriter,
	})
	defer client.Close()
	defer serverWriter.Close() // unblock the background reader at teardown

	// Far more than the previous fixed buffer (16). io.Pipe is synchronous, so
	// each write blocks until the reader consumes it.
	const burst = 64
	writeDone := make(chan error, 1)
	go func() {
		for i := 0; i < burst; i++ {
			ev := &dap.OutputEvent{
				Event: dap.Event{
					ProtocolMessage: dap.ProtocolMessage{Seq: i, Type: "event"},
					Event:           "output",
				},
				Body: dap.OutputEventBody{Category: "stdout", Output: "x\n"},
			}
			if err := dap.WriteProtocolMessage(serverWriter, ev); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()

	// With an unbounded reader the writer completes the whole burst; with a
	// bounded buffer and no consumer it blocks partway and never finishes.
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("server write failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server writer blocked: reader did not drain the burst without a consumer")
	}

	// Every buffered message must still be retrievable in order.
	for got := 0; got < burst; got++ {
		msg, err := client.ReadMessageWithTimeout(2 * time.Second)
		if err != nil {
			t.Fatalf("ReadMessage %d/%d: %v", got, burst, err)
		}
		if _, ok := msg.(*dap.OutputEvent); !ok {
			t.Fatalf("message %d: expected *dap.OutputEvent, got %T", got, msg)
		}
	}
}

func TestNewDAPClientFromRWC(t *testing.T) {
	// Create a pipe to simulate a bidirectional connection
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	rwc := &readWriteCloser{
		Reader:      clientReader,
		WriteCloser: clientWriter,
	}

	client := newDAPClientFromRWC(rwc)
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	// Send an initialize request through the client
	go func() {
		req := client.newRequest("initialize")
		_ = client.send(&dap.InitializeRequest{
			Request: *req,
		})
	}()

	// Read the message from the server side
	msg, err := dap.ReadProtocolMessage(bufio.NewReader(serverReader))
	if err != nil {
		t.Fatalf("failed to read message from server side: %v", err)
	}

	if _, ok := msg.(*dap.InitializeRequest); !ok {
		t.Fatalf("expected InitializeRequest, got %T", msg)
	}

	// Write a response from the server side
	go func() {
		resp := &dap.InitializeResponse{}
		resp.Response.RequestSeq = 1
		resp.Response.Command = "initialize"
		resp.Response.Success = true
		resp.Seq = 1
		resp.Type = "response"
		_ = dap.WriteProtocolMessage(serverWriter, resp)
	}()

	// Read the response through the client
	respMsg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if _, ok := respMsg.(*dap.InitializeResponse); !ok {
		t.Fatalf("expected InitializeResponse, got %T", respMsg)
	}

	client.Close()

	// Verify close propagated (write to closed pipe should fail)
	var buf bytes.Buffer
	buf.WriteString("test")
	_, err = clientWriter.Write(buf.Bytes())
	if err == nil {
		t.Error("expected error writing to closed connection")
	}
}
