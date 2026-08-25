package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeRunner is a minimal stand-in for lab-connect's Runner control API,
// serving /peers and /run over a Unix socket exactly like the real
// control.Server does — this package intentionally doesn't import
// lab-connect's internal packages (see main.go's doc comment), so its
// tests fake the wire protocol instead of standing up a real Runner.
//
// /run additionally interprets the specific argv shapes doUpload/
// doDownload construct (sh -c with $0/$1 positional args) against an
// in-memory fake filesystem, and echoes a canned "ok:<session>" result
// for anything else — enough to exercise "execute" and "transport" end
// to end without a real shell or Runner.
type fakeRunner struct {
	mu    sync.Mutex
	peers []peerInfo
	files map[string][]byte

	sockPath string
	srv      *http.Server
}

func newFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	f := &fakeRunner{sockPath: filepath.Join(t.TempDir(), "control.sock"), files: map[string][]byte{}}

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
		out, exit := f.exec(req.Argv)
		json.NewEncoder(w).Encode(runResponse{Output: out, ExitCode: exit})
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

// exec fakes just enough of a shell to support doUpload/doDownload's argv
// shapes (`sh -c SCRIPT $0 $1`) plus a generic echo for anything else.
func (f *fakeRunner) exec(argv []string) ([]byte, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(argv) == 5 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "base64 -d") {
		content, path := argv[3], argv[4]
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return []byte(err.Error()), 1
		}
		f.files[path] = decoded
		return nil, 0
	}
	if len(argv) == 4 && argv[0] == "sh" && argv[1] == "-c" && argv[2] == `base64 "$0"` {
		path := argv[3]
		content, ok := f.files[path]
		if !ok {
			return []byte("no such file: " + path), 1
		}
		return []byte(base64.StdEncoding.EncodeToString(content)), 0
	}
	return []byte("ok"), 0
}

func TestMachinesTool_ListsOnlyPairedNodes(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{
		{NodeKey: "a", Name: "paired-a", Online: true, PairingState: statePaired},
		{NodeKey: "b", Name: "pending-b", Online: true, PairingState: "pending"},
	})

	cs := connectTestClient(t, client)
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "machines"})
	if err != nil {
		t.Fatalf("CallTool(machines) error: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "paired-a") {
		t.Fatalf("machines output = %q, want it to mention the paired Node", text)
	}
	if strings.Contains(text, "pending-b") {
		t.Fatalf("machines output = %q, must not mention a non-paired Node", text)
	}
}

func TestExecuteTool_RejectsUnpairedMachine(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: "unpaired"}})

	cs := connectTestClient(t, client)
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"machine": "sandbox-ubuntu", "argv": []string{"echo", "hi"}},
	})
	if err != nil {
		t.Fatalf("CallTool(execute) error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("execute against an unpaired machine should be an error result, got: %+v", res.Content)
	}
}

func TestExecuteTool_RunsOnPairedMachine(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs := connectTestClient(t, client)
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"machine": "sandbox-ubuntu", "argv": []string{"echo", "hi"}},
	})
	if err != nil {
		t.Fatalf("CallTool(execute) error: %v", err)
	}
	if res.IsError {
		t.Fatalf("execute against a paired machine should succeed, got error: %+v", res.Content)
	}
}

func TestTransportTool_UploadThenDownloadRoundTrips(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs := connectTestClient(t, client)
	defer cs.Close()
	ctx := context.Background()

	content := "hello from transport test"
	b64 := base64.StdEncoding.EncodeToString([]byte(content))

	uploadRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "transport",
		Arguments: map[string]any{
			"machine": "sandbox-ubuntu", "direction": "upload",
			"path": "/tmp/greeting.txt", "content": b64,
		},
	})
	if err != nil {
		t.Fatalf("CallTool(transport upload) error: %v", err)
	}
	if uploadRes.IsError {
		t.Fatalf("upload result is an error: %+v", uploadRes.Content)
	}

	downloadRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "transport",
		Arguments: map[string]any{
			"machine": "sandbox-ubuntu", "direction": "download",
			"path": "/tmp/greeting.txt",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(transport download) error: %v", err)
	}
	if downloadRes.IsError {
		t.Fatalf("download result is an error: %+v", downloadRes.Content)
	}
	gotB64 := toolResultText(t, downloadRes)
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(gotB64))
	if err != nil {
		t.Fatalf("download result isn't valid base64: %v (%q)", err, gotB64)
	}
	if string(got) != content {
		t.Fatalf("round-tripped content = %q, want %q", got, content)
	}
}

func TestTransportTool_DownloadMissingFileIsError(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs := connectTestClient(t, client)
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "transport",
		Arguments: map[string]any{
			"machine": "sandbox-ubuntu", "direction": "download",
			"path": "/tmp/does-not-exist.txt",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(transport download) error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("downloading a nonexistent file should be an error result, got: %+v", res.Content)
	}
}

func TestTransportTool_RejectsBadDirection(t *testing.T) {
	runner := newFakeRunner(t)
	client := &controlClient{socketPath: runner.sockPath}
	runner.setPeers([]peerInfo{{NodeKey: "a", Name: "sandbox-ubuntu", Online: true, PairingState: statePaired}})

	cs := connectTestClient(t, client)
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "transport",
		Arguments: map[string]any{
			"machine": "sandbox-ubuntu", "direction": "sideways", "path": "/tmp/x",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(transport) error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("an invalid direction should be an error result, got: %+v", res.Content)
	}
}

// TestMCPServer_DistinctSessionIDsPerConnection verifies the core claim
// behind this binary's SSE/HTTP transport: one *mcp.Server instance
// shared across many concurrent connections (mcp.NewStreamableHTTPHandler,
// exactly as run() wires it in non-stdio mode) gives each connection its
// own session id via req.Session.ID(), with no per-process id minted by
// this package. execute/transport pass req.Session.ID() straight through
// to controlClient.Run as the Audit Entry's Client Session id, so
// distinct ids here is exactly what makes distinct Audit Entries possible
// for concurrent callers. (req.Session.ID() is only populated by
// transports implementing hasSessionID — the in-memory transport used
// elsewhere in this file does not, so this test must go over real HTTP.)
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

// connectTestClient builds the real server (machines/execute/transport
// registered exactly as run() does) and connects an in-memory client
// session to it.
func connectTestClient(t *testing.T, client *controlClient) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "lab-connect-mcp", Version: "test"}, &mcp.ServerOptions{HasTools: true})
	addMachinesTool(server, client)
	addExecuteTool(server, client)
	addTransportTool(server, client)

	ctx := context.Background()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect() error: %v", err)
	}
	cs, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect() error: %v", err)
	}
	return cs
}

func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
