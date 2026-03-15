package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

const (
	defaultTCPListenAddr = "0.0.0.0:4601"
	defaultServerAddr    = "host.docker.internal:4601"
	defaultHTTPPath      = "/v1/connect"
	defaultCodexCmd      = "npx -y @zed-industries/codex-acp@0.10.0"
	defaultClaudeCmd     = "npx -y @zed-industries/claude-agent-acp@0.21.0"
	defaultDialTimeout   = 10 * time.Second
	inputDrainGrace      = 2 * time.Second
	maxHandshakeBytes    = 64 * 1024
)

type handshakeRequest struct {
	Token string `json:"token"`
	Agent string `json:"agent"`
	Cwd   string `json:"cwd"`
}

type handshakeResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type pathMapping struct {
	from string
	to   string
}

type pathMappings []pathMapping

type rewriteDirection int

const (
	rewriteClientToHost rewriteDirection = iota
	rewriteHostToClient
)

type serverConfig struct {
	tcpListenAddr  string
	httpListenAddr string
	httpPath       string
	token          string
	codexCmd       string
	claudeCmd      string
	mappings       pathMappings
}

type clientConfig struct {
	server string
	token  string
	agent  string
	cwd    string
}

type prefixedConn struct {
	net.Conn
	reader io.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *prefixedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usageAndExit()
	}

	switch os.Args[1] {
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "client":
		if err := runClient(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "version", "--version", "-version":
		fmt.Println(version)
	default:
		usageAndExit()
	}
}

func usageAndExit() {
	fmt.Fprintf(os.Stderr, "usage: %s <serve|client|version> [flags]\n", filepath.Base(os.Args[0]))
	os.Exit(2)
}

func runServe(args []string) error {
	cfg := serverConfig{
		tcpListenAddr:  envOrDefault("ACPX_HOST_BRIDGE_LISTEN", defaultTCPListenAddr),
		httpListenAddr: strings.TrimSpace(os.Getenv("ACPX_HOST_BRIDGE_HTTP_LISTEN")),
		httpPath:       envOrDefault("ACPX_HOST_BRIDGE_HTTP_PATH", defaultHTTPPath),
		token:          os.Getenv("ACPX_HOST_BRIDGE_TOKEN"),
		codexCmd:       envOrDefault("ACPX_HOST_BRIDGE_CODEX_CMD", defaultCodexCmd),
		claudeCmd:      envOrDefault("ACPX_HOST_BRIDGE_CLAUDE_CMD", defaultClaudeCmd),
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.tcpListenAddr, "listen", cfg.tcpListenAddr, "raw TCP listen address; set to empty to disable")
	fs.StringVar(&cfg.httpListenAddr, "http-listen", cfg.httpListenAddr, "HTTP CONNECT listen address")
	fs.StringVar(&cfg.httpPath, "http-path", cfg.httpPath, "HTTP CONNECT path")
	fs.StringVar(&cfg.token, "token", cfg.token, "shared secret")
	fs.StringVar(&cfg.codexCmd, "codex-cmd", cfg.codexCmd, "host command for the codex ACP adapter")
	fs.StringVar(&cfg.claudeCmd, "claude-cmd", cfg.claudeCmd, "host command for the claude ACP adapter")
	fs.Var(&cfg.mappings, "map", "container-to-host path mapping in from=to form; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.token == "" {
		return errors.New("missing shared secret: pass --token or set ACPX_HOST_BRIDGE_TOKEN")
	}
	if cfg.httpPath == "" {
		cfg.httpPath = defaultHTTPPath
	}
	if !strings.HasPrefix(cfg.httpPath, "/") {
		cfg.httpPath = "/" + cfg.httpPath
	}
	if cfg.tcpListenAddr == "" && cfg.httpListenAddr == "" {
		return errors.New("nothing to listen on: provide --listen and/or --http-listen")
	}

	log.Printf("version: %s", version)
	log.Printf("codex command: %s", cfg.codexCmd)
	log.Printf("claude command: %s", cfg.claudeCmd)
	if len(cfg.mappings) > 0 {
		log.Printf("path mappings: %s", cfg.mappings.String())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	if cfg.tcpListenAddr != "" {
		ln, err := net.Listen("tcp", cfg.tcpListenAddr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", cfg.tcpListenAddr, err)
		}
		log.Printf("tcp listening on %s", cfg.tcpListenAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ln.Close()
			<-ctx.Done()
			_ = ln.Close()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveTCPListener(ctx, ln, cfg); err != nil && !errors.Is(err, net.ErrClosed) {
				errCh <- err
			}
		}()
	}

	if cfg.httpListenAddr != "" {
		srv := &http.Server{
			Addr:    cfg.httpListenAddr,
			Handler: newHTTPHandler(ctx, cfg),
		}
		log.Printf("http CONNECT listening on %s%s", cfg.httpListenAddr, cfg.httpPath)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		stop()
		wg.Wait()
		return err
	}

	wg.Wait()
	return nil
}

func serveTCPListener(ctx context.Context, ln net.Listener, cfg serverConfig) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Printf("accept: %v", err)
				continue
			}
			return err
		}
		go handleBridgeConn(ctx, conn, conn.RemoteAddr().String(), cfg)
	}
}

func newHTTPHandler(ctx context.Context, cfg serverConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"version": version,
			"path":    cfg.httpPath,
		})
	})
	mux.HandleFunc(cfg.httpPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			_ = conn.Close()
			return
		}
		if err := rw.Flush(); err != nil {
			_ = conn.Close()
			return
		}
		go handleBridgeConn(ctx, conn, r.RemoteAddr, cfg)
	})
	return mux
}

func handleBridgeConn(parent context.Context, conn net.Conn, peer string, cfg serverConfig) {
	defer conn.Close()
	logPrefix := fmt.Sprintf("[%s]", peer)

	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}

	reader := bufio.NewReader(conn)
	line, err := readHandshakeLine(reader)
	if err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: fmt.Sprintf("read handshake: %v", err)})
		log.Printf("%s handshake read failed: %v", logPrefix, err)
		return
	}

	var req handshakeRequest
	if err := json.Unmarshal(bytesTrimSpace(line), &req); err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: fmt.Sprintf("invalid handshake json: %v", err)})
		log.Printf("%s invalid handshake: %v", logPrefix, err)
		return
	}

	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(cfg.token)) != 1 {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: "unauthorized"})
		log.Printf("%s rejected unauthorized request for agent=%q", logPrefix, req.Agent)
		return
	}

	command, err := resolveAgentCommand(req.Agent, cfg)
	if err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: err.Error()})
		log.Printf("%s rejected request: %v", logPrefix, err)
		return
	}

	hostCwd, err := resolvePath(req.Cwd, cfg.mappings, rewriteClientToHost)
	if err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: err.Error()})
		log.Printf("%s invalid cwd %q: %v", logPrefix, req.Cwd, err)
		return
	}
	if err := requireDirectory(hostCwd); err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: err.Error()})
		log.Printf("%s invalid cwd %q: %v", logPrefix, req.Cwd, err)
		return
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", command)
	cmd.Dir = hostCwd
	cmd.Env = os.Environ()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: fmt.Sprintf("stdin pipe: %v", err)})
		log.Printf("%s stdin pipe failed: %v", logPrefix, err)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: fmt.Sprintf("stdout pipe: %v", err)})
		log.Printf("%s stdout pipe failed: %v", logPrefix, err)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: fmt.Sprintf("stderr pipe: %v", err)})
		log.Printf("%s stderr pipe failed: %v", logPrefix, err)
		return
	}
	if err := cmd.Start(); err != nil {
		_ = writeHandshakeResponse(conn, handshakeResponse{OK: false, Error: fmt.Sprintf("start command: %v", err)})
		log.Printf("%s start failed for agent=%q cwd=%q: %v", logPrefix, req.Agent, hostCwd, err)
		return
	}

	log.Printf("%s started agent=%q pid=%d cwd=%q", logPrefix, req.Agent, cmd.Process.Pid, hostCwd)
	if err := writeHandshakeResponse(conn, handshakeResponse{OK: true}); err != nil {
		log.Printf("%s write handshake response failed: %v", logPrefix, err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}

	go streamStderr(stderrPipe, logPrefix, req.Agent)

	var copyWG sync.WaitGroup
	copyWG.Add(2)

	stdinDoneCh := make(chan struct{}, 1)
	stdoutDoneCh := make(chan struct{}, 1)

	go func() {
		defer copyWG.Done()
		defer stdinPipe.Close()
		_, _ = relayStream(stdinPipe, reader, cfg.mappings, rewriteClientToHost)
		stdinDoneCh <- struct{}{}
	}()

	go func() {
		defer copyWG.Done()
		_, _ = relayStream(conn, stdoutPipe, cfg.mappings, rewriteHostToClient)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		stdoutDoneCh <- struct{}{}
	}()

	waitErrCh := make(chan error, 1)
	go func() {
		waitErrCh <- cmd.Wait()
	}()

	var drainTimerCh <-chan time.Time
	stdoutClosed := false
	forcedCancel := false

	for !stdoutClosed {
		select {
		case <-ctx.Done():
			forcedCancel = true
			cancel()
		case <-stdinDoneCh:
			if drainTimerCh == nil {
				drainTimerCh = time.After(inputDrainGrace)
			}
		case <-drainTimerCh:
			forcedCancel = true
			cancel()
			drainTimerCh = nil
		case <-stdoutDoneCh:
			stdoutClosed = true
		}
	}
	copyWG.Wait()
	err = <-waitErrCh
	if forcedCancel {
		log.Printf("%s agent=%q terminated after client disconnect", logPrefix, req.Agent)
	} else if err != nil {
		log.Printf("%s agent=%q exited with error: %v", logPrefix, req.Agent, err)
	} else {
		log.Printf("%s agent=%q exited cleanly", logPrefix, req.Agent)
	}
}

func runClient(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	cfg := clientConfig{
		server: envOrDefault("ACPX_HOST_BRIDGE_SERVER", defaultServerAddr),
		token:  os.Getenv("ACPX_HOST_BRIDGE_TOKEN"),
		cwd:    cwd,
	}

	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	fs.StringVar(&cfg.server, "server", cfg.server, "bridge server address or URL")
	fs.StringVar(&cfg.token, "token", cfg.token, "shared secret")
	fs.StringVar(&cfg.agent, "agent", cfg.agent, "remote agent name (codex or claude)")
	fs.StringVar(&cfg.cwd, "cwd", cfg.cwd, "reported working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.token == "" {
		return errors.New("missing shared secret: pass --token or set ACPX_HOST_BRIDGE_TOKEN")
	}
	if cfg.agent == "" {
		return errors.New("missing --agent")
	}

	conn, err := openServerConnection(cfg.server)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := handshakeRequest{
		Token: cfg.token,
		Agent: cfg.agent,
		Cwd:   cfg.cwd,
	}
	if err := writeJSONLine(conn, req); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	respLine, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read handshake response: %w", err)
	}

	var resp handshakeResponse
	if err := json.Unmarshal(bytesTrimSpace(respLine), &resp); err != nil {
		return fmt.Errorf("decode handshake response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("remote rejected request: %s", resp.Error)
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		if tcp, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = tcp.CloseWrite()
		}
		errCh <- err
	}()

	go func() {
		_, err := io.Copy(os.Stdout, reader)
		errCh <- err
	}()

	var firstErr error
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil && !errors.Is(err, net.ErrClosed) {
			firstErr = err
		}
	}
	return firstErr
}

func openServerConnection(server string) (io.ReadWriteCloser, error) {
	if !strings.Contains(server, "://") {
		return dialTCP(server)
	}
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("parse server %q: %w", server, err)
	}
	switch u.Scheme {
	case "tcp":
		return dialTCP(u.Host)
	case "http", "https":
		return dialHTTPConnect(u)
	default:
		return nil, fmt.Errorf("unsupported server scheme %q", u.Scheme)
	}
}

func dialTCP(address string) (io.ReadWriteCloser, error) {
	conn, err := net.DialTimeout("tcp", address, defaultDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	return conn, nil
}

func dialHTTPConnect(u *url.URL) (io.ReadWriteCloser, error) {
	address := u.Host
	if !strings.Contains(address, ":") {
		if u.Scheme == "https" {
			address += ":443"
		} else {
			address += ":80"
		}
	}

	var conn net.Conn
	var err error
	if u.Scheme == "https" {
		host := u.Hostname()
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: defaultDialTimeout}, "tcp", address, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = net.DialTimeout("tcp", address, defaultDialTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}

	path := u.EscapedPath()
	if path == "" {
		path = defaultHTTPPath
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Path: path, RawQuery: u.RawQuery},
		Host:   u.Host,
		Header: make(http.Header),
	}
	req.Header.Set("User-Agent", "acpnet/"+version)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT request: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("CONNECT %s returned %s", u.String(), resp.Status)
	}

	return &prefixedConn{
		Conn:   conn,
		reader: reader,
	}, nil
}

func relayStream(dst io.Writer, src io.Reader, mappings pathMappings, direction rewriteDirection) (int64, error) {
	if len(mappings) == 0 {
		return io.Copy(dst, src)
	}

	reader := bufio.NewReader(src)
	var total int64
	for {
		line, err := readLineFragment(reader)
		if len(line) > 0 {
			out := rewriteLine(line, mappings, direction)
			n, writeErr := dst.Write(out)
			total += int64(n)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

func rewriteLine(line []byte, mappings pathMappings, direction rewriteDirection) []byte {
	trimmed := bytesTrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return line
	}

	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return line
	}
	rewritten, changed := rewriteJSONValue(value, mappings, direction)
	if !changed {
		return line
	}
	data, err := json.Marshal(rewritten)
	if err != nil {
		return line
	}
	if line[len(line)-1] == '\n' {
		data = append(data, '\n')
	}
	return data
}

func rewriteJSONValue(value any, mappings pathMappings, direction rewriteDirection) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		next := make(map[string]any, len(typed))
		for key, child := range typed {
			rewrittenChild, childChanged := rewriteJSONValue(child, mappings, direction)
			next[key] = rewrittenChild
			changed = changed || childChanged
		}
		return next, changed
	case []any:
		changed := false
		next := make([]any, len(typed))
		for i, child := range typed {
			rewrittenChild, childChanged := rewriteJSONValue(child, mappings, direction)
			next[i] = rewrittenChild
			changed = changed || childChanged
		}
		return next, changed
	case string:
		rewritten, changed := rewritePath(typed, mappings, direction)
		if changed {
			return rewritten, true
		}
		return typed, false
	default:
		return value, false
	}
}

func rewritePath(input string, mappings pathMappings, direction rewriteDirection) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return input, false
	}

	for _, mapping := range mappings {
		from, to := mapping.from, mapping.to
		if direction == rewriteHostToClient {
			from, to = to, from
		}
		if rewritten, ok := replacePathPrefix(trimmed, from, to); ok {
			if trimmed == input {
				return rewritten, true
			}
			return strings.Replace(input, trimmed, rewritten, 1), true
		}
	}
	return input, false
}

func replacePathPrefix(input string, from string, to string) (string, bool) {
	if input == from {
		return to, true
	}
	if strings.HasPrefix(input, from+string(os.PathSeparator)) {
		suffix := strings.TrimPrefix(input, from)
		return filepath.Clean(to + suffix), true
	}
	return "", false
}

func readLineFragment(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	return line, err
}

func streamStderr(r io.Reader, prefix string, agent string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Printf("%s %s stderr: %s", prefix, agent, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, os.ErrClosed) || strings.Contains(err.Error(), "file already closed") {
			return
		}
		log.Printf("%s %s stderr read failed: %v", prefix, agent, err)
	}
}

func (m *pathMappings) String() string {
	if len(*m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*m))
	for _, item := range *m {
		parts = append(parts, item.from+"="+item.to)
	}
	return strings.Join(parts, ",")
}

func (m *pathMappings) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid --map %q: expected from=to", value)
	}
	from := filepath.Clean(strings.TrimSpace(parts[0]))
	to := filepath.Clean(strings.TrimSpace(parts[1]))
	if from == "." || to == "." || from == "" || to == "" {
		return fmt.Errorf("invalid --map %q: empty path", value)
	}
	*m = append(*m, pathMapping{from: from, to: to})
	return nil
}

func resolveAgentCommand(agent string, cfg serverConfig) (string, error) {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "codex":
		return cfg.codexCmd, nil
	case "claude":
		return cfg.claudeCmd, nil
	default:
		return "", fmt.Errorf("unsupported agent %q", agent)
	}
}

func requireDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	return nil
}

func resolvePath(path string, mappings pathMappings, direction rewriteDirection) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty path")
	}
	rewritten, _ := rewritePath(filepath.Clean(path), mappings, direction)
	return filepath.Clean(rewritten), nil
}

func writeHandshakeResponse(w io.Writer, resp handshakeResponse) error {
	return writeJSONLine(w, resp)
}

func writeJSONLine(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func readHandshakeLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > maxHandshakeBytes {
			return nil, fmt.Errorf("handshake exceeds %d bytes", maxHandshakeBytes)
		}
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

func bytesTrimSpace(data []byte) []byte {
	return []byte(strings.TrimSpace(string(data)))
}

func envOrDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
