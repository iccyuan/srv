package daemon

import (
	"testing"

	"srv/internal/config"
)

func TestTunnelListenLabel(t *testing.T) {
	prof := &config.Profile{Host: "db.internal"}
	cases := []struct {
		def  *config.TunnelDef
		want string
	}{
		{&config.TunnelDef{Type: "local", Spec: "5432"}, "127.0.0.1:5432"},
		{&config.TunnelDef{Type: "local", Spec: "5432:db:5432"}, "127.0.0.1:5432"},
		{&config.TunnelDef{Type: "remote", Spec: "9000:3000"}, "db.internal:9000"},
	}
	for _, c := range cases {
		got := tunnelListenLabel(c.def, prof)
		if got != c.want {
			t.Errorf("listenLabel(%+v) = %q, want %q", c.def, got, c.want)
		}
	}
}
