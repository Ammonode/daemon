package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ammonode/daemon/internal/remote"
)

func cfg() remote.ServerConfiguration {
	c := remote.ServerConfiguration{Environment: map[string]string{"MAX_PLAYERS": "32"}}
	c.Allocations.Default = remote.Allocation{IP: "0.0.0.0", Port: 27015}
	return c
}

func TestXMLParser(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.xml")
	os.WriteFile(p, []byte(`<?xml version="1.0"?><ServerSettings><Port>1</Port><Name>Keep me</Name></ServerSettings>`), 0o644)
	f := remote.ConfigFile{File: "config.xml", Parser: "xml", Replace: []remote.ConfigReplacement{
		{Match: "ServerSettings.Port", Value: "{{server.build.default.port}}"},
		{Match: "ServerSettings.Limits.MaxPlayers", Value: "{{server.build.env.MAX_PLAYERS}}"},
		{Match: "ServerSettings.Network[@bind]", Value: "{{server.build.default.ip}}"},
	}}
	if err := applyOne(p, f, cfg()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	out := string(b)
	for _, want := range []string{"<Port>27015</Port>", "<Name>Keep me</Name>", "<MaxPlayers>32</MaxPlayers>", `bind="0.0.0.0"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPropertiesParser(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "server.properties")
	os.WriteFile(p, []byte("motd=Hello\nserver-port=1\n"), 0o644)
	f := remote.ConfigFile{File: "server.properties", Parser: "properties", Replace: []remote.ConfigReplacement{
		{Match: "server-port", Value: "{{server.build.default.port}}"},
		{Match: "max-players", Value: "{{server.build.env.MAX_PLAYERS}}"},
	}}
	if err := applyOne(p, f, cfg()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if got := string(b); got != "motd=Hello\nserver-port=27015\nmax-players=32\n" {
		t.Fatalf("unexpected output:\n%s", got)
	}
}
