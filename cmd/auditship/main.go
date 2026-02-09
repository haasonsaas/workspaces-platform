package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"workspaces-platform/internal/artifacts"
)

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func main() {
	fs := flag.NewFlagSet("auditship", flag.ExitOnError)
	dir := fs.String("dir", strings.TrimSpace(os.Getenv("AUDIT_DIR")), "Audit directory containing events/checkpoints JSONL")
	dryRun := fs.Bool("dry-run", false, "Print actions without uploading")
	prefix := fs.String("env-prefix", "AUDIT_", "Env prefix for S3 config (e.g. AUDIT_ => AUDIT_S3_ENDPOINT, AUDIT_S3_BUCKET, ...)")
	fs.Parse(os.Args[1:])

	if strings.TrimSpace(*dir) == "" {
		fmt.Fprintln(os.Stderr, "missing --dir (or AUDIT_DIR)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := artifacts.NewS3FromEnvWithPrefix(ctx, strings.TrimSpace(*prefix))
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3: %v\n", err)
		os.Exit(1)
	}

	files, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "readdir: %v\n", err)
		os.Exit(1)
	}

	type item struct {
		name   string
		stream string
		date   string
		kind   string // events|checkpoints
	}
	items := []item{}

	for _, de := range files {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		stream, date, kind, ok := parseAuditFilename(name)
		if !ok {
			continue
		}
		items = append(items, item{name: name, stream: stream, date: date, kind: kind})
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "no audit files found")
		os.Exit(1)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].date != items[j].date {
			return items[i].date < items[j].date
		}
		if items[i].stream != items[j].stream {
			return items[i].stream < items[j].stream
		}
		return items[i].name < items[j].name
	})

	for _, it := range items {
		localPath := filepath.Join(*dir, it.name)
		b, err := os.ReadFile(localPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", localPath, err)
			os.Exit(1)
		}

		key := store.Key("audit", it.date, "stream="+it.stream, it.name)
		if *dryRun {
			fmt.Printf("would upload %s -> %s\n", localPath, key)
			continue
		}

		ct := "text/plain; charset=utf-8"
		if err := store.Put(ctx, key, ct, b); err != nil {
			fmt.Fprintf(os.Stderr, "upload %s: %v\n", it.name, err)
			os.Exit(1)
		}
		fmt.Printf("uploaded %s -> %s\n", it.name, key)
	}
}

func parseAuditFilename(name string) (stream string, date string, kind string, ok bool) {
	if strings.HasPrefix(name, "events-") && strings.HasSuffix(name, ".jsonl") {
		stream, date, ok := parseStreamDate(strings.TrimSuffix(strings.TrimPrefix(name, "events-"), ".jsonl"))
		if !ok {
			return "", "", "", false
		}
		return stream, date, "events", true
	}
	if strings.HasPrefix(name, "checkpoints-") && strings.HasSuffix(name, ".jsonl") {
		stream, date, ok := parseStreamDate(strings.TrimSuffix(strings.TrimPrefix(name, "checkpoints-"), ".jsonl"))
		if !ok {
			return "", "", "", false
		}
		return stream, date, "checkpoints", true
	}
	return "", "", "", false
}

func parseStreamDate(stem string) (stream string, date string, ok bool) {
	stem = strings.TrimSpace(stem)
	if len(stem) < 12 {
		return "", "", false
	}
	date = stem[len(stem)-10:]
	if !dateRe.MatchString(date) {
		return "", "", false
	}
	if stem[len(stem)-11] != '-' {
		return "", "", false
	}
	stream = strings.TrimSpace(stem[:len(stem)-11])
	if stream == "" {
		return "", "", false
	}
	return stream, date, true
}
