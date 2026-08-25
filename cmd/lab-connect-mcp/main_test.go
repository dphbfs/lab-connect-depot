package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolName_SanitizedAndDisambiguated(t *testing.T) {
	name := toolName(peerInfo{Name: "my proxmox host!", NodeKey: "nodekey0123456789"})
	if name != "run-command-my-proxmox-host--23456789" {
		t.Fatalf("toolName() = %q, want a sanitized name with an 8-char NodeKey suffix", name)
	}
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if !valid {
			t.Fatalf("toolName() contains invalid MCP tool-name rune %q", r)
		}
	}
}

// fakeRunner is a minimal stand-in for lab-connect's Runner control API,
// serving /peers and /run over a Unix socket exactly like the real
// control.Server does — this package intentionally doesn't import
// lab-connect's internal packages (see main.go's doc comment), so its
// tests fake the wire protocol instead of standing up a real Runner.
type fakeRunner struct {
	mu    sync.Mutex
	peers []peerInfo

	sockPath string
	srv      *http.Server
}

func newFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	f := &fakeRunner{sockPath: filepath.Join(t.TempDir(), "control.sock")}

	mux := http.NewServeMux()
	mux.HandleFunc("/peers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		peers := f.peers
		f.mu.Unlock()
		if peers == nil {
			peers = []peerInfo{}
		}
		json.NewEncoder(w).Encode(peers)
	})
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		var req runRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(runResponse{Output: []byte("ok:" + req.ClientSessionID), ExitCode: 0})
	})

	ln, err := net.Listen("unix", f.sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })

	return f
}

func (f *fakeRunner) setPeers(peers []peerInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = peers
}

func TestMCPServer_ToolListReflectsLivePairing(t *testing.T) {
	const peerNodeKey = "peer-key-mcp-1"
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}

	server := mcp.NewServer(&mcp.Implementation{Name: "lab-connect-mcp", Version: "test"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go refreshTools(ctx, server, client)

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect() error: %v", err)
	}
	cs, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect() error: %v", err)
	}
	defer cs.Close()

	// Not yet Paired: the tool list must not mention this Node at all.
	if got := listToolNames(t, ctx, cs); len(got) != 0 {
		t.Fatalf("tool list before pairing = %v, want empty (unpaired Node must never be listed)", got)
	}

	runner.setPeers([]peerInfo{{NodeKey: peerNodeKey, Name: "peer-b", Online: true, PairingState: statePaired}})
	waitForToolCount(t, ctx, cs, 1)

	names := listToolNames(t, ctx, cs)
	if len(names) != 1 {
		t.Fatalf("tool list after pairing = %v, want exactly 1 tool", names)
	}
	toolName := names[0]

	// Call it end to end through the real MCP protocol.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: map[string]any{"argv": []string{"echo", "hello"}},
	})
	if err != nil {
		t.Fatalf("CallTool() error: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() result is an error: %+v", res.Content)
	}

	runner.setPeers([]peerInfo{{NodeKey: peerNodeKey, Name: "peer-b", Online: true, PairingState: "unpaired"}})
	waitForToolCount(t, ctx, cs, 0)
}

// TestMCPServer_DistinctSessionIDsPerConnection verifies the core claim
// behind this binary's SSE/HTTP transport: one *mcp.Server instance
// shared across many concurrent connections (mcp.NewStreamableHTTPHandler,
// exactly as run() wires it in non-stdio mode) gives each connection its
// own session id via req.Session.ID(), with no per-process id minted by
// this package. addRunCommandTool passes req.Session.ID() straight
// through to controlClient.Run as the Audit Entry's Client Session id, so
// distinct ids here is exactly what makes distinct Audit Entries possible
// for concurrent callers. (req.Session.ID() is only populated by
// transports implementing hasSessionID — the in-memory transport used
// above does not, so this test must go over real HTTP.)
func TestMCPServer_DistinctSessionIDsPerConnection(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "lab-connect-mcp", Version: "test"}, nil)

	seen := make(chan string, 2)
	mcp.AddTool(server, &mcp.Tool{Name: "probe"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		seen <- req.Session.ID()
		return &mcp.CallToolResult{}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connect := func() *mcp.ClientSession {
		t.Helper()
		mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
		cs, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL}, nil)
		if err != nil {
			t.Fatalf("client.Connect() error: %v", err)
		}
		return cs
	}

	cs1 := connect()
	defer cs1.Close()
	cs2 := connect()
	defer cs2.Close()

	if _, err := cs1.CallTool(ctx, &mcp.CallToolParams{Name: "probe"}); err != nil {
		t.Fatalf("CallTool() on cs1 error: %v", err)
	}
	if _, err := cs2.CallTool(ctx, &mcp.CallToolParams{Name: "probe"}); err != nil {
		t.Fatalf("CallTool() on cs2 error: %v", err)
	}

	id1, id2 := <-seen, <-seen
	if id1 == "" || id2 == "" {
		t.Fatalf("session ids must not be empty: got %q, %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("two distinct connections got the same session id %q, want distinct", id1)
	}
}

func listToolNames(t *testing.T, ctx context.Context, cs *mcp.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("Tools() error: %v", err)
		}
		names = append(names, tool.Name)
	}
	return names
}

func waitForToolCount(t *testing.T, ctx context.Context, cs *mcp.ClientSession, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := listToolNames(t, ctx, cs); len(got) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("tool count never reached %d within 10s (last: %v)", want, listToolNames(t, ctx, cs))
}
