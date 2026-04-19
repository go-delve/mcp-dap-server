//go:build integration

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	dlvHostPort   = "localhost:14000"
	socatHostPort = "localhost:14040"
	appHostPort   = "localhost:18080"
	// incrLine is the line inside testdata/integration/main.go where counter++ lives.
	// "counter++" is line 12 in that file (1-indexed).
	incrLine = 12
)

var (
	integrationBinaryPath string
	integrationComposeFile string
)

// TestMain builds the binary and starts the docker-compose stack once for all
// integration tests, then tears down after all tests complete.
func TestMain(m *testing.M) {
	_, thisFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(thisFile)
	integrationComposeFile = filepath.Join(moduleRoot, "testdata", "docker-compose.yml")

	// Build the mcp-dap-server binary.
	outPath := filepath.Join(os.TempDir(), "mcp-dap-server-integration-test")
	cmd := exec.Command("go", "build", "-o", outPath, ".")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: go build failed: %v\n", err)
		os.Exit(1)
	}
	integrationBinaryPath = outPath

	// Start the docker-compose stack.
	upCmd := exec.Command("docker", "compose", "-f", integrationComposeFile, "up", "-d", "--wait")
	upCmd.Stdout = os.Stderr
	upCmd.Stderr = os.Stderr
	if err := upCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: docker compose up failed: %v\n", err)
		os.Exit(1)
	}

	// Wait for services to be reachable on the host.
	if err := waitTCPOpenErr(socatHostPort, 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: socat-proxy not reachable: %v\n", err)
		dockerComposeDownQuiet()
		os.Exit(1)
	}
	if err := waitHTTPHealthyErr(fmt.Sprintf("http://%s/health", appHostPort), 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: app not healthy: %v\n", err)
		dockerComposeDownQuiet()
		os.Exit(1)
	}

	code := m.Run()

	dockerComposeDownQuiet()
	os.Exit(code)
}

func dockerComposeDownQuiet() {
	cmd := exec.Command("docker", "compose", "-f", integrationComposeFile, "down")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Run()
}

// mcpSession holds a running mcp-dap-server subprocess and its MCP client session.
type mcpSession struct {
	session *mcp.ClientSession
	cancel  context.CancelFunc
	ctx     context.Context
}

// callTool calls an MCP tool and returns (text, isError).
// Fatals on transport-level errors.
func (s *mcpSession) callTool(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := s.session.CallTool(s.ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("transport error calling %s: %v", name, err)
	}
	var text string
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, result.IsError
}

// spawnMCPServer spawns mcp-dap-server --connect <addr> and returns a connected session.
// Killed via t.Cleanup.
func spawnMCPServer(t *testing.T, connectAddr string) *mcpSession {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, integrationBinaryPath, "--connect", connectAddr)
	impl := mcp.Implementation{Name: "integration-test-client", Version: "0"}
	client := mcp.NewClient(&impl, nil)
	transport := mcp.NewCommandTransport(cmd)

	session, err := client.Connect(ctx, transport)
	if err != nil {
		cancel()
		t.Fatalf("failed to connect MCP client to mcp-dap-server: %v", err)
	}

	ms := &mcpSession{session: session, cancel: cancel, ctx: ctx}
	t.Cleanup(func() {
		session.Close()
		cancel()
	})
	return ms
}

// restartDelve restarts delve-with-app container to get a fresh dlv state.
// After restart, waits for dlv (port 14000) and app (HTTP) to be ready.
func restartDelve(t *testing.T) {
	t.Helper()
	restartComposeService(t, "delve-with-app")
	// socat depends_on delve-with-app, but docker compose restart doesn't re-evaluate
	// depends_on. Restart socat too to ensure it re-connects.
	restartComposeService(t, "socat-proxy")
	waitTCPOpen(t, dlvHostPort, 60*time.Second)
	waitTCPOpen(t, socatHostPort, 30*time.Second)
	waitHTTPHealthy(t, fmt.Sprintf("http://%s/health", appHostPort), 30*time.Second)
}

// waitTCPOpenErr polls until a TCP address accepts connections or timeout.
func waitTCPOpenErr(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}

// waitTCPOpen is the test-fatal version.
func waitTCPOpen(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	if err := waitTCPOpenErr(addr, timeout); err != nil {
		t.Fatalf("%v", err)
	}
}

// waitHTTPHealthyErr polls until the URL returns 200 or timeout.
func waitHTTPHealthyErr(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to be healthy", url)
}

// waitHTTPHealthy is the test-fatal version.
func waitHTTPHealthy(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	if err := waitHTTPHealthyErr(url, timeout); err != nil {
		t.Fatalf("%v", err)
	}
}

// curlIncr fires a GET to /incr. If stopped at a BP this will block.
func curlIncr() (*http.Response, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	return client.Get(fmt.Sprintf("http://%s/incr", appHostPort))
}

// incrFilePath returns the path to testdata/integration/main.go as seen by dlv
// inside the container (project root is mounted at /app).
func incrFilePath() string {
	return "/app/testdata/integration/main.go"
}

// restartComposeService restarts a named service in the compose stack.
func restartComposeService(t *testing.T, service string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", integrationComposeFile, "restart", service)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose restart %s failed: %v", service, err)
	}
}

// waitUntilRecovered forces a reconnect cycle (force=true to trigger stale) and then
// polls until healthy. Returns elapsed time from when force was issued.
func waitUntilRecovered(t *testing.T, ms *mcpSession, maxDuration time.Duration) time.Duration {
	t.Helper()

	// Force a redial to ensure the stale flag is set (in case the TCP stack
	// hasn't detected the drop yet via a passive read error).
	ms.callTool(t, "reconnect", map[string]any{"force": true, "wait_timeout_sec": 1})

	start := time.Now()
	deadline := start.Add(maxDuration)
	for time.Now().Before(deadline) {
		text, isErr := ms.callTool(t, "reconnect", map[string]any{
			"wait_timeout_sec": 5,
		})
		if !isErr && strings.Contains(text, "healthy") {
			return time.Since(start)
		}
		t.Logf("waitUntilRecovered: not yet (%s)", text)
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("did not recover within %s", maxDuration)
	return maxDuration
}

// debugAttach calls the 'debug' tool in remote-attach mode via socat.
func debugAttach(t *testing.T, ms *mcpSession) {
	t.Helper()
	text, isErr := ms.callTool(t, "debug", map[string]any{"mode": "remote-attach"})
	if isErr {
		t.Fatalf("debug tool returned error: %s", text)
	}
	t.Logf("debug attach: %s", text)
}

// setBreakpointOnIncr sets a source BP at the counter++ line.
func setBreakpointOnIncr(t *testing.T, ms *mcpSession) {
	t.Helper()
	text, isErr := ms.callTool(t, "breakpoint", map[string]any{
		"file": incrFilePath(),
		"line": incrLine,
	})
	if isErr {
		t.Fatalf("breakpoint returned error: %s", text)
	}
	t.Logf("set breakpoint: %s", text)
}

// pauseAndContinue pauses the running program, fires /incr concurrently, and
// waits for the BP to be hit. Returns the context text at the BP location.
// The program remains paused at the BP after this call — cleanup kills the session.
func pauseAndContinue(t *testing.T, ms *mcpSession) string {
	t.Helper()

	// Pause the running program so we have a stopped thread to resume from.
	pauseText, isErr := ms.callTool(t, "pause", map[string]any{})
	if isErr {
		t.Logf("pause returned error (may be OK if already stopped): %s", pauseText)
	} else {
		t.Logf("paused: %s", pauseText)
	}

	// Fire HTTP /incr in background — will hit the BP.
	incrDone := make(chan error, 1)
	go func() {
		resp, err := curlIncr()
		if err != nil {
			incrDone <- err
			return
		}
		resp.Body.Close()
		incrDone <- nil
	}()

	// Continue — blocks until BP is hit (StoppedEvent).
	contText, isErr := ms.callTool(t, "continue", map[string]any{})
	if isErr {
		t.Fatalf("continue returned error: %s", contText)
	}
	t.Logf("continue (BP hit): %s", contText)

	// Get context at the BP.
	ctxText, isErr := ms.callTool(t, "context", map[string]any{})
	if isErr {
		t.Fatalf("context returned error: %s", ctxText)
	}

	// The HTTP goroutine is blocked waiting for the response (program stopped at BP).
	// We don't release the BP here — test cleanup (cancel + session.Close) will
	// terminate the mcp-dap-server, which unblocks the HTTP goroutine.
	// Register a cleanup to drain the channel so the goroutine can exit.
	t.Cleanup(func() {
		select {
		case <-incrDone:
		case <-time.After(10 * time.Second):
		}
	})

	return ctxText
}

// TestIntegration_ConnectBackend_InitialAttach verifies that mcp-dap-server
// --connect to a running dlv --headless + socat proxy successfully performs
// DAP Initialize + Attach(remote) + ConfigurationDone.
func TestIntegration_ConnectBackend_InitialAttach(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)

	// Verify the session is live: context may error if not stopped, which is fine.
	ctxText, isErr := ms.callTool(t, "context", map[string]any{})
	if isErr {
		t.Logf("context after attach (acceptable if not stopped): %s", ctxText)
	} else {
		t.Logf("context: %s", ctxText)
	}
}

// TestIntegration_BreakpointAndContinue sets a BP on incr, triggers via curl,
// verifies BP hit in context.
func TestIntegration_BreakpointAndContinue(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)
	setBreakpointOnIncr(t, ms)

	ctxText := pauseAndContinue(t, ms)

	if !strings.Contains(ctxText, "main.incr") && !strings.Contains(ctxText, "counter") {
		t.Errorf("expected context to mention incr or counter, got: %s", ctxText)
	}
}

// TestIntegration_SocatDrop_AutoReconnect sets a BP, restarts socat-proxy,
// waits for recovery (≤5s after proxy is back), then verifies BP still works.
func TestIntegration_SocatDrop_AutoReconnect(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)
	setBreakpointOnIncr(t, ms)

	// Verify BP works initially.
	pauseAndContinue(t, ms)
	t.Log("initial BP trigger confirmed")

	// Drop the socat proxy (simulate TCP drop).
	restartComposeService(t, "socat-proxy")
	t.Log("socat-proxy restarted, waiting for reconnect...")

	waitTCPOpen(t, socatHostPort, 20*time.Second)
	elapsed := waitUntilRecovered(t, ms, 15*time.Second)
	t.Logf("recovered after socat drop in %s", elapsed)

	// Trigger again — BP should still work after reconnect + BP re-apply.
	ctxText := pauseAndContinue(t, ms)
	if !strings.Contains(ctxText, "main.incr") && !strings.Contains(ctxText, "counter") {
		t.Errorf("expected BP hit after reconnect, got context: %s", ctxText)
	}
}

// TestIntegration_MultipleDropsBackoffCap kills socat 3 times back-to-back and
// verifies the system recovers within a bounded time (backoff caps at 30s).
func TestIntegration_MultipleDropsBackoffCap(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)

	const drops = 3
	for i := 0; i < drops; i++ {
		restartComposeService(t, "socat-proxy")
		t.Logf("socat-proxy restart %d/%d", i+1, drops)
		time.Sleep(300 * time.Millisecond)
	}

	waitTCPOpen(t, socatHostPort, 30*time.Second)

	start := time.Now()
	waitUntilRecovered(t, ms, 90*time.Second)
	elapsed := time.Since(start)
	t.Logf("recovered after %d drops in %s", drops, elapsed)

	// With backoff cap of 30s, total recovery should be well under 90s.
	text, isErr := ms.callTool(t, "reconnect", map[string]any{})
	if isErr {
		t.Errorf("reconnect returned error after recovery: %s", text)
	}
	if !strings.Contains(text, "healthy") {
		t.Errorf("expected healthy status, got: %s", text)
	}
}

// TestIntegration_PodRestart_BreakpointsPreserved restarts delve-with-app,
// waits for recovery, then verifies the BP fires in the new process.
func TestIntegration_PodRestart_BreakpointsPreserved(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)
	setBreakpointOnIncr(t, ms)

	// Confirm BP works on first instance.
	pauseAndContinue(t, ms)
	t.Log("pre-restart BP confirmed")

	// Restart the full container (simulates pod restart).
	restartComposeService(t, "delve-with-app")
	restartComposeService(t, "socat-proxy")
	t.Log("delve-with-app restarted, waiting for recovery...")

	waitTCPOpen(t, dlvHostPort, 60*time.Second)
	waitTCPOpen(t, socatHostPort, 30*time.Second)
	waitHTTPHealthy(t, fmt.Sprintf("http://%s/health", appHostPort), 30*time.Second)

	waitUntilRecovered(t, ms, 30*time.Second)
	t.Log("auto-reconnected after pod restart")

	// BP should have been re-applied to the new dlv instance.
	ctxText := pauseAndContinue(t, ms)
	if !strings.Contains(ctxText, "main.incr") && !strings.Contains(ctxText, "counter") {
		t.Errorf("expected BP hit on new instance, got: %s", ctxText)
	}
}

// TestIntegration_RecoverWithin15s measures recovery time from socat drop and
// asserts it is below the NFR-1 bound of 15s.
func TestIntegration_RecoverWithin15s(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)

	// Drop the proxy.
	restartComposeService(t, "socat-proxy")
	t.Log("socat-proxy dropped, measuring recovery time...")

	waitTCPOpen(t, socatHostPort, 30*time.Second)

	elapsed := waitUntilRecovered(t, ms, 30*time.Second)
	t.Logf("recovery time: %s", elapsed)

	const nfr1 = 15 * time.Second
	if elapsed > nfr1 {
		t.Errorf("NFR-1 violated: recovery took %s, must be <%s", elapsed, nfr1)
	}
}

// TestIntegration_DlvOldVersion_AttachRemoteFails is skipped unless
// DLV_OLD_VERSION=v1.7.2 is set — building a separate old-dlv image is
// out of scope for standard CI.
func TestIntegration_DlvOldVersion_AttachRemoteFails(t *testing.T) {
	if os.Getenv("DLV_OLD_VERSION") == "" {
		t.Skip("requires DLV_OLD_VERSION=v1.7.2 to be set; skipping old-version compatibility test")
	}
	t.Fatal("old-version test body not implemented: set up old-dlv container first")
}

// TestIntegration_GracefulShutdown_LeavesDebuggeeAlive closes mcp-dap-server
// (simulating Claude Code quit) and verifies dlv + debuggee survive.
func TestIntegration_GracefulShutdown_LeavesDebuggeeAlive(t *testing.T) {
	restartDelve(t)

	ms := spawnMCPServer(t, socatHostPort)
	debugAttach(t, ms)

	// Gracefully close the MCP client session (closes stdin of mcp-dap-server).
	if err := ms.session.Close(); err != nil {
		t.Logf("session close: %v (non-fatal)", err)
	}
	ms.cancel()

	// Allow server to exit cleanly.
	time.Sleep(2 * time.Second)

	// Verify dlv process is still running in the container.
	checkCmd := exec.Command("docker", "compose", "-f", integrationComposeFile,
		"exec", "-T", "delve-with-app",
		"sh", "-c", "pgrep dlv && echo dlv_alive")
	out, err := checkCmd.CombinedOutput()
	if err != nil {
		t.Errorf("dlv not alive after shutdown: %v, output: %s", err, out)
	} else {
		t.Logf("dlv alive: %s", strings.TrimSpace(string(out)))
	}

	// Verify the debuggee (example) is still running.
	checkCmd2 := exec.Command("docker", "compose", "-f", integrationComposeFile,
		"exec", "-T", "delve-with-app",
		"sh", "-c", "pgrep example && echo app_alive")
	out2, err2 := checkCmd2.CombinedOutput()
	if err2 != nil {
		t.Errorf("example process not alive after shutdown: %v, output: %s", err2, out2)
	} else {
		t.Logf("example alive: %s", strings.TrimSpace(string(out2)))
	}
}
