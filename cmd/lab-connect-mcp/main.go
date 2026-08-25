// Command lab-connect-mcp is the MCP server for lab-connect's Gateway
// image: it queries the local Runner's Peer list + Pairing state over the
// control API and exposes one run-command tool per paired Node — nothing
// about unpaired Nodes is ever visible.
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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// statePaired mirrors lab-connect's internal/pairing.StatePaired — the
// only Pairing state string this binary needs to recognize.
const statePaired = "paired"

// refreshInterval is how often the tool set is reconciled against the
// Runner's live Peer/Pairing state. This is a UX/visibility concern only
// — the actual per-call Pairing check happens on every Client.Run call
// regardless of how stale the tool listing is, enforced by the Runner's
// own control.Server.
const refreshInterval = 3 * time.Second

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

	server := mcp.NewServer(&mcp.Implementation{Name: "lab-connect-mcp", Version: "v1"}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go refreshTools(ctx, server, client)

	if *stdio {
		return server.Run(ctx, &mcp.StdioTransport{})
	}

	// Many concurrent AI-agent clients now hit this one process, so each
	// gets its own MCP session id for free from the SDK (req.Session.ID())
	// rather than a single id minted for the process lifetime — see
	// addRunCommandTool.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return http.ListenAndServe(*addr, handler)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// refreshTools polls the local Runner's Peer list and keeps the MCP
// server's tool set in sync: one run-command tool per currently-Paired
// Node, nothing for anything else. A newly formed Pairing becomes a
// usable tool without restarting this process (AddTool sends a
// list_changed notification to already-connected sessions).
func refreshTools(ctx context.Context, server *mcp.Server, client *controlClient) {
	registered := map[string]string{} // NodeKey -> tool name
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	sync := func() {
		peers, err := client.Peers(ctx)
		if err != nil {
			return // Runner not reachable right now; keep the last-known tool set
		}

		wanted := map[string]bool{}
		for _, p := range peers {
			if p.PairingState != statePaired {
				continue
			}
			wanted[p.NodeKey] = true
			if _, ok := registered[p.NodeKey]; ok {
				continue
			}
			name := toolName(p)
			registered[p.NodeKey] = name
			addRunCommandTool(server, client, name, p.NodeKey, p.Name)
		}

		var stale []string
		for nodeKey, name := range registered {
			if !wanted[nodeKey] {
				stale = append(stale, name)
				delete(registered, nodeKey)
			}
		}
		if len(stale) > 0 {
			server.RemoveTools(stale...)
		}
	}

	sync()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sync()
		}
	}
}

// runCommandInput is the run-command tool's argument shape: argv-array,
// matching the Runner's own "never a shell string" contract end to end.
type runCommandInput struct {
	Argv []string `json:"argv" jsonschema:"the command and its arguments as an argv-style array — e.g. [\"docker\",\"ps\"] — never a single shell string"`
}

func addRunCommandTool(server *mcp.Server, client *controlClient, toolName, peerNodeKey, peerName string) {
	mcp.AddTool(server, &mcp.Tool{
		Name: toolName,
		Description: fmt.Sprintf(
			"Run a command on the paired lab-connect Node %q. "+
				"v1 access is UNSCOPED FULL-SHELL: there is no allow-list and no command-class restriction — "+
				"the command runs with whatever privileges the Runner process has on that machine. "+
				"Only call this against a machine you would hand full shell access to today. "+
				"Every call is recorded as an Audit Entry on the target Node.",
			peerName,
		),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in runCommandInput) (*mcp.CallToolResult, any, error) {
		if len(in.Argv) == 0 {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "argv must not be empty"}},
			}, nil, nil
		}
		result, err := client.Run(ctx, peerNodeKey, req.Session.ID(), in.Argv)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("exit status %d\n%s", result.ExitCode, string(result.Output)),
			}},
		}, nil, nil
	})
}

// toolName derives a valid, reasonably-collision-resistant MCP tool name
// from a Peer: mcp.AddTool requires [a-zA-Z0-9_.-]+, so a hostname alone
// isn't always safe, and a NodeKey suffix disambiguates two Peers that
// happen to share a display name.
func toolName(p peerInfo) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, p.Name)
	suffix := p.NodeKey
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return fmt.Sprintf("run-command-%s-%s", sanitized, suffix)
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
