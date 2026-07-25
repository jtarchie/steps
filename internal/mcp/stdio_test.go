package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
)

// stdioServerMarker, when present in os.Args, tells TestMain to re-exec this
// test binary as a stdio MCP server instead of running tests.
const stdioServerMarker = "-steps-mcp-stdio-test-server"

// TestMain re-invokes this test binary as a stdio MCP server when
// stdioServerMarker is present, and exits WITHOUT calling m.Run() — the
// testing framework must never write to stdout in that mode, since stdout
// IS the newline-delimited JSON transport a stdio client reads. The marker
// is an argv element, not an env var, because commandTransport assigns
// cmd.Env = shell.HostEnv(), which filters out any STEPS_*-style env
// marker; only argv survives that filter unmodified.
func TestMain(m *testing.M) {
	if slices.Contains(os.Args[1:], stdioServerMarker) {
		_ = echoServer().Run(context.Background(), &sdkmcp.StdioTransport{})
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// stdioEchoServer returns an MCPServer that, when connected to, re-execs
// this test binary as a stdio MCP server (see TestMain) exposing the same
// "echo" tool the HTTP tests in client_test.go use.
func stdioEchoServer(t *testing.T) config.MCPServer {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	return config.MCPServer{Name: "echo-stdio", Command: exe, Args: []string{stdioServerMarker}}
}

func TestConnectStdio(t *testing.T) {
	t.Parallel()

	client, err := Connect(context.Background(), stdioEchoServer(t))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, want one tool named echo", tools)
	}

	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if result.IsError {
		t.Fatalf("CallTool result is an error: %+v", result)
	}

	sc, ok := result.StructuredContent.(map[string]any)
	if !ok || sc["text"] != "hi" {
		t.Fatalf("StructuredContent = %+v, want {text: hi}", result.StructuredContent)
	}
}

func TestConnectStdioMissingBinary(t *testing.T) {
	t.Parallel()

	srv := config.MCPServer{Name: "ghost", Command: "steps-test-no-such-binary-xyz"}

	_, err := Connect(context.Background(), srv)
	if err == nil {
		t.Fatal("Connect: expected an error for a nonexistent command")
	}
}

func TestCommandTransportClosesReapsProcess(t *testing.T) {
	t.Parallel()

	transport := commandTransport(context.Background(), stdioEchoServer(t))

	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatalf("transport.Connect: %v", err)
	}

	err = conn.Close()
	if err != nil {
		t.Fatalf("conn.Close: %v", err)
	}

	if transport.Command.ProcessState == nil || !transport.Command.ProcessState.Exited() {
		t.Error("expected the subprocess to be reaped after Close")
	}
}

func TestCommandTransportArgvAndEnv(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	t.Setenv("STEPS_TEST_SECRET", "s3cr3t")

	dir := t.TempDir()
	srv := config.MCPServer{Name: "gopls", Command: "gopls", Args: []string{"a", "b"}, Cwd: dir}

	transport := commandTransport(context.Background(), srv)
	cmd := transport.Command

	if len(cmd.Args) != 3 || cmd.Args[1] != "a" || cmd.Args[2] != "b" {
		t.Errorf("cmd.Args = %v, want [<path> a b]", cmd.Args)
	}

	if cmd.Dir != dir {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, dir)
	}

	hasPath := false

	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "STEPS_TEST_SECRET=") {
			t.Errorf("cmd.Env leaked a non-allowlisted variable: %q", kv)
		}

		if strings.HasPrefix(kv, "PATH=") {
			hasPath = true
		}
	}

	if !hasPath {
		t.Error("cmd.Env should contain PATH")
	}
}

func TestCommandTransportUnsetCwdInherits(t *testing.T) {
	t.Parallel()

	srv := config.MCPServer{Name: "gopls", Command: "gopls", Args: []string{"mcp"}}
	transport := commandTransport(context.Background(), srv)

	if transport.Command.Dir != "" {
		t.Errorf("cmd.Dir = %q, want empty (inherit steps' own cwd)", transport.Command.Dir)
	}
}

func TestStderrLoggerLogsLinesAtDebug(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	t.Cleanup(func() { slog.SetDefault(prev) })

	w := &stderrLogger{server: "gopls"}

	_, err := w.Write([]byte("boom\npartial"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte("boom")) {
		t.Errorf("expected the complete line to be logged, got: %s", buf.String())
	}

	if bytes.Contains(buf.Bytes(), []byte("partial")) {
		t.Errorf("expected the trailing partial line to stay buffered, got: %s", buf.String())
	}

	_, err = w.Write([]byte(" line\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte("partial line")) {
		t.Errorf("expected the completed partial line to be logged, got: %s", buf.String())
	}
}

func TestStderrLoggerSilentAboveDebug(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	t.Cleanup(func() { slog.SetDefault(prev) })

	w := &stderrLogger{server: "gopls"}

	_, err := w.Write([]byte("should not appear\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("expected no output at info level, got: %s", buf.String())
	}
}
