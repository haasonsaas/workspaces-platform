package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

type fileAuditRecord struct {
	Seq      int64           `json:"seq"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
	Event    json.RawMessage `json:"event"`
}

type checkpointRecord struct {
	TS        string `json:"ts"`
	Component string `json:"component"`
	Stream    string `json:"stream"`
	File      string `json:"file"`
	Seq       int64  `json:"seq"`
	Hash      string `json:"hash"`
	Sig       string `json:"sig,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		cmdVerify(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `auditctl: verify workspaces-platform audit file sink chains

Usage:
  auditctl verify --events <events.jsonl> [--checkpoints <checkpoints.jsonl>] [--hmac-key <hex>]

Notes:
  - Validates the SHA256 hash chain for events records.
  - If checkpoints are provided, validates checkpoint hashes and (optionally) HMAC signatures.
`)
}

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	eventsPath := fs.String("events", "", "Path to events-<stream>-<date>.jsonl")
	checkpointsPath := fs.String("checkpoints", "", "Path to checkpoints-<stream>-<date>.jsonl (optional)")
	hmacKeyHex := fs.String("hmac-key", "", "Hex-encoded HMAC key used to sign checkpoints (optional)")
	fs.Parse(args)

	if strings.TrimSpace(*eventsPath) == "" {
		fs.Usage()
		os.Exit(2)
	}

	var hmacKey []byte
	if strings.TrimSpace(*hmacKeyHex) != "" {
		k, err := hex.DecodeString(strings.TrimSpace(*hmacKeyHex))
		dieIf(err)
		if len(k) < 16 {
			die("hmac-key must be >=16 bytes")
		}
		hmacKey = k
	}

	wantCheckpoints := map[int64]checkpointRecord{}
	if strings.TrimSpace(*checkpointsPath) != "" {
		cps, err := readCheckpoints(*checkpointsPath)
		dieIf(err)
		for _, cp := range cps {
			if cp.Seq <= 0 {
				continue
			}
			wantCheckpoints[cp.Seq] = cp
		}
	}

	seq, hashHex, err := verifyEvents(*eventsPath, wantCheckpoints, hmacKey)
	dieIf(err)

	fmt.Printf("ok seq=%d hash=%s\n", seq, hashHex)
}

func verifyEvents(path string, checkpoints map[int64]checkpointRecord, hmacKey []byte) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()

	prevHash := make([]byte, 32) // zero seed for a new file
	prevSeq := int64(0)

	sc := bufio.NewScanner(f)
	// Allow reasonably large events; still bounded.
	sc.Buffer(make([]byte, 64<<10), 4<<20)

	for sc.Scan() {
		line := bytesTrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec fileAuditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return prevSeq, hex.EncodeToString(prevHash), fmt.Errorf("parse record: %w", err)
		}
		if rec.Seq != prevSeq+1 {
			return prevSeq, hex.EncodeToString(prevHash), fmt.Errorf("seq mismatch: got %d want %d", rec.Seq, prevSeq+1)
		}

		wantPrev := hex.EncodeToString(prevHash)
		if strings.TrimSpace(rec.PrevHash) != wantPrev {
			return prevSeq, hex.EncodeToString(prevHash), fmt.Errorf("prev_hash mismatch at seq %d: got %s want %s", rec.Seq, strings.TrimSpace(rec.PrevHash), wantPrev)
		}

		sum := sha256.Sum256(append(prevHash, rec.Event...))
		gotHash := strings.TrimSpace(rec.Hash)
		wantHash := hex.EncodeToString(sum[:])
		if gotHash != wantHash {
			return prevSeq, hex.EncodeToString(prevHash), fmt.Errorf("hash mismatch at seq %d: got %s want %s", rec.Seq, gotHash, wantHash)
		}

		prevSeq = rec.Seq
		copy(prevHash, sum[:])

		if cp, ok := checkpoints[rec.Seq]; ok {
			if strings.TrimSpace(cp.Hash) != wantHash {
				return prevSeq, hex.EncodeToString(prevHash), fmt.Errorf("checkpoint hash mismatch at seq %d: got %s want %s", rec.Seq, strings.TrimSpace(cp.Hash), wantHash)
			}
			if cp.Sig != "" && len(hmacKey) != 0 {
				msg := fmt.Sprintf("%s|%s|%s|%d|%s|%s", strings.TrimSpace(cp.Component), strings.TrimSpace(cp.Stream), strings.TrimSpace(cp.File), cp.Seq, strings.TrimSpace(cp.Hash), strings.TrimSpace(cp.TS))
				mac := hmac.New(sha256.New, hmacKey)
				_, _ = mac.Write([]byte(msg))
				wantSig := hex.EncodeToString(mac.Sum(nil))
				if !hmac.Equal([]byte(strings.TrimSpace(cp.Sig)), []byte(wantSig)) {
					return prevSeq, hex.EncodeToString(prevHash), fmt.Errorf("checkpoint sig mismatch at seq %d", rec.Seq)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return prevSeq, hex.EncodeToString(prevHash), err
	}
	if prevSeq == 0 {
		return 0, "", errors.New("no records found")
	}
	return prevSeq, hex.EncodeToString(prevHash), nil
}

func readCheckpoints(path string) ([]checkpointRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := []checkpointRecord{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytesTrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var cp checkpointRecord
		if err := json.Unmarshal(line, &cp); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func bytesTrimSpace(b []byte) []byte {
	// Minimal whitespace trim to avoid importing bytes for one call.
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	j := len(b) - 1
	for j >= i && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
		j--
	}
	if j < i {
		return nil
	}
	return b[i : j+1]
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func dieIf(err error) {
	if err == nil {
		return
	}
	die(err.Error())
}
