package main

import (
	"strings"
	"testing"
)

// Job 009 spec D9.4: the open-UI decision across its four
// combinations, the firewall argv builders, and the icon path
// in computePaths.
func TestShouldOpenUI(t *testing.T) {
	cases := []struct {
		name       string
		isAdminNow bool
		args       []string
		code       int
		want       bool
	}{
		{"relay succeeded -> parent opens", false, nil, 0, true},
		{"relay/failed install -> no open", false, nil, 1, false},
		{"direct admin, install ok -> opens", true, nil, 0, true},
		{"direct admin, install failed -> no open", true, nil, 1, false},
		{"elevated child never opens (even on success)", true, []string{"--elevated"}, 0, false},
		{"elevated child never opens (relay args)", false, []string{"--elevated"}, 0, false},
	}
	for _, c := range cases {
		if got := shouldOpenUI(c.isAdminNow, c.args, c.code); got != c.want {
			t.Errorf("%s: shouldOpenUI(%v, %v, %d) = %v, want %v",
				c.name, c.isAdminNow, c.args, c.code, got, c.want)
		}
	}
}

func TestFirewallCommands(t *testing.T) {
	p := computePaths(`C:\Users\X\AppData\Local`, `C:\Users\X\AppData\Roaming`)
	adds := firewallAddCommands(p.exe)
	if len(adds) != 2 {
		t.Fatalf("firewallAddCommands = %d rules, want 2", len(adds))
	}
	join := func(a []string) string { return strings.Join(a, " ") }
	tcp, udp := join(adds[0]), join(adds[1])
	if !strings.Contains(tcp, "name="+firewallRuleTCP) || !strings.Contains(tcp, "protocol=TCP") ||
		!strings.Contains(tcp, "localport=22000") || !strings.Contains(tcp, "profile=private") ||
		!strings.Contains(tcp, "program="+p.exe) || !strings.Contains(tcp, "dir=in") {
		t.Errorf("TCP rule malformed: %s", tcp)
	}
	if !strings.Contains(udp, "name="+firewallRuleUDP) || !strings.Contains(udp, "protocol=UDP") ||
		!strings.Contains(udp, "localport=5353") || !strings.Contains(udp, "profile=private") {
		t.Errorf("UDP rule malformed: %s", udp)
	}
	dels := firewallDeleteCommands()
	if len(dels) != 2 || !strings.Contains(join(dels[0]), "name="+firewallRuleTCP) ||
		!strings.Contains(join(dels[1]), "name="+firewallRuleUDP) {
		t.Errorf("firewallDeleteCommands = %v", dels)
	}
	if !strings.HasSuffix(p.iconPath, "filesync.ico") || !strings.HasPrefix(p.iconPath, p.installDir) {
		t.Errorf("iconPath = %q, want <installDir>/filesync.ico", p.iconPath)
	}
}

func TestEmbeddedIconIsRealICO(t *testing.T) {
	if len(icon) < 10<<10 {
		t.Fatalf("embedded icon is %d bytes, want a real multi-size ICO (>10KiB)", len(icon))
	}
	if string(icon[:4]) != "\x00\x00\x01\x00" {
		t.Fatalf("embedded icon magic = % x, want ICO 00 00 01 00", icon[:4])
	}
}
