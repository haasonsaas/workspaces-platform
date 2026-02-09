package main

import "testing"

func TestParseNetgrantApproveCommand_ScansLines(t *testing.T) {
	cmd, ok := parseNetgrantApproveCommand("hello\n`/netgrant approve agents/netgrant-abc ttl=1800`\nthanks")
	if !ok {
		t.Fatalf("expected ok")
	}
	if cmd.Namespace != "agents" || cmd.Name != "netgrant-abc" {
		t.Fatalf("unexpected target: %#v", cmd)
	}
	if cmd.TTLSeconds == nil || *cmd.TTLSeconds != 1800 {
		t.Fatalf("unexpected ttl: %#v", cmd.TTLSeconds)
	}
}

func TestParseAgentRunCommand(t *testing.T) {
	cmd, ok := parseAgentRunCommand("please run\n/agent run profile=restricted ttl=3600\n")
	if !ok {
		t.Fatalf("expected ok")
	}
	if cmd.PolicyProfile != "restricted" {
		t.Fatalf("unexpected profile: %q", cmd.PolicyProfile)
	}
	if cmd.TTLSeconds == nil || *cmd.TTLSeconds != 3600 {
		t.Fatalf("unexpected ttl: %#v", cmd.TTLSeconds)
	}
}
