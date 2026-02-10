package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"workspaces-platform/internal/netutil"
)

// ws-relay is a small client used as an OpenSSH ProxyCommand.
//
// It talks to ws-relayd over a local unix socket and then shuttles bytes.
// This is experimental and intended for a "no gateway kubeconfig" connectivity
// mode (desktop agent reverse tunnel).
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dial":
		cmdDial(os.Args[2:])
	case "proxy":
		cmdProxy(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `ws-relay: reverse-tunnel ProxyCommand helper (experimental)

Usage:
  ws-relay dial  --socket /var/run/ws-relayd.sock --key desktops/jonathan --port 2222
  ws-relay proxy --socket /var/run/ws-relayd.sock --namespace desktops --host-prefix desk- <host> <port>

ProxyCommand example:
  ProxyCommand ssh ws-gateway -- ws-relay proxy --namespace desktops --host-prefix desk- %%h %%p
`)
}

type dialReq struct {
	Key  string `json:"key"`
	Port int    `json:"port"`
}

type dialResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func cmdDial(args []string) {
	fs := flag.NewFlagSet("dial", flag.ExitOnError)
	var (
		socket = fs.String("socket", getenv("WS_RELAYD_SOCKET", "/var/run/ws-relayd.sock"), "ws-relayd unix socket path")
		key    = fs.String("key", "", "desktop key namespace/desktop")
		port   = fs.Int("port", 0, "target port (e.g. 2222)")
	)
	fs.Parse(args)

	if strings.TrimSpace(*key) == "" || *port <= 0 || *port > 65535 {
		fs.Usage()
		os.Exit(2)
	}

	c, err := net.Dial("unix", strings.TrimSpace(*socket))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial socket:", err)
		os.Exit(1)
	}
	defer c.Close()

	if err := writeJSONLine(c, dialReq{Key: strings.TrimSpace(*key), Port: *port}); err != nil {
		fmt.Fprintln(os.Stderr, "write request:", err)
		os.Exit(1)
	}

	r := bufio.NewReaderSize(c, 64<<10)
	line, rest, err := readLineWithRest(r, 64<<10)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read response:", err)
		os.Exit(1)
	}
	var resp dialResp
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		fmt.Fprintln(os.Stderr, "invalid response json:", err)
		os.Exit(1)
	}
	if !resp.OK {
		msg := strings.TrimSpace(resp.Error)
		if msg == "" {
			msg = "dial rejected"
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}

	// Start raw shuttle. Include any buffered bytes after the response line.
	src := io.Reader(c)
	if len(rest) != 0 {
		src = io.MultiReader(bytes.NewReader(rest), c)
	}

	copyErr := make(chan error, 2)
	go func() {
		_, e := io.Copy(c, os.Stdin)
		if tcp, ok := c.(*net.UnixConn); ok {
			_ = tcp.CloseWrite()
		}
		copyErr <- e
	}()
	go func() {
		_, e := io.Copy(os.Stdout, src)
		copyErr <- e
	}()
	<-copyErr
}

func cmdProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	var (
		socket     = fs.String("socket", getenv("WS_RELAYD_SOCKET", "/var/run/ws-relayd.sock"), "ws-relayd unix socket path")
		namespace  = fs.String("namespace", "desktops", "default namespace")
		hostPrefix = fs.String("host-prefix", "desk-", "prefix to strip from host")
	)
	fs.Parse(args)

	if fs.NArg() < 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: ws-relay proxy [flags] <host> <port>")
		os.Exit(2)
	}

	host := strings.TrimSpace(fs.Arg(0))
	portStr := strings.TrimSpace(fs.Arg(1))
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		fmt.Fprintf(os.Stderr, "invalid port %q\n", portStr)
		os.Exit(2)
	}

	deskNS, deskName, err := parseDesktopHost(host, strings.TrimSpace(*namespace), strings.TrimSpace(*hostPrefix))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// SSH connects to port 22 by default, but our desktop sshd listens on 2222.
	// In port-forward mode, the Service maps 22 -> 2222. In relay mode we connect
	// directly to the in-pod port.
	if p == 22 {
		p = 2222
	}

	key := deskNS + "/" + deskName
	args2 := []string{
		"--socket", strings.TrimSpace(*socket),
		"--key", key,
		"--port", strconv.Itoa(p),
	}
	cmdDial(args2)
}

func parseDesktopHost(host, defaultNS, prefix string) (namespace, desktop string, _ error) {
	h := strings.TrimSpace(host)
	if h == "" {
		return "", "", errors.New("empty host")
	}
	if strings.ContainsAny(h, " \t\r\n") {
		return "", "", errors.New("host contains whitespace")
	}

	parts := strings.Split(h, ".")
	name := parts[0]
	ns := defaultNS
	if len(parts) >= 2 && strings.TrimSpace(parts[1]) != "" {
		ns = strings.TrimSpace(parts[1])
	}

	if prefix != "" && strings.HasPrefix(name, prefix) {
		name = strings.TrimPrefix(name, prefix)
	}
	name = strings.Trim(name, ".")
	if name == "" {
		return "", "", fmt.Errorf("invalid host %q", host)
	}

	// Validate conservative DNS-ish tokens.
	if err := netutil.ValidateExactHostname(name + ".example"); err != nil {
		return "", "", fmt.Errorf("invalid desktop name derived from host %q", host)
	}
	if err := netutil.ValidateExactHostname(ns + ".example"); err != nil {
		return "", "", fmt.Errorf("invalid namespace derived from host %q", host)
	}

	return ns, name, nil
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

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
