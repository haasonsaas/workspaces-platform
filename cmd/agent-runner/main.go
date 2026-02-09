package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	auditpkg "workspaces-platform/internal/audit"
	"workspaces-platform/internal/redact"
)

func main() {
	script := os.Getenv("WORKSPACES_TASK_SCRIPT")
	if strings.TrimSpace(script) == "" {
		log.Fatalf("WORKSPACES_TASK_SCRIPT is required")
	}

	workdir := strings.TrimSpace(os.Getenv("WORKSPACES_WORKDIR"))
	if workdir == "" {
		workdir = strings.TrimSpace(os.Getenv("WORKSPACES_REPO_DIR"))
	}
	if workdir == "" {
		workdir = "/workspace"
	}

	capBytes := int64(64 << 10) // 64KiB default
	if raw := strings.TrimSpace(os.Getenv("WORKSPACES_LOG_CAP_BYTES")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			log.Fatalf("invalid WORKSPACES_LOG_CAP_BYTES %q", raw)
		}
		capBytes = n
	}

	auditEmitter, err := auditpkg.NewFromEnv("agent-runner")
	if err != nil {
		log.Fatalf("audit: %v", err)
	}
	defer func() { _ = auditEmitter.Close() }()

	red := redact.NewDefault()

	repo := strings.TrimSpace(os.Getenv("WORKSPACES_GITHUB_REPO"))
	pr := strings.TrimSpace(os.Getenv("WORKSPACES_GITHUB_PR_NUMBER"))
	sha := strings.TrimSpace(os.Getenv("WORKSPACES_GITHUB_HEAD_SHA"))

	start := time.Now()
	auditEmitter.Emit("agent.exec.start", map[string]any{
		"workdir": workdir,
		"repo":    repo,
		"pr":      pr,
		"sha":     sha,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", script)
	cmd.Dir = workdir
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalf("stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}

	var (
		stdoutWritten int64
		stderrWritten int64
		stdoutTrunc   bool
		stderrTrunc   bool
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, trunc, _ := copyRedacted(os.Stdout, stdout, red, capBytes)
		stdoutWritten = n
		stdoutTrunc = trunc
	}()
	go func() {
		defer wg.Done()
		n, trunc, _ := copyRedacted(os.Stderr, stderr, red, capBytes)
		stderrWritten = n
		stderrTrunc = trunc
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}

	dur := time.Since(start)
	conclusion := "success"
	if exitCode != 0 {
		conclusion = "failure"
	}
	auditEmitter.Emit("agent.exec.finish", map[string]any{
		"workdir":          workdir,
		"repo":             repo,
		"pr":               pr,
		"sha":              sha,
		"duration_ms":      dur.Milliseconds(),
		"exit_code":        exitCode,
		"conclusion":       conclusion,
		"stdout_bytes":     stdoutWritten,
		"stderr_bytes":     stderrWritten,
		"stdout_truncated": stdoutTrunc,
		"stderr_truncated": stderrTrunc,
	})

	if waitErr != nil {
		// Preserve the exit code for Kubernetes Job status.
		os.Exit(exitCode)
	}
}

func copyRedacted(dst io.Writer, src io.Reader, red *redact.Redactor, capBytes int64) (written int64, truncated bool, _ error) {
	// Drain even after cap is reached to avoid blocking the child process.
	buf := make([]byte, 32<<10)
	var announced bool

	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := red.RedactBytes(buf[:n])

			remain := capBytes - written
			if remain > 0 {
				toWrite := int64(len(chunk))
				if toWrite > remain {
					toWrite = remain
					truncated = true
				}
				if toWrite > 0 {
					if _, werr := dst.Write(chunk[:toWrite]); werr != nil {
						return written, truncated, werr
					}
					written += toWrite
				}
			} else {
				truncated = true
			}

			if truncated && !announced {
				announced = true
				_, _ = fmt.Fprintln(dst, "\n[output truncated]")
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, truncated, nil
			}
			return written, truncated, rerr
		}
	}
}
