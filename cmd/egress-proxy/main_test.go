package main

import (
	"net"
	"testing"
)

func TestValidateConnectHost(t *testing.T) {
	ok := []string{
		"github.com",
		"api.github.com",
		"example.com.",
	}
	for _, h := range ok {
		if err := validateConnectHost(h); err != nil {
			t.Fatalf("expected %q valid, got %v", h, err)
		}
	}

	bad := []string{
		"192.168.1.1",
		"kubernetes.default.svc",
		"*.example.com",
		"https://example.com",
	}
	for _, h := range bad {
		if err := validateConnectHost(h); err == nil {
			t.Fatalf("expected %q invalid", h)
		}
	}
}

func TestIsAllowedDialIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"1.1.1.1", true},
		{"8.8.8.8", true},
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"169.254.1.1", false},
		{"100.64.0.1", false}, // CGNAT
		{"127.0.0.1", false},
	}

	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", tc.ip)
		}
		if got := isAllowedDialIP(ip); got != tc.want {
			t.Fatalf("isAllowedDialIP(%q)=%v want %v", tc.ip, got, tc.want)
		}
	}
}

