package main

import (
	"os/exec"
	"testing"
)

func TestDelveBackendSpawn(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not found in PATH")
	}

	backend := &delveBackend{}
	cmd, listenAddr, err := backend.Spawn(":0")
	if err != nil {
		t.Fatalf("failed to spawn delve: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	if listenAddr == "" {
		t.Error("expected non-empty listen address")
	}
	if backend.TransportMode() != "tcp" {
		t.Errorf("expected tcp transport, got: %s", backend.TransportMode())
	}
	t.Logf("Delve listening at: %s", listenAddr)
}

func TestDelveBackendLaunchArgs(t *testing.T) {
	backend := &delveBackend{}

	t.Run("source mode", func(t *testing.T) {
		args, err := backend.LaunchArgs("source", "/path/to/main.go", true, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if args["mode"] != "debug" {
			t.Errorf("expected mode 'debug', got: %v", args["mode"])
		}
		if args["program"] != "/path/to/main.go" {
			t.Errorf("expected program '/path/to/main.go', got: %v", args["program"])
		}
		if args["stopOnEntry"] != true {
			t.Errorf("expected stopOnEntry true, got: %v", args["stopOnEntry"])
		}
		if _, ok := args["args"]; ok {
			t.Error("expected no args key when programArgs is nil")
		}
	})

	t.Run("binary mode", func(t *testing.T) {
		args, err := backend.LaunchArgs("binary", "/path/to/binary", false, []string{"--flag", "value"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if args["mode"] != "exec" {
			t.Errorf("expected mode 'exec', got: %v", args["mode"])
		}
		if args["program"] != "/path/to/binary" {
			t.Errorf("expected program '/path/to/binary', got: %v", args["program"])
		}
		if args["stopOnEntry"] != false {
			t.Errorf("expected stopOnEntry false, got: %v", args["stopOnEntry"])
		}
		programArgs, ok := args["args"].([]string)
		if !ok {
			t.Fatalf("expected args to be []string, got: %T", args["args"])
		}
		if len(programArgs) != 2 || programArgs[0] != "--flag" || programArgs[1] != "value" {
			t.Errorf("unexpected args: %v", programArgs)
		}
	})

	t.Run("unsupported mode", func(t *testing.T) {
		_, err := backend.LaunchArgs("invalid", "/path", false, nil)
		if err == nil {
			t.Error("expected error for unsupported mode")
		}
	})
}

func TestDelveBackendCoreArgs(t *testing.T) {
	backend := &delveBackend{}
	args, err := backend.CoreArgs("/path/to/program", "/path/to/core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args["mode"] != "core" {
		t.Errorf("expected mode 'core', got: %v", args["mode"])
	}
	if args["program"] != "/path/to/program" {
		t.Errorf("expected program '/path/to/program', got: %v", args["program"])
	}
	if args["coreFilePath"] != "/path/to/core" {
		t.Errorf("expected coreFilePath '/path/to/core', got: %v", args["coreFilePath"])
	}
}

func TestDelveBackendAttachArgs(t *testing.T) {
	backend := &delveBackend{}
	args, err := backend.AttachArgs(12345)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args["mode"] != "local" {
		t.Errorf("expected mode 'local', got: %v", args["mode"])
	}
	if args["processId"] != 12345 {
		t.Errorf("expected processId 12345, got: %v", args["processId"])
	}
}
