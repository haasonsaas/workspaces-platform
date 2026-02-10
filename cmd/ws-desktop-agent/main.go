package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// ws-desktop-agent is an experimental reverse-tunnel client intended to run
// inside a Desktop pod (as a sidecar or as a supervised process).
//
// It maintains a control connection to ws-relayd, and on "open" requests it
// connects back to the relay data listener and bridges bytes to localhost:<port>.
func main() {
	var (
		controlAddr = flag.String("control-addr", getenv("WS_RELAYD_CONTROL_ADDR", ""), "relay control addr (host:port)")
		dataAddr    = flag.String("data-addr", getenv("WS_RELAYD_DATA_ADDR", ""), "relay data addr (host:port)")
		key         = flag.String("key", getenv("WS_RELAY_KEY", ""), "desktop key (namespace/desktop)")
		token       = flag.String("token", getenv("WS_RELAY_TOKEN", ""), "shared secret token")
		concurrency = flag.Int("concurrency", intFromEnv("WS_RELAY_CONCURRENCY", 4), "max concurrent streams")
	)
	flag.Parse()

	if strings.TrimSpace(*controlAddr) == "" || strings.TrimSpace(*dataAddr) == "" {
		log.Fatalf("missing --control-addr/--data-addr (or WS_RELAYD_CONTROL_ADDR/WS_RELAYD_DATA_ADDR)")
	}
	if strings.TrimSpace(*key) == "" || strings.TrimSpace(*token) == "" {
		log.Fatalf("missing --key/--token (or WS_RELAY_KEY/WS_RELAY_TOKEN)")
	}
	if *concurrency <= 0 {
		*concurrency = 1
	}

	sem := make(chan struct{}, *concurrency)

	for {
		if err := runOnce(*controlAddr, *dataAddr, strings.TrimSpace(*key), strings.TrimSpace(*token), sem); err != nil {
			log.Printf("agent: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

type registerMsg struct {
	Key   string `json:"key"`
	Token string `json:"token"`
}

type openMsg struct {
	Type   string `json:"type"`
	Stream string `json:"stream"`
	Port   int    `json:"port"`
}

type dataHello struct {
	Stream string `json:"stream"`
	Key    string `json:"key"`
	Token  string `json:"token"`
}

func runOnce(controlAddr, dataAddr, key, token string, sem chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", controlAddr)
	if err != nil {
		return fmt.Errorf("dial control: %w", err)
	}
	defer conn.Close()

	if err := writeJSONLine(conn, registerMsg{Key: key, Token: token}); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	r := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, rest, err := readLineWithRest(r, 64<<10)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("control closed")
			}
			return fmt.Errorf("control read: %w", err)
		}
		if len(rest) != 0 {
			// Control channel should not carry raw bytes; ignore.
		}

		var msg openMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if strings.TrimSpace(msg.Type) != "open" {
			continue
		}
		stream := strings.TrimSpace(msg.Stream)
		port := msg.Port
		if stream == "" || port <= 0 || port > 65535 {
			continue
		}

		sem <- struct{}{}
		go func(stream string, port int) {
			defer func() { <-sem }()
			if err := handleStream(dataAddr, key, token, stream, port); err != nil {
				log.Printf("stream %s port=%d: %v", stream, port, err)
			}
		}(stream, port)
	}
}

func handleStream(dataAddr, key, token, stream string, port int) error {
	// Connect to local target.
	targetConn, err := (&net.Dialer{}).Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("dial local 127.0.0.1:%d: %w", port, err)
	}
	defer targetConn.Close()

	// Connect to relay data and bind stream.
	dataConn, err := (&net.Dialer{}).Dial("tcp", dataAddr)
	if err != nil {
		return fmt.Errorf("dial data: %w", err)
	}
	defer dataConn.Close()

	bw := bufio.NewWriterSize(dataConn, 4<<10)
	if err := writeJSONLine(bw, dataHello{Stream: stream, Key: key, Token: token}); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	// Bridge.
	copyErr := make(chan error, 2)
	go func() {
		_, e := io.Copy(dataConn, targetConn)
		if tcp, ok := dataConn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyErr <- e
	}()
	go func() {
		_, e := io.Copy(targetConn, dataConn)
		copyErr <- e
	}()
	<-copyErr
	return nil
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

func intFromEnv(k string, def int) int {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}
