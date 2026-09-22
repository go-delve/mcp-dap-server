package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/google/go-dap"
)

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

func TestDAPClientPreservesOutOfOrderMessages(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	client := newDAPClientFromRWC(clientConn)
	defer client.Close()
	handled := make(chan dap.EventMessage, 1)
	client.SetEventHandler(func(event dap.EventMessage) {
		handled <- event
	})

	go func() {
		_ = dap.WriteProtocolMessage(serverConn, &dap.ContinueResponse{
			Response: dap.Response{
				ProtocolMessage: dap.ProtocolMessage{Seq: 3, Type: "response"},
				RequestSeq:      2,
				Success:         true,
				Command:         "continue",
			},
		})
		_ = dap.WriteProtocolMessage(serverConn, &dap.StoppedEvent{
			Event: dap.Event{ProtocolMessage: dap.ProtocolMessage{Seq: 4, Type: "event"}, Event: "stopped"},
			Body:  dap.StoppedEventBody{Reason: "breakpoint", ThreadId: 1},
		})
		_ = dap.WriteProtocolMessage(serverConn, &dap.PauseResponse{
			Response: dap.Response{
				ProtocolMessage: dap.ProtocolMessage{Seq: 5, Type: "response"},
				RequestSeq:      1,
				Success:         true,
				Command:         "pause",
			},
		})
	}()

	first, err := client.waitResponse(1)
	if err != nil {
		t.Fatal(err)
	}
	if first.(dap.ResponseMessage).GetResponse().RequestSeq != 1 {
		t.Fatalf("waitResponse(1) got %#v", first)
	}
	event, err := client.waitEvent()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := event.(*dap.StoppedEvent); !ok {
		t.Fatalf("waitEvent got %T", event)
	}
	if _, ok := (<-handled).(*dap.StoppedEvent); !ok {
		t.Fatal("central event handler did not receive stopped event")
	}
	second, err := client.waitResponse(2)
	if err != nil {
		t.Fatal(err)
	}
	if second.(dap.ResponseMessage).GetResponse().RequestSeq != 2 {
		t.Fatalf("waitResponse(2) got %#v", second)
	}
}
