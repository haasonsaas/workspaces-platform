package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"workspaces-platform/internal/netutil"
)

// cmdProxy is designed to be used as an OpenSSH ProxyCommand.
//
// It maps a friendly desktop host (e.g. desk-jonathan) to the backing SSH
// Service name (desktop-jonathan-ssh.<ns>) and then executes:
//   ssh <gateway> -- ws-proxy <serviceHost> <port>
//
// Important: this command must not write to stdout (stdout is the SSH transport).
func cmdProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	var (
		gatewayAlias  = fs.String("gateway", "ws-gateway", "SSH Host alias for the gateway (as in ~/.ssh/config)")
		namespace     = fs.String("namespace", "desktops", "Default desktop namespace")
		hostPrefix    = fs.String("host-prefix", "desk-", "Prefix to strip from the provided host before mapping to a Desktop name")
		clusterDomain = fs.String("cluster-domain", "", "If set, append .svc.<cluster-domain> to the computed service host")
	)
	fs.Parse(args)

	if fs.NArg() < 2 {
		// Note: writes to stderr are ok for ProxyCommand.
		_, _ = fmt.Fprintln(os.Stderr, "usage: wsctl proxy [--gateway ws-gateway] [--namespace desktops] <host> <port>")
		os.Exit(2)
	}

	targetHost := strings.TrimSpace(fs.Arg(0))
	targetPort := strings.TrimSpace(fs.Arg(1))
	if targetHost == "" || targetPort == "" {
		_, _ = fmt.Fprintln(os.Stderr, "proxy: host/port required")
		os.Exit(2)
	}
	if p, err := strconv.Atoi(targetPort); err != nil || p <= 0 || p > 65535 {
		_, _ = fmt.Fprintf(os.Stderr, "proxy: invalid port %q\n", targetPort)
		os.Exit(2)
	}

	serviceHost, err := resolveDesktopServiceHost(targetHost, strings.TrimSpace(*namespace), strings.TrimSpace(*hostPrefix), strings.TrimSpace(*clusterDomain))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "proxy: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command("ssh",
		"-T",
		"-o", "BatchMode=yes",
		strings.TrimSpace(*gatewayAlias),
		"--",
		"ws-proxy",
		serviceHost,
		targetPort,
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		_, _ = fmt.Fprintf(os.Stderr, "proxy: ssh failed: %v\n", err)
		os.Exit(1)
	}
}

// resolveDesktopServiceHost maps a user-friendly desktop host to a Service host
// that ws-proxy can parse as <service>.<namespace>...
//
// Supported forms:
// - desk-<desktop>
// - desk-<desktop>.<namespace>
// - <desktop>.<namespace> (if hostPrefix is "")
// - desktop-<desktop>-ssh.<namespace>[.<suffix>...] (pass-through service/ns)
// - <service>.<namespace>[.<suffix>...] (pass-through service/ns)
func resolveDesktopServiceHost(host, defaultNamespace, hostPrefix, clusterDomain string) (string, error) {
	h := strings.TrimSpace(host)
	if h == "" {
		return "", fmt.Errorf("empty host")
	}
	if strings.ContainsAny(h, " \t\r\n") {
		return "", fmt.Errorf("host contains whitespace")
	}

	defNS := strings.TrimSpace(defaultNamespace)
	if defNS == "" {
		defNS = "desktops"
	}
	cd := normalizeClusterDomain(clusterDomain)

	parts := strings.Split(h, ".")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		first := parts[0]
		ns := parts[1]

		// If it looks like our SSH service name, treat it as a service host.
		// Otherwise treat it as a friendly desktop name (optionally with a prefix).
		if strings.HasPrefix(first, "desktop-") && strings.HasSuffix(first, "-ssh") {
			out := first + "." + ns
			if cd != "" {
				out = out + ".svc." + cd
			}
			if err := netutil.ValidateExactHostname(out); err != nil {
				return "", fmt.Errorf("invalid service host %q: %w", out, err)
			}
			return out, nil
		}

		name := first
		if hostPrefix != "" && strings.HasPrefix(name, hostPrefix) {
			name = strings.TrimPrefix(name, hostPrefix)
		}
		name = strings.Trim(name, ".")
		if name == "" {
			return "", fmt.Errorf("empty desktop name")
		}

		svc := "desktop-" + name + "-ssh"
		out := svc + "." + ns
		if cd != "" {
			out = out + ".svc." + cd
		}
		if err := netutil.ValidateExactHostname(out); err != nil {
			return "", fmt.Errorf("invalid computed service host %q: %w", out, err)
		}
		return out, nil
	}

	// Friendly host: derive from desktop name and default namespace.
	name := parts[0]
	if hostPrefix != "" && strings.HasPrefix(name, hostPrefix) {
		name = strings.TrimPrefix(name, hostPrefix)
	}
	name = strings.Trim(name, ".")
	if name == "" {
		return "", fmt.Errorf("empty desktop name")
	}

	svc := name
	if !(strings.HasPrefix(name, "desktop-") && strings.HasSuffix(name, "-ssh")) {
		svc = "desktop-" + name + "-ssh"
	}

	out := svc + "." + defNS
	if cd != "" {
		out = out + ".svc." + cd
	}
	if err := netutil.ValidateExactHostname(out); err != nil {
		return "", fmt.Errorf("invalid computed service host %q: %w", out, err)
	}
	return out, nil
}
