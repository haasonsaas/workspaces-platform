package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Emitter writes structured audit events. Implementations must be safe for
// concurrent use.
//
// Note: this intentionally does not accept context.Context to keep audit
// plumbing lightweight; callers should include request identifiers explicitly.
type Emitter interface {
	Emit(eventType string, fields map[string]any)
	Close() error
}

type Config struct {
	Sink string // stdout|file

	Stream string // e.g. broker|operator

	Dir string // required for file sink

	CheckpointEveryN        int64
	CheckpointEveryDuration time.Duration

	FsyncOnCheckpoint bool

	// Optional. When set, checkpoints include an HMAC-SHA256 signature.
	HMACKey []byte
}

func NewFromEnv(component string) (Emitter, error) {
	sink := stringsTrimLower(getenv("AUDIT_SINK", "stdout"))
	stream := stringsTrimLower(getenv("AUDIT_STREAM", component))

	switch sink {
	case "", "stdout":
		return &stdoutEmitter{component: component, stream: stream, logger: log.Default()}, nil
	case "file":
		dir := getenv("AUDIT_DIR", "")
		if dir == "" {
			return nil, errors.New("AUDIT_DIR is required when AUDIT_SINK=file")
		}

		checkEveryN := int64(500)
		if raw := getenv("AUDIT_CHECKPOINT_EVERY_N", "500"); raw != "" {
			n, err := parseInt64(raw)
			if err != nil || n <= 0 {
				return nil, errors.New("AUDIT_CHECKPOINT_EVERY_N must be a positive integer")
			}
			checkEveryN = n
		}

		checkEveryDur := 60 * time.Second
		if raw := getenv("AUDIT_CHECKPOINT_EVERY_SECONDS", "60"); raw != "" {
			n, err := parseInt64(raw)
			if err != nil || n <= 0 {
				return nil, errors.New("AUDIT_CHECKPOINT_EVERY_SECONDS must be a positive integer")
			}
			checkEveryDur = time.Duration(n) * time.Second
		}

		fsync := stringsTrimLower(getenv("AUDIT_FSYNC_ON_CHECKPOINT", "false")) == "true"

		var hmacKey []byte
		if raw := strings.TrimSpace(os.Getenv("AUDIT_HMAC_KEY")); raw != "" {
			k, err := hex.DecodeString(raw)
			if err != nil || len(k) < 16 {
				return nil, errors.New("AUDIT_HMAC_KEY must be a hex-encoded key (recommend >=16 bytes)")
			}
			hmacKey = k
		}

		return newFileEmitter(Config{
			Sink:                    sink,
			Stream:                  stream,
			Dir:                     dir,
			CheckpointEveryN:        checkEveryN,
			CheckpointEveryDuration: checkEveryDur,
			FsyncOnCheckpoint:       fsync,
			HMACKey:                 hmacKey,
		}, component)
	default:
		return nil, fmt.Errorf("unknown AUDIT_SINK %q (expected stdout|file)", sink)
	}
}

type stdoutEmitter struct {
	component string
	stream    string
	logger    *log.Logger
}

func (e *stdoutEmitter) Emit(eventType string, fields map[string]any) {
	evt := map[string]any{
		"type":      eventType,
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		"component": e.component,
		"stream":    e.stream,
	}
	for k, v := range fields {
		evt[k] = v
	}
	b, err := json.Marshal(evt)
	if err != nil {
		e.logger.Printf("AUDIT marshal failed: %v", err)
		return
	}
	e.logger.Printf("AUDIT %s", b)
}

func (e *stdoutEmitter) Close() error { return nil }

type fileEmitter struct {
	component string
	stream    string
	dir       string

	checkEveryN   int64
	checkEveryDur time.Duration
	fsync         bool
	hmacKey       []byte

	mu sync.Mutex

	openDate string
	eventsF  *os.File
	checkF   *os.File

	seq      int64
	prevHash [32]byte

	lastCheckpoint time.Time
}

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

func newFileEmitter(cfg Config, component string) (*fileEmitter, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}

	e := &fileEmitter{
		component:     component,
		stream:        cfg.Stream,
		dir:           cfg.Dir,
		checkEveryN:   cfg.CheckpointEveryN,
		checkEveryDur: cfg.CheckpointEveryDuration,
		fsync:         cfg.FsyncOnCheckpoint,
		hmacKey:       cfg.HMACKey,
	}

	// Open today's files and seed chain state.
	if err := e.rotateLocked(time.Now().UTC()); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *fileEmitter) Emit(eventType string, fields map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UTC()
	if err := e.rotateLocked(now); err != nil {
		// Last resort: do not crash the service. Emit to stdout-ish log.
		log.Printf("AUDIT rotate failed: %v", err)
		return
	}

	payload := map[string]any{
		"type":      eventType,
		"ts":        now.Format(time.RFC3339Nano),
		"component": e.component,
		"stream":    e.stream,
	}
	for k, v := range fields {
		payload[k] = v
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("AUDIT marshal failed: %v", err)
		return
	}

	sum := sha256.Sum256(append(e.prevHash[:], payloadBytes...))
	rec := fileAuditRecord{
		Seq:      e.seq + 1,
		PrevHash: hex.EncodeToString(e.prevHash[:]),
		Hash:     hex.EncodeToString(sum[:]),
		Event:    payloadBytes,
	}

	line, err := json.Marshal(rec)
	if err != nil {
		log.Printf("AUDIT marshal record failed: %v", err)
		return
	}
	line = append(line, '\n')

	if _, err := e.eventsF.Write(line); err != nil {
		log.Printf("AUDIT write failed: %v", err)
		return
	}

	e.seq = rec.Seq
	e.prevHash = sum

	// Periodic checkpoint.
	needCheckpoint := false
	if e.checkEveryN > 0 && (e.seq%e.checkEveryN) == 0 {
		needCheckpoint = true
	}
	if e.checkEveryDur > 0 && now.Sub(e.lastCheckpoint) >= e.checkEveryDur {
		needCheckpoint = true
	}
	if needCheckpoint {
		_ = e.writeCheckpointLocked(now)
	}
}

func (e *fileEmitter) writeCheckpointLocked(now time.Time) error {
	cp := checkpointRecord{
		TS:        now.Format(time.RFC3339Nano),
		Component: e.component,
		Stream:    e.stream,
		File:      filepath.Base(e.eventsF.Name()),
		Seq:       e.seq,
		Hash:      hex.EncodeToString(e.prevHash[:]),
	}
	if len(e.hmacKey) != 0 {
		// Sign a stable string form of the checkpoint. (This is not intended to be
		// perfect cryptographic provenance; it's an MVP step before Vault transit.)
		msg := fmt.Sprintf("%s|%s|%s|%d|%s|%s", cp.Component, cp.Stream, cp.File, cp.Seq, cp.Hash, cp.TS)
		mac := hmac.New(sha256.New, e.hmacKey)
		_, _ = mac.Write([]byte(msg))
		cp.Sig = hex.EncodeToString(mac.Sum(nil))
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := e.checkF.Write(b); err != nil {
		return err
	}
	if e.fsync {
		_ = e.eventsF.Sync()
		_ = e.checkF.Sync()
	}
	e.lastCheckpoint = now
	return nil
}

func (e *fileEmitter) rotateLocked(now time.Time) error {
	date := now.Format("2006-01-02")
	if e.openDate == date && e.eventsF != nil && e.checkF != nil {
		return nil
	}

	// Close any previously open files.
	if e.eventsF != nil {
		_ = e.eventsF.Close()
	}
	if e.checkF != nil {
		_ = e.checkF.Close()
	}

	eventsPath := filepath.Join(e.dir, fmt.Sprintf("events-%s-%s.jsonl", e.stream, date))
	checkPath := filepath.Join(e.dir, fmt.Sprintf("checkpoints-%s-%s.jsonl", e.stream, date))

	eventsF, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	checkF, err := os.OpenFile(checkPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = eventsF.Close()
		return err
	}

	// Seed chain from existing events file (if present).
	seq, prevHash, err := readLastAuditState(eventsPath)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = eventsF.Close()
		_ = checkF.Close()
		return err
	}

	e.openDate = date
	e.eventsF = eventsF
	e.checkF = checkF
	e.seq = seq
	e.prevHash = prevHash
	e.lastCheckpoint = time.Time{}
	return nil
}

func (e *fileEmitter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	if e.eventsF != nil {
		errs = append(errs, e.eventsF.Close())
	}
	if e.checkF != nil {
		errs = append(errs, e.checkF.Close())
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func readLastAuditState(eventsPath string) (seq int64, prevHash [32]byte, _ error) {
	line, err := readLastJSONLLine(eventsPath, 4<<20) // 4 MiB tail
	if err != nil {
		return 0, [32]byte{}, err
	}

	var rec fileAuditRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return 0, [32]byte{}, err
	}
	if rec.Seq < 0 {
		return 0, [32]byte{}, errors.New("invalid seq in audit record")
	}
	hb, err := hex.DecodeString(rec.Hash)
	if err != nil || len(hb) != 32 {
		return 0, [32]byte{}, errors.New("invalid hash in audit record")
	}
	copy(prevHash[:], hb)
	return rec.Seq, prevHash, nil
}

func readLastJSONLLine(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, io.EOF
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, io.EOF
	}

	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if start > 0 {
		// Drop partial first line in the tail chunk.
		if idx := bytes.IndexByte(b, '\n'); idx >= 0 {
			b = b[idx+1:]
		} else {
			return nil, io.EOF
		}
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, io.EOF
	}
	if idx := bytes.LastIndexByte(b, '\n'); idx >= 0 {
		b = b[idx+1:]
	}
	return b, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func stringsTrimLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}
