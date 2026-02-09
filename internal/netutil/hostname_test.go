package netutil

import "testing"

func TestNormalizeHostname(t *testing.T) {
	if got := NormalizeHostname("  ExAmPle.Com. "); got != "example.com" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeHostname("example.com."); got != "example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateExactHostname(t *testing.T) {
	ok := []string{
		"github.com",
		"api.github.com",
		"crates.io",
		"registry.npmjs.org",
		"minio.workspaces-system.svc.cluster.local",
	}
	for _, h := range ok {
		if err := ValidateExactHostname(h); err != nil {
			t.Fatalf("expected %q valid, got %v", h, err)
		}
	}

	bad := []string{
		"",
		"   ",
		"*.github.com",
		"github.com:443",
		"https://github.com",
		"github.com/",
		"example.com.",
		".example.com",
		"ex..ample.com",
		"192.168.1.1",
		"kubernetes.default.svc",
		"kubernetes.default.svc.cluster.local",
		"bad_label_.example.com",
		"-bad.example.com",
		"bad-.example.com",
	}
	for _, h := range bad {
		if err := ValidateExactHostname(h); err == nil {
			t.Fatalf("expected %q invalid", h)
		}
	}
}

