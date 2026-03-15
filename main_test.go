package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"testing"
)

func TestRewriteLineRewritesJSONPaths(t *testing.T) {
	mappings := pathMappings{{from: "/workspace", to: "/Users/test/work"}}
	line := []byte(`{"jsonrpc":"2.0","method":"session/new","params":{"cwd":"/workspace/repo","paths":["/workspace/repo/a.txt"]}}` + "\n")

	rewritten := rewriteLine(line, mappings, rewriteClientToHost)

	var payload map[string]any
	if err := json.Unmarshal(bytesTrimSpace(rewritten), &payload); err != nil {
		t.Fatalf("unmarshal rewritten line: %v", err)
	}
	params := payload["params"].(map[string]any)
	if got := params["cwd"].(string); got != "/Users/test/work/repo" {
		t.Fatalf("cwd rewrite mismatch: %q", got)
	}
	paths := params["paths"].([]any)
	if got := paths[0].(string); got != "/Users/test/work/repo/a.txt" {
		t.Fatalf("nested path rewrite mismatch: %q", got)
	}
}

func TestRewriteLineLeavesNonJSONUntouched(t *testing.T) {
	line := []byte("plain-text\n")
	got := rewriteLine(line, pathMappings{{from: "/workspace", to: "/Users/test/work"}}, rewriteClientToHost)
	if string(got) != string(line) {
		t.Fatalf("expected non-JSON line to stay unchanged: %q", got)
	}
}

func TestTCPBridgeRoundTrip(t *testing.T) {
	cfg := serverConfig{
		token:    "test-token",
		codexCmd: "cat",
	}
	address, shutdown := startTCPBridge(t, cfg)
	defer shutdown()

	output := runBridgeRoundTrip(t, address, handshakeRequest{
		Token: "test-token",
		Agent: "codex",
		Cwd:   t.TempDir(),
	}, "bridge-tcp-ok\n")

	if output != "bridge-tcp-ok\n" {
		t.Fatalf("unexpected tcp output: %q", output)
	}
}

func TestHTTPConnectBridgeRoundTrip(t *testing.T) {
	cfg := serverConfig{
		token:    "test-token",
		httpPath: "/v1/connect",
		codexCmd: "cat",
	}
	serverURL, shutdown := startHTTPBridge(t, cfg)
	defer shutdown()

	output := runBridgeRoundTrip(t, serverURL, handshakeRequest{
		Token: "test-token",
		Agent: "codex",
		Cwd:   t.TempDir(),
	}, "bridge-http-ok\n")

	if output != "bridge-http-ok\n" {
		t.Fatalf("unexpected http output: %q", output)
	}
}

func startTCPBridge(t *testing.T, cfg serverConfig) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				return
			}
			go handleBridgeConn(ctx, conn, conn.RemoteAddr().String(), cfg)
		}
	}()
	return ln.Addr().String(), func() {
		cancel()
		_ = ln.Close()
	}
}

func startHTTPBridge(t *testing.T, cfg serverConfig) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(newHTTPHandler(ctx, cfg))
	return server.URL + cfg.httpPath, func() {
		cancel()
		server.Close()
	}
}

func runBridgeRoundTrip(t *testing.T, server string, req handshakeRequest, payload string) string {
	t.Helper()
	conn, err := openServerConnection(server)
	if err != nil {
		t.Fatalf("openServerConnection: %v", err)
	}
	defer conn.Close()

	if err := writeJSONLine(conn, req); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	respLine, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	var resp handshakeResponse
	if err := json.Unmarshal(bytesTrimSpace(respLine), &resp); err != nil {
		t.Fatalf("unmarshal handshake response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("unexpected handshake failure: %s", resp.Error)
	}

	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := closer.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
