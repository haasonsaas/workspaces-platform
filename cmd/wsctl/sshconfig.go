package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"sigs.k8s.io/controller-runtime/pkg/client"
	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

const (
	sshConfigSectionBegin = "# BEGIN workspaces-platform (managed by wsctl ssh-config)\n"
	sshConfigSectionEnd   = "# END workspaces-platform\n"
)

type sshConfigOptions struct {
	FilePath      string
	DryRun        bool
	Mode          string
	Connectivity  string
	SkipGateway   bool
	GatewayAlias  string
	GatewayHost   string
	GatewayUser   string
	Namespace     string
	HostPrefix    string
	ClusterDomain string
	DesktopUser   string
	WsctlBinary   string
	ProxyCommand  string
}

func cmdSSHConfig(args []string) {
	fs := flag.NewFlagSet("ssh-config", flag.ExitOnError)
	var (
		filePath      = fs.String("file", "~/.ssh/config", "Path to ssh config file")
		dryRun        = fs.Bool("dry-run", false, "Print a diff; do not write changes")
		mode          = fs.String("mode", "pattern", "Mode: pattern (Coder-style) or list (enumerate Desktops via kubeconfig)")
		connectivity  = fs.String("connectivity", "portforward", "Connectivity: portforward (ws-proxy) or relay (ws-relayd)")
		skipGateway   = fs.Bool("skip-gateway", false, "Do not manage the gateway Host entry")
		gatewayAlias  = fs.String("gateway-alias", "ws-gateway", "SSH Host alias for the gateway")
		gatewayHost   = fs.String("gateway-hostname", "", "Gateway hostname/IP (required unless --skip-gateway)")
		gatewayUser   = fs.String("gateway-user", "", "Gateway SSH user (optional)")
		namespace     = fs.String("namespace", "desktops", "Desktop namespace")
		hostPrefix    = fs.String("host-prefix", "desk-", "Desktop Host prefix/glob (pattern mode uses Host <prefix>*)")
		clusterDomain = fs.String("cluster-domain", "", "If set, use <svc>.<ns>.svc.<cluster-domain> for HostName (otherwise <svc>.<ns>)")
		desktopUser   = fs.String("desktop-user", os.Getenv("USER"), "Desktop SSH username (pattern mode only)")
		wsctlBinary   = fs.String("wsctl-binary", "", "Path to wsctl binary to embed in ProxyCommand (pattern mode only; default: this executable)")
		proxyCommand  = fs.String("proxy-command", "", "Override ProxyCommand (advanced)")
	)
	fs.Parse(args)

	opts := sshConfigOptions{
		FilePath:      strings.TrimSpace(*filePath),
		DryRun:        *dryRun,
		Mode:          strings.ToLower(strings.TrimSpace(*mode)),
		Connectivity:  strings.ToLower(strings.TrimSpace(*connectivity)),
		SkipGateway:   *skipGateway,
		GatewayAlias:  strings.TrimSpace(*gatewayAlias),
		GatewayHost:   strings.TrimSpace(*gatewayHost),
		GatewayUser:   strings.TrimSpace(*gatewayUser),
		Namespace:     strings.TrimSpace(*namespace),
		HostPrefix:    strings.TrimSpace(*hostPrefix),
		ClusterDomain: strings.TrimSpace(*clusterDomain),
		DesktopUser:   strings.TrimSpace(*desktopUser),
		WsctlBinary:   strings.TrimSpace(*wsctlBinary),
		ProxyCommand:  strings.TrimSpace(*proxyCommand),
	}

	if opts.FilePath == "" {
		die("missing --file")
	}
	opts.FilePath = expandHomePath(opts.FilePath)
	switch opts.Mode {
	case "pattern", "list":
	default:
		die("invalid --mode (expected pattern|list)")
	}
	switch opts.Connectivity {
	case "", "portforward", "relay":
	default:
		die("invalid --connectivity (expected portforward|relay)")
	}
	if opts.GatewayAlias == "" && !opts.SkipGateway {
		die("missing --gateway-alias")
	}
	if opts.Namespace == "" {
		die("missing --namespace")
	}
	if !opts.SkipGateway && opts.GatewayHost == "" {
		die("missing --gateway-hostname (or set --skip-gateway)")
	}
	if opts.ProxyCommand == "" && opts.Mode == "list" {
		// ProxyCommand runs on the user's machine and executes on the gateway.
		// ws-proxy lives on the gateway and uses K8s port-forward to reach the desktop pod.
		opts.ProxyCommand = fmt.Sprintf("ssh %s -- ws-proxy %%h %%p", opts.GatewayAlias)
	}
	if opts.ProxyCommand == "" && opts.Mode == "pattern" {
		if opts.HostPrefix == "" {
			die("pattern mode requires --host-prefix (e.g. desk-)")
		}
		if opts.DesktopUser == "" {
			die("pattern mode requires --desktop-user (or set USER)")
		}

		switch opts.Connectivity {
		case "", "portforward":
			wsctlBin := opts.WsctlBinary
			if wsctlBin == "" {
				if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
					wsctlBin = exe
				} else {
					wsctlBin = "wsctl"
				}
			}
			opts.WsctlBinary = wsctlBin
			opts.ProxyCommand = fmt.Sprintf("%s proxy --gateway %s --namespace %s --host-prefix %s %%h %%p",
				shellWord(wsctlBin),
				shellWord(opts.GatewayAlias),
				shellWord(opts.Namespace),
				shellWord(opts.HostPrefix),
			)
		case "relay":
			// Relay mode assumes ws-relayd runs on the gateway host and ws-relay is installed there.
			// This keeps the gateway free of Kubernetes API credentials.
			opts.ProxyCommand = fmt.Sprintf("ssh %s -- ws-relay proxy --namespace %s --host-prefix %s %%h %%p",
				shellWord(opts.GatewayAlias),
				shellWord(opts.Namespace),
				shellWord(opts.HostPrefix),
			)
		default:
			die("invalid connectivity")
		}
	}

	var desktops []workspacesv1alpha1.Desktop
	if opts.Mode == "list" {
		k, err := newK8sClient()
		dieIf(err)

		var list workspacesv1alpha1.DesktopList
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		dieIf(k.List(ctx, &list, client.InNamespace(opts.Namespace)))

		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		desktops = list.Items
	}

	section, err := renderSSHConfigSection(opts, desktops)
	dieIf(err)

	oldBytes, oldMode, err := readSSHConfigFile(opts.FilePath)
	dieIf(err)

	newBytes, changed, err := upsertSSHConfigSection(oldBytes, section)
	dieIf(err)

	if !changed {
		fmt.Printf("no changes to %s\n", opts.FilePath)
		return
	}

	if opts.DryRun {
		fmt.Print(unifiedDiffString(opts.FilePath, oldBytes, newBytes))
		return
	}

	// Best-effort safety net: backup the old file if it existed.
	if len(oldBytes) != 0 {
		backup := opts.FilePath + ".workspaces-platform.bak"
		_ = os.WriteFile(backup, oldBytes, oldMode)
	}

	dieIf(writeSSHConfigFile(opts.FilePath, newBytes, oldMode))
	fmt.Printf("updated %s\n", opts.FilePath)
}

func renderSSHConfigSection(opts sshConfigOptions, desktops []workspacesv1alpha1.Desktop) ([]byte, error) {
	var b strings.Builder
	b.WriteString(sshConfigSectionBegin)
	b.WriteString("# This section is generated; re-run `wsctl ssh-config` to update.\n\n")

	if !opts.SkipGateway {
		b.WriteString("Host " + opts.GatewayAlias + "\n")
		b.WriteString("  HostName " + opts.GatewayHost + "\n")
		if strings.TrimSpace(opts.GatewayUser) != "" {
			b.WriteString("  User " + strings.TrimSpace(opts.GatewayUser) + "\n")
		}
		b.WriteString("\n")
	}

	switch opts.Mode {
	case "pattern":
		pat := opts.HostPrefix + "*"
		b.WriteString("Host " + pat + "\n")
		b.WriteString("  User " + strings.TrimSpace(opts.DesktopUser) + "\n")
		b.WriteString("  ProxyCommand " + opts.ProxyCommand + "\n")
		b.WriteString("  StrictHostKeyChecking accept-new\n")
		b.WriteString("\n")

	case "list":
		cd := normalizeClusterDomain(opts.ClusterDomain)
		for _, d := range desktops {
			user := strings.TrimSpace(d.Spec.User)
			if user == "" {
				// Skip malformed objects (operator would also treat these as non-runnable).
				continue
			}
			alias := opts.HostPrefix + d.Name

			svc := fmt.Sprintf("desktop-%s-ssh", d.Name)
			host := svc + "." + d.Namespace
			if cd != "" {
				host = host + ".svc." + cd
			}

			b.WriteString("Host " + alias + "\n")
			b.WriteString("  HostName " + host + "\n")
			b.WriteString("  User " + user + "\n")
			b.WriteString("  ProxyCommand " + opts.ProxyCommand + "\n")
			b.WriteString("  StrictHostKeyChecking accept-new\n")
			b.WriteString("\n")
		}
	default:
		return nil, fmt.Errorf("invalid mode %q", opts.Mode)
	}

	b.WriteString(sshConfigSectionEnd)
	return []byte(b.String()), nil
}

func normalizeClusterDomain(in string) string {
	s := strings.TrimSpace(in)
	s = strings.TrimPrefix(s, ".")
	s = strings.TrimSuffix(s, ".")
	return s
}

func shellWord(s string) string {
	// Best-effort: OpenSSH executes ProxyCommand using the user's shell.
	// Avoid being clever; just quote things with spaces.
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\r\n") {
		s = strings.ReplaceAll(s, "'", "'\\''")
		return "'" + s + "'"
	}
	return s
}

func expandHomePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func readSSHConfigFile(path string) ([]byte, os.FileMode, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte{}, 0o600, nil
		}
		return nil, 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return b, fi.Mode().Perm(), nil
}

func writeSSHConfigFile(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o600
	}
	return os.WriteFile(path, b, mode)
}

func upsertSSHConfigSection(existing, section []byte) ([]byte, bool, error) {
	before, _, after, found, err := splitOnSSHConfigSection(existing)
	if err != nil {
		return nil, false, err
	}

	var buf bytes.Buffer
	buf.Write(before)
	if len(before) > 0 && !bytes.HasSuffix(before, []byte("\n")) {
		buf.WriteString("\n")
	}
	buf.Write(section)
	if found && len(after) > 0 && !bytes.HasPrefix(after, []byte("\n")) {
		// Preserve a blank line separation if the original section had one.
		buf.WriteString("\n")
	}
	buf.Write(after)

	out := buf.Bytes()
	if bytes.Equal(existing, out) {
		return out, false, nil
	}
	return out, true, nil
}

func splitOnSSHConfigSection(in []byte) (before, section, after []byte, found bool, err error) {
	s := string(in)
	beginIdx := strings.Index(s, strings.TrimSuffix(sshConfigSectionBegin, "\n"))
	if beginIdx < 0 {
		return in, nil, nil, false, nil
	}
	endMarker := strings.TrimSuffix(sshConfigSectionEnd, "\n")
	endIdx := strings.Index(s[beginIdx:], endMarker)
	if endIdx < 0 {
		return nil, nil, nil, false, fmt.Errorf("ssh config section begin marker found but end marker missing")
	}
	endIdx = beginIdx + endIdx + len(endMarker)

	// Include trailing newline after the end marker if present.
	if endIdx < len(s) && s[endIdx] == '\n' {
		endIdx++
	}

	return []byte(s[:beginIdx]), []byte(s[beginIdx:endIdx]), []byte(s[endIdx:]), true, nil
}

func unifiedDiffString(path string, oldBytes, newBytes []byte) string {
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(oldBytes)),
		B:        difflib.SplitLines(string(newBytes)),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
	}
	s, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		// Fall back to the full file if diffing fails (shouldn't).
		return string(newBytes)
	}
	return s
}
