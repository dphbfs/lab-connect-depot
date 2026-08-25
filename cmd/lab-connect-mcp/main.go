// Command lab-connect-mcp is the MCP server for lab-connect's Gateway
// image: it queries the local Runner's Peer list + Pairing state over the
// control API and exposes a fixed three-tool surface —  "machines",
// "execute", "transport" — that only ever act on currently-Paired Nodes.
//
// This binary lives in lab-connect-depot, not lab-connect, on purpose:
// lab-connect's own release pipeline (scripts/install.sh,
// .github/workflows/release.yml) only ever builds and ships the Runner
// binary (cmd/lab-connect). The Gateway image installs that Runner via
// install.sh and builds this MCP server from source here instead.
//
// controlClient below intentionally duplicates lab-connect's own
// internal/control wire format (PeerInfo/runRequest/runResponse JSON
// shapes, the /peers and /run routes) rather than importing it — Go's
// internal/ packages aren't importable across repos/modules, and this
// project's call is to keep a small duplicated client here rather than
// take on a cross-repo Go dependency. If the two ever drift, lab-connect's
// internal/control is the source of truth to re-sync against.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// statePaired mirrors lab-connect's internal/pairing.StatePaired — the
// only Pairing state string this binary needs to recognize.
const statePaired = "paired"

// maxTransportBytes bounds "transport"'s upload/download size. There's no
// dedicated file-transfer RPC on the Runner side (internal/rpc is
// argv-exec only, no stdin, no streaming channel) — transport instead
// base64s the content and hands it to the existing run-command RPC as a
// plain argv element to `sh -c`. That argv element has to fit under the
// OS's real exec argument-size limit (ARG_MAX, ~2MB on Linux, smaller on
// some platforms), so this is a conservative cap well under that,
// accounting for base64's ~33% size inflation.
const maxTransportBytes = 1 << 20 // 1MiB, ~1.4MB once base64-encoded

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("LAB_CONNECT_MCP_ADDR", ":8091"), "address to serve MCP over SSE/HTTP on")
	stdio := flag.Bool("stdio", false, "serve MCP over stdio instead of SSE/HTTP (local/dev usage)")
	flag.Parse()

	sockPath, err := controlSocketPath()
	if err != nil {
		return err
	}
	client := &controlClient{socketPath: sockPath}

	// HasTools: true forces the "tools" capability into every session's
	// initialize response, matching a static tool set that's always
	// present regardless of current Peer/Pairing state — see
	// addMachinesTool/addExecuteTool/addTransportTool below, none of
	// which are added or removed at runtime.
	server := mcp.NewServer(&mcp.Implementation{Name: "lab-connect-mcp", Version: "v1"}, &mcp.ServerOptions{HasTools: true})
	addMachinesTool(server, client)
	addExecuteTool(server, client)
	addTransportTool(server, client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *stdio {
		return server.Run(ctx, &mcp.StdioTransport{})
	}

	// Many concurrent AI-agent clients now hit this one process, so each
	// gets its own MCP session id for free from the SDK (req.Session.ID())
	// rather than a single id minted for the process lifetime.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return http.ListenAndServe(*addr, handler)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pairedMachine looks up name among the Runner's currently-Paired Peers.
// Every tool call re-resolves the name fresh against the live Peer list
// rather than caching it — a Pairing that's revoked between calls (or
// never existed) must never be actionable, matching CONTEXT.md's
// "checked per-call, not cached" requirement.
func pairedMachine(ctx context.Context, client *controlClient, name string) (peerInfo, error) {
	peers, err := client.Peers(ctx)
	if err != nil {
		return peerInfo{}, fmt.Errorf("list machines: %w", err)
	}
	for _, p := range peers {
		if p.Name == name && p.PairingState == statePaired {
			return p, nil
		}
	}
	return peerInfo{}, fmt.Errorf("no paired machine named %q (call \"machines\" for the current list)", name)
}

func errorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// --- machines ---------------------------------------------------------

type machinesInput struct{}

func addMachinesTool(server *mcp.Server, client *controlClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "machines",
		Description: "List the lab-connect machines currently available to \"execute\" and \"transport\" — " +
			"i.e. Paired Nodes only. A machine not listed here isn't reachable by those tools, even if it exists " +
			"on the Overlay Network (unpaired Nodes are never actionable).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in machinesInput) (*mcp.CallToolResult, any, error) {
		peers, err := client.Peers(ctx)
		if err != nil {
			return errorResult("%s", err), nil, nil
		}
		var lines []string
		for _, p := range peers {
			if p.PairingState != statePaired {
				continue
			}
			status := "offline"
			if p.Online {
				status = "online"
			}
			lines = append(lines, fmt.Sprintf("%s (%s)", p.Name, status))
		}
		if len(lines) == 0 {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no paired machines"}}}, nil, nil
		}
		text := ""
		for _, l := range lines {
			text += l + "\n"
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

// --- execute ------------------------------------------------------------

type executeInput struct {
	Machine string   `json:"machine" jsonschema:"name of a paired machine, as returned by the \"machines\" tool"`
	Argv    []string `json:"argv" jsonschema:"the command and its arguments as an argv-style array — e.g. [\"docker\",\"ps\"] — never a single shell string"`
}

func addExecuteTool(server *mcp.Server, client *controlClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "execute",
		Description: "Run a command on a paired lab-connect machine (see \"machines\" for the current list). " +
			"v1 access is UNSCOPED FULL-SHELL: there is no allow-list and no command-class restriction — " +
			"the command runs with whatever privileges the Runner process has on that machine. " +
			"Only call this against a machine you would hand full shell access to today. " +
			"Every call is recorded as an Audit Entry on the target Node.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in executeInput) (*mcp.CallToolResult, any, error) {
		if len(in.Argv) == 0 {
			return errorResult("argv must not be empty"), nil, nil
		}
		peer, err := pairedMachine(ctx, client, in.Machine)
		if err != nil {
			return errorResult("%s", err), nil, nil
		}
		result, err := client.Run(ctx, peer.NodeKey, req.Session.ID(), in.Argv)
		if err != nil {
			return errorResult("%s", err), nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("exit status %d\n%s", result.ExitCode, string(result.Output)),
			}},
		}, nil, nil
	})
}

// --- transport ----------------------------------------------------------

type transportInput struct {
	Machine   string `json:"machine" jsonschema:"name of a paired machine, as returned by the \"machines\" tool"`
	Direction string `json:"direction" jsonschema:"\"upload\" to write content onto the machine, or \"download\" to read a file from it"`
	Path      string `json:"path" jsonschema:"absolute path of the file on the machine"`
	Content   string `json:"content,omitempty" jsonschema:"base64-encoded file content — required for \"upload\", ignored for \"download\""`
}

// addTransportTool moves file content onto or off of a paired machine.
// There's no dedicated file-transfer RPC on the Runner (internal/rpc is
// argv-exec only) — this reuses the existing run-command RPC, passing
// base64 content as a plain argv element to `sh -c` rather than
// interpolating it into a shell string (avoids injection: the content
// lands in $0, never parsed as script syntax). See maxTransportBytes for
// the resulting size cap.
func addTransportTool(server *mcp.Server, client *controlClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "transport",
		Description: fmt.Sprintf(
			"Copy a file onto or off of a paired lab-connect machine (see \"machines\" for the current list). "+
				"\"upload\" writes base64-encoded content to a path on the machine; \"download\" reads a path from "+
				"the machine and returns its content base64-encoded. Limited to files up to ~%d bytes "+
				"(no streaming transfer — content travels as a single command argument). "+
				"Every call is recorded as an Audit Entry on the target Node, same as \"execute\".",
			maxTransportBytes,
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in transportInput) (*mcp.CallToolResult, any, error) {
		if in.Path == "" {
			return errorResult("path must not be empty"), nil, nil
		}
		peer, err := pairedMachine(ctx, client, in.Machine)
		if err != nil {
			return errorResult("%s", err), nil, nil
		}

		switch in.Direction {
		case "upload":
			return doUpload(ctx, client, req.Session.ID(), peer, in)
		case "download":
			return doDownload(ctx, client, req.Session.ID(), peer, in)
		default:
			return errorResult("direction must be \"upload\" or \"download\", got %q", in.Direction), nil, nil
		}
	})
}

func doUpload(ctx context.Context, client *controlClient, sessionID string, peer peerInfo, in transportInput) (*mcp.CallToolResult, any, error) {
	if in.Content == "" {
		return errorResult("content (base64) is required for upload"), nil, nil
	}
	if len(in.Content) > (maxTransportBytes*4)/3+4 {
		return errorResult("content exceeds the ~%d byte transport limit", maxTransportBytes), nil, nil
	}
	if _, err := base64.StdEncoding.DecodeString(in.Content); err != nil {
		return errorResult("content is not valid base64: %s", err), nil, nil
	}

	// sh -c SCRIPT ARG0 ARG1 sets $0=ARG0, $1=ARG1 inside SCRIPT — the
	// base64 content and path are never concatenated into the script
	// string itself, so this is safe regardless of what either contains.
	argv := []string{"sh", "-c", `printf '%s' "$0" | base64 -d > "$1"`, in.Content, in.Path}
	result, err := client.Run(ctx, peer.NodeKey, sessionID, argv)
	if err != nil {
		return errorResult("%s", err), nil, nil
	}
	if result.ExitCode != 0 {
		return errorResult("upload to %s failed (exit status %d): %s", in.Path, result.ExitCode, string(result.Output)), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("uploaded to %s:%s", peer.Name, in.Path)}},
	}, nil, nil
}

func doDownload(ctx context.Context, client *controlClient, sessionID string, peer peerInfo, in transportInput) (*mcp.CallToolResult, any, error) {
	argv := []string{"sh", "-c", `base64 "$0"`, in.Path}
	result, err := client.Run(ctx, peer.NodeKey, sessionID, argv)
	if err != nil {
		return errorResult("%s", err), nil, nil
	}
	if result.ExitCode != 0 {
		return errorResult("download from %s failed (exit status %d): %s", in.Path, result.ExitCode, string(result.Output)), nil, nil
	}
	if len(result.Output) > (maxTransportBytes*4)/3+4 {
		return errorResult("file exceeds the ~%d byte transport limit", maxTransportBytes), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(result.Output)}},
	}, nil, nil
}

// controlSocketPath mirrors lab-connect's internal/config.Dir(): a single
// process expected to run alongside the Runner it talks to, sharing the
// same LAB_CONNECT_CONFIG_DIR convention.
func controlSocketPath() (string, error) {
	if d := os.Getenv("LAB_CONNECT_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "control.sock"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "lab-connect", "control.sock"), nil
}

// --- controlClient: a minimal duplicate of lab-connect's internal/control
// Client, covering only the two routes this binary needs (/peers, /run).

// peerInfo mirrors internal/control.PeerInfo's JSON shape.
type peerInfo struct {
	NodeKey      string   `json:"node_key"`
	Name         string   `json:"name"`
	Online       bool     `json:"online"`
	IPAddresses  []string `json:"ip_addresses"`
	PairingState string   `json:"pairing_state"`
}

// runRequest mirrors internal/control's /run POST body.
type runRequest struct {
	PeerNodeKey     string   `json:"peer_node_key"`
	ClientSessionID string   `json:"client_session_id"`
	Argv            []string `json:"argv"`
}

// runResponse mirrors internal/control's /run response body on success.
type runResponse struct {
	Output   []byte `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// runResult is the outcome of a Run call.
type runResult struct {
	Output   []byte
	ExitCode int
}

// controlClient talks to a running Runner's control API over its Unix
// domain socket.
type controlClient struct {
	socketPath string
}

func (c *controlClient) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", c.socketPath)
			},
		},
	}
}

func (c *controlClient) Peers(ctx context.Context) ([]peerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/peers", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request peers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("peers (status %d): %s", resp.StatusCode, string(body))
	}
	var peers []peerInfo
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return nil, fmt.Errorf("parse peers response: %w", err)
	}
	return peers, nil
}

func (c *controlClient) Run(ctx context.Context, peerNodeKey, clientSessionID string, argv []string) (runResult, error) {
	body, err := json.Marshal(runRequest{PeerNodeKey: peerNodeKey, ClientSessionID: clientSessionID, Argv: argv})
	if err != nil {
		return runResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/run", bytes.NewReader(body))
	if err != nil {
		return runResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return runResult{}, fmt.Errorf("request run: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return runResult{}, fmt.Errorf("run (status %d): %s", resp.StatusCode, string(respBody))
	}
	var out runResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return runResult{}, fmt.Errorf("parse run response: %w", err)
	}
	return runResult{Output: out.Output, ExitCode: out.ExitCode}, nil
}
