package main

import "testing"

func TestResolveDesktopServiceHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ns   string
		pre  string
		cd   string
		want string
	}{
		{name: "prefix", in: "desk-jonathan", ns: "desktops", pre: "desk-", want: "desktop-jonathan-ssh.desktops"},
		{name: "dot_namespace", in: "desk-jonathan.desktops", ns: "desktops", pre: "desk-", want: "desktop-jonathan-ssh.desktops"},
		{name: "dot_no_prefix", in: "jonathan.desktops", ns: "desktops", pre: "", want: "desktop-jonathan-ssh.desktops"},
		{name: "service_pass_through", in: "desktop-jonathan-ssh.desktops.svc.cluster.local", ns: "desktops", pre: "desk-", want: "desktop-jonathan-ssh.desktops"},
		{name: "cluster_domain_append", in: "desk-jonathan", ns: "desktops", pre: "desk-", cd: "cluster.local", want: "desktop-jonathan-ssh.desktops.svc.cluster.local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDesktopServiceHost(tc.in, tc.ns, tc.pre, tc.cd)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

