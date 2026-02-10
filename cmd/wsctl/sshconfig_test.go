package main

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

func TestSplitOnSSHConfigSection_NoSection(t *testing.T) {
	in := []byte("Host foo\n  HostName example.com\n")
	before, section, after, found, err := splitOnSSHConfigSection(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if found {
		t.Fatalf("expected found=false")
	}
	if string(before) != string(in) {
		t.Fatalf("before mismatch: %q", string(before))
	}
	if section != nil || after != nil {
		t.Fatalf("expected nil section/after")
	}
}

func TestSplitOnSSHConfigSection_Replace(t *testing.T) {
	in := []byte("Host foo\n  HostName example.com\n\n" + sshConfigSectionBegin + "Host old\n" + sshConfigSectionEnd + "Host bar\n")
	before, section, after, found, err := splitOnSSHConfigSection(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if !strings.Contains(string(section), "Host old") {
		t.Fatalf("expected section content")
	}
	if !strings.Contains(string(before), "Host foo") {
		t.Fatalf("expected before content")
	}
	if !strings.Contains(string(after), "Host bar") {
		t.Fatalf("expected after content")
	}
}

func TestUpsertSSHConfigSection_Appends(t *testing.T) {
	in := []byte("Host foo\n  HostName example.com\n")
	sec := []byte(sshConfigSectionBegin + "Host a\n" + sshConfigSectionEnd)
	out, changed, err := upsertSSHConfigSection(in, sec)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	s := string(out)
	if !strings.Contains(s, "Host foo") || !strings.Contains(s, "Host a") {
		t.Fatalf("unexpected output: %q", s)
	}
}

func TestUpsertSSHConfigSection_Replaces(t *testing.T) {
	in := []byte("Host foo\n\n" + sshConfigSectionBegin + "Host old\n" + sshConfigSectionEnd + "\nHost bar\n")
	sec := []byte(sshConfigSectionBegin + "Host new\n" + sshConfigSectionEnd)
	out, changed, err := upsertSSHConfigSection(in, sec)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	s := string(out)
	if strings.Contains(s, "Host old") {
		t.Fatalf("expected old section removed")
	}
	if !strings.Contains(s, "Host new") {
		t.Fatalf("expected new section present")
	}
}

func TestRenderSSHConfigSection(t *testing.T) {
	opts := sshConfigOptions{
		SkipGateway:  false,
		GatewayAlias: "ws-gateway",
		GatewayHost:  "100.64.0.1",
		GatewayUser:  "ts",
		HostPrefix:   "desk-",
		ProxyCommand: "ssh ws-gateway -- ws-proxy %h %p",
	}
	desktops := []workspacesv1alpha1.Desktop{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "desktops"}, Spec: workspacesv1alpha1.DesktopSpec{User: "jonathan"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "desktops"}, Spec: workspacesv1alpha1.DesktopSpec{User: "dev"}},
	}

	sec, err := renderSSHConfigSection(opts, desktops)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(sec)
	if !strings.Contains(s, "Host ws-gateway") || !strings.Contains(s, "HostName 100.64.0.1") {
		t.Fatalf("missing gateway block: %q", s)
	}
	if !strings.Contains(s, "Host desk-a") || !strings.Contains(s, "HostName desktop-a-ssh.desktops") {
		t.Fatalf("missing desktop a block: %q", s)
	}
	if !strings.Contains(s, "ProxyCommand ssh ws-gateway -- ws-proxy") {
		t.Fatalf("missing proxycommand: %q", s)
	}
}
