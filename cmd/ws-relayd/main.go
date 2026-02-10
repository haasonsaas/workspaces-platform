package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ws-relayd is an experimental reverse-tunnel relay intended to reduce
// gateway Kubernetes API surface.
//
// Model:
// - desktop-side "agent" maintains an outbound control connection to relayd
// - relayd requests a data stream by sending an "open" message over control
// - agent connects back to relayd's data listener and binds the stream token
// - relayd bridges bytes between a local ProxyCommand client (unix socket)
//   and the agent's data connection
//
// This is deliberately simple: no multiplexing, one data connection per stream.
//
// WARNING: not enabled/used by default in this repo. See docs/connectivity.md.

type registerMsg struct {
	Key   string `json:"key"`   // namespace/desktop
	Token string `json:"token"` // shared secret
}

type openMsg struct {
	Type   string `json:"type"` // "open"
	Stream string `json:"stream"`
	Port   int    `json:"port"`
}

type dataHello struct {
	Stream string `json:"stream"`
	Key    string `json:"key"`
	Token  string `json:"token"`
}

type dialReq struct {
	Key  string `json:"key"`
	Port int    `json:"port"`
}

type dialResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type agent struct {
	key   string
	conn  net.Conn
	wmu   sync.Mutex
	alive chan struct{}
}

func (a *agent) send(v any) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	_, err := a.conn.Write(b)
	return err
}

type server struct {
	auth authConfig

	allowedPorts map[int]struct{}

	mu     sync.Mutex
	agents map[string]*agent

	pending map[string]*pendingStream
}

type authConfig struct {
	tokens    map[string]string
	jwtSecret []byte
}

func (a authConfig) validate(key, token string) bool {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(token) == "" {
		return false
	}
	if len(a.jwtSecret) != 0 {
		if validateJWT(a.jwtSecret, key, token) {
			return true
		}
	}
	return tokenMatches(a.tokens, key, token)
}

type pendingStream struct {
	key  string
	port int
	ch   chan net.Conn
}

func main() {
	var (
		controlAddr = flag.String("control-addr", getenv("WS_RELAYD_CONTROL_ADDR", ":7443"), "TCP addr for agent control connections")
		dataAddr    = flag.String("data-addr", getenv("WS_RELAYD_DATA_ADDR", ":7444"), "TCP addr for agent data connections")
		socketPath  = flag.String("socket", getenv("WS_RELAYD_SOCKET", "/var/run/ws-relayd.sock"), "Unix socket path for local dial requests")
		jwtSecret   = flag.String("jwt-secret", getenv("WS_RELAYD_JWT_SECRET", ""), "HMAC secret for agent JWT auth (optional but recommended)")
		tokensJSON  = flag.String("tokens-json", getenv("WS_RELAYD_TOKENS_JSON", ""), "JSON map of {\"namespace/desktop\":\"token\"}")
		tokensFile  = flag.String("tokens-file", getenv("WS_RELAYD_TOKENS_FILE", ""), "Path to JSON tokens file (same format as --tokens-json)")
		portsCSV    = flag.String("allowed-ports", getenv("WS_RELAYD_ALLOWED_PORTS", "2222"), "CSV of allowed target ports (default: 2222 only)")
		streamTO    = flag.Duration("stream-timeout", parseDuration(getenv("WS_RELAYD_STREAM_TIMEOUT", "20s"), 20*time.Second), "Timeout waiting for agent data connection")
	)
	flag.Parse()

	tokens, err := loadTokens(*tokensJSON, *tokensFile)
	if err != nil {
		log.Fatalf("tokens: %v", err)
	}
	secret := []byte(strings.TrimSpace(*jwtSecret))
	if len(secret) == 0 && len(tokens) == 0 {
		log.Fatalf("no auth configured (set WS_RELAYD_JWT_SECRET or WS_RELAYD_TOKENS_JSON/FILE)")
	}

	allowedPorts, err := parseAllowedPorts(*portsCSV)
	if err != nil {
		log.Fatalf("allowed ports: %v", err)
	}

	s := &server{
		auth:         authConfig{tokens: tokens, jwtSecret: secret},
		allowedPorts: allowedPorts,
		agents:       map[string]*agent{},
		pending:      map[string]*pendingStream{},
	}

	// Control listener (agents register here).
	ctlLn, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		log.Fatalf("listen control: %v", err)
	}
	defer ctlLn.Close()

	// Data listener (agents connect here for each stream).
	dataLn, err := net.Listen("tcp", *dataAddr)
	if err != nil {
		log.Fatalf("listen data: %v", err)
	}
	defer dataLn.Close()

	// Unix socket listener for local dial requests.
	if err := ensureUnixSocketDir(*socketPath); err != nil {
		log.Fatalf("socket dir: %v", err)
	}
	if err := removeUnixSocketIfPresent(*socketPath); err != nil {
		log.Fatalf("socket: %v", err)
	}
	unixLn, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listen socket: %v", err)
	}
	defer unixLn.Close()
	_ = os.Chmod(*socketPath, 0o600)

	log.Printf("ws-relayd: control=%s data=%s socket=%s allowedPorts=%v", strings.TrimSpace(*controlAddr), strings.TrimSpace(*dataAddr), *socketPath, keysOf(allowedPorts))

	go s.acceptControl(ctlLn)
	go s.acceptData(dataLn)
	s.acceptDial(unixLn, *streamTO)
}

func (s *server) acceptControl(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("control accept: %v", err)
			return
		}
		go s.handleControlConn(c)
	}
}

func (s *server) handleControlConn(c net.Conn) {
	defer func() { _ = c.Close() }()

	r := bufio.NewReaderSize(c, 64<<10)
	line, rest, err := readLineWithRest(r, 64<<10)
	if err != nil {
		log.Printf("control handshake read: %v", err)
		return
	}
	_ = rest // no extra bytes expected in control channel

	var reg registerMsg
	if err := json.Unmarshal([]byte(line), &reg); err != nil {
		log.Printf("control handshake json: %v", err)
		return
	}
	key := strings.TrimSpace(reg.Key)
	tok := strings.TrimSpace(reg.Token)
	if key == "" || tok == "" {
		log.Printf("control handshake missing key/token")
		return
	}
	if !s.auth.validate(key, tok) {
		log.Printf("control handshake invalid token key=%q", key)
		return
	}

	a := &agent{key: key, conn: c, alive: make(chan struct{})}

	s.mu.Lock()
	if old := s.agents[key]; old != nil {
		_ = old.conn.Close()
	}
	s.agents[key] = a
	s.mu.Unlock()

	log.Printf("agent registered key=%s remote=%s", key, c.RemoteAddr().String())

	// Keep reading until EOF to detect disconnect.
	_, _ = io.Copy(io.Discard, r)

	s.mu.Lock()
	cur := s.agents[key]
	if cur == a {
		delete(s.agents, key)
	}
	s.mu.Unlock()
	log.Printf("agent disconnected key=%s", key)
}

func (s *server) acceptData(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("data accept: %v", err)
			return
		}
		go s.handleDataConn(c)
	}
}

func (s *server) handleDataConn(c net.Conn) {
	defer func() {
		// The dial handler takes ownership on success.
	}()

	r := bufio.NewReaderSize(c, 64<<10)
	line, rest, err := readLineWithRest(r, 64<<10)
	if err != nil {
		_ = c.Close()
		return
	}

	var hello dataHello
	if err := json.Unmarshal([]byte(line), &hello); err != nil {
		_ = c.Close()
		return
	}
	stream := strings.TrimSpace(hello.Stream)
	key := strings.TrimSpace(hello.Key)
	tok := strings.TrimSpace(hello.Token)
	if stream == "" || key == "" || tok == "" {
		_ = c.Close()
		return
	}
	if !s.auth.validate(key, tok) {
		_ = c.Close()
		return
	}

	s.mu.Lock()
	p := s.pending[stream]
	// Only accept data connections that match a currently pending stream token.
	if p == nil || p.key != key {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	delete(s.pending, stream)
	s.mu.Unlock()

	// Wrap the connection so buffered bytes from the hello read are not lost.
	conn := &connWithReadRest{Conn: c, rest: rest}

	select {
	case p.ch <- conn:
		// ownership transferred; do not close here
		return
	default:
		_ = c.Close()
		return
	}
}

func (s *server) acceptDial(ln net.Listener, streamTimeout time.Duration) {
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("dial accept: %v", err)
			return
		}
		go s.handleDialConn(c, streamTimeout)
	}
}

func (s *server) handleDialConn(c net.Conn, streamTimeout time.Duration) {
	defer func() { _ = c.Close() }()

	r := bufio.NewReaderSize(c, 64<<10)
	line, _, err := readLineWithRest(r, 64<<10)
	if err != nil {
		_ = writeJSONLine(c, dialResp{OK: false, Error: "invalid request"})
		return
	}

	var req dialReq
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		_ = writeJSONLine(c, dialResp{OK: false, Error: "invalid json"})
		return
	}
	key := strings.TrimSpace(req.Key)
	port := req.Port
	if key == "" || port <= 0 || port > 65535 {
		_ = writeJSONLine(c, dialResp{OK: false, Error: "invalid key/port"})
		return
	}
	if _, ok := s.allowedPorts[port]; !ok {
		_ = writeJSONLine(c, dialResp{OK: false, Error: "port not allowed"})
		return
	}

	s.mu.Lock()
	a := s.agents[key]
	s.mu.Unlock()
	if a == nil {
		_ = writeJSONLine(c, dialResp{OK: false, Error: "desktop not connected"})
		return
	}

	stream := randomToken(16)
	p := &pendingStream{key: key, port: port, ch: make(chan net.Conn, 1)}
	s.mu.Lock()
	s.pending[stream] = p
	s.mu.Unlock()

	// Ask agent to open a data stream.
	if err := a.send(openMsg{Type: "open", Stream: stream, Port: port}); err != nil {
		s.mu.Lock()
		delete(s.pending, stream)
		s.mu.Unlock()
		_ = writeJSONLine(c, dialResp{OK: false, Error: "failed to contact desktop"})
		return
	}

	var dataConn net.Conn
	select {
	case dataConn = <-p.ch:
	case <-time.After(streamTimeout):
		s.mu.Lock()
		delete(s.pending, stream)
		s.mu.Unlock()
		_ = writeJSONLine(c, dialResp{OK: false, Error: "timeout waiting for desktop stream"})
		return
	}
	defer func() { _ = dataConn.Close() }()

	// Signal the client to start raw streaming.
	if err := writeJSONLine(c, dialResp{OK: true}); err != nil {
		return
	}

	copyErr := make(chan error, 2)
	go func() {
		_, e := io.Copy(dataConn, r)
		if tcp, ok := dataConn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyErr <- e
	}()
	go func() {
		_, e := io.Copy(c, dataConn)
		copyErr <- e
	}()
	<-copyErr
}

func loadTokens(tokensJSON, tokensFile string) (map[string]string, error) {
	raw := strings.TrimSpace(tokensJSON)
	if raw == "" && strings.TrimSpace(tokensFile) != "" {
		b, err := os.ReadFile(strings.TrimSpace(tokensFile))
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(string(b))
	}
	if raw == "" {
		return map[string]string{}, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	normalized := map[string]string{}
	for k, v := range out {
		key := strings.TrimSpace(k)
		tok := strings.TrimSpace(v)
		if key == "" || tok == "" {
			continue
		}
		normalized[key] = tok
	}
	return normalized, nil
}

func tokenMatches(tokens map[string]string, key, token string) bool {
	want := strings.TrimSpace(tokens[strings.TrimSpace(key)])
	return want != "" && strings.TrimSpace(token) == want
}

func validateJWT(secret []byte, key, token string) bool {
	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method == nil || t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected jwt alg")
		}
		return secret, nil
	})
	if err != nil || parsed == nil || !parsed.Valid {
		return false
	}
	sub := strings.TrimSpace(claims.Subject)
	if sub == "" || sub != strings.TrimSpace(key) {
		return false
	}
	// Enforce expiry when present. RegisteredClaims.Valid() checks exp/nbf when the
	// parser has timefunc configured, but we keep this explicit and conservative.
	if claims.ExpiresAt != nil && !claims.ExpiresAt.Time.IsZero() && time.Now().After(claims.ExpiresAt.Time) {
		return false
	}
	return true
}

func parseAllowedPorts(csv string) (map[int]struct{}, error) {
	out := map[int]struct{}{}
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("invalid port %q", part)
		}
		out[n] = struct{}{}
	}
	if len(out) == 0 {
		return nil, errors.New("empty allowlist")
	}
	return out, nil
}

func keysOf(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic print.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func readLineWithRest(r *bufio.Reader, maxBytes int) (line string, rest []byte, _ error) {
	b, err := r.ReadBytes('\n')
	if err != nil {
		return "", nil, err
	}
	if maxBytes > 0 && len(b) > maxBytes {
		return "", nil, fmt.Errorf("line too long")
	}
	line = strings.TrimSpace(string(b))
	if n := r.Buffered(); n > 0 {
		buf, _ := r.Peek(n)
		rest = append([]byte(nil), buf...)
		_, _ = r.Discard(n)
	}
	return line, rest, nil
}

func writeJSONLine(w io.Writer, v any) error {
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	_, err := w.Write(b)
	return err
}

type connWithReadRest struct {
	net.Conn
	rest []byte
}

func (c *connWithReadRest) Read(p []byte) (int, error) {
	if len(c.rest) > 0 {
		n := copy(p, c.rest)
		c.rest = c.rest[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

func randomToken(n int) string {
	if n <= 0 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// fallback
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

func ensureUnixSocketDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o700)
}

func removeUnixSocketIfPresent(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a unix socket", path)
	}
	return os.Remove(path)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseDuration(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
