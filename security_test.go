package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecurityPorts(t *testing.T) {
	for input, want := range map[string]string{"0": "127.0.0.1:0", ":0": "127.0.0.1:0", ":65535": "127.0.0.1:65535", "00080": "127.0.0.1:80"} {
		got, err := delveListenAddress(input)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", ":", "-1", "+1", "65536", " 80", "80\n", "localhost:80", "0.0.0.0:80", "::80", "http"} {
		if _, err := delveListenAddress(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestSecurityPrivateLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	f, err := openPrivateLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("preserve"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("insecure permissions: %v %v", info, err)
	}
	if f, err := openPrivateLog(path); err == nil {
		f.Close()
		t.Fatal("existing file accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openPrivateLog(link); err == nil {
		f.Close()
		t.Fatal("symlink accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "preserve" {
		t.Fatalf("target changed: %q %v", got, err)
	}
	a, err := openPrivateLog("")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(a.Name())
	defer a.Close()
	b, err := openPrivateLog("")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(b.Name())
	defer b.Close()
	if a.Name() == b.Name() {
		t.Fatal("nonunique logs")
	}
}

func TestSecurityBoundedLog(t *testing.T) {
	var output bytes.Buffer
	w := newBoundedLogWriter(&output)
	p := bytes.Repeat([]byte("x"), maxLogBytes+100)
	for range 2 {
		if n, err := w.Write(p); n != len(p) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if output.Len() != maxLogBytes {
		t.Fatalf("log length = %d", output.Len())
	}
}

func TestSecurityGDBLogPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"with space", "line\nbreak", "semi;colon", `quote"`, "back\\slash"} {
		if err := prepareGDBLog(filepath.Join(dir, name)); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	path := filepath.Join(dir, "safe.log")
	if err := prepareGDBLog(path); err != nil {
		t.Fatal(err)
	}
	if err := prepareGDBLog(path); err == nil {
		t.Fatal("accepted existing log")
	}
	link := filepath.Join(dir, "symlink.log")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareGDBLog(link); err == nil {
		t.Fatal("accepted symlink log")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("insecure log permissions")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := prepareGDBLog(filepath.Join(dir, "other.log")); err == nil {
		t.Fatal("accepted nonprivate parent")
	}
}

// Re-exec the test executable instead of depending on a shell or debugger.
func TestSecurityDelveChild(t *testing.T) {
	switch os.Getenv("MCP_DAP_TEST_CHILD") {
	case "silent":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "flood":
		fmt.Println("DAP server listening at: 127.0.0.1:12345")
		_, _ = io.CopyN(os.Stdout, strings.NewReader(strings.Repeat("x", 2<<20)), 2<<20)
		os.Exit(0)
	case "bad":
		fmt.Println("DAP server listening at: 0.0.0.0:12345")
		os.Exit(0)
	case "eof":
		os.Exit(0)
	case "oversized":
		fmt.Println(strings.Repeat("x", 128<<10))
		os.Exit(0)
	}
}

func TestSecurityDelveStartup(t *testing.T) {
	for _, mode := range []string{"silent", "flood", "bad", "eof", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestSecurityDelveChild$")
			cmd.Env = append(os.Environ(), "MCP_DAP_TEST_CHILD="+mode)
			start := time.Now()
			timeout := 5 * time.Second
			if mode == "silent" {
				timeout = 100 * time.Millisecond
			}
			got, addr, err := startDelve(cmd, io.Discard, timeout)
			if mode != "flood" {
				if err == nil || got != nil || cmd.ProcessState == nil {
					t.Fatalf("failure not cleaned up: %v, %v", got, err)
				}
				if time.Since(start) > 10*time.Second {
					t.Fatal("startup not bounded")
				}
				return
			}
			if err != nil || addr != "127.0.0.1:12345" {
				t.Fatalf("startup = %q, %v", addr, err)
			}
			done := make(chan error, 1)
			go func() { done <- got.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				got.Process.Kill()
				<-done
				t.Fatal("stdout was not drained")
			}
		})
	}
}
