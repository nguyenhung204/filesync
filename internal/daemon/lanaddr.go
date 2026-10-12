package daemon

import (
	"net"
	"sort"
	"strings"
)

// lanDialAddrs turns this machine's interface IPs into the
// "host:port" addresses a person can read out to whoever stands
// at the other machine during pairing (job 009 spec D5). Rules:
// IPv4 only, no loopback, no link-local; RFC1918 private
// addresses first (the home/office LAN the user means), then any
// other routable address; at most 3. Pure function over the IP
// list so it is unit-testable with fake addresses.
func lanDialAddrs(ips []net.IP, port string) []string {
	var priv, pub []string
	seen := make(map[string]bool)
	for _, ip := range ips {
		ip4 := ip.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
			continue
		}
		s := net.JoinHostPort(ip4.String(), port)
		if seen[s] {
			continue
		}
		seen[s] = true
		if ip4.IsPrivate() {
			priv = append(priv, s)
		} else {
			pub = append(pub, s)
		}
	}
	out := append(priv, pub...)
	if len(out) > 3 {
		out = out[:3]
	}
	if out == nil {
		return []string{}
	}
	return out
}

// listenPort extracts the port part of a listen address like
// ":22000" or "127.0.0.1:22000"; "" when it cannot be split.
func listenPort(listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// A bare ":22000" splits fine; anything else is not an
		// address a user could read out anyway.
		if strings.HasPrefix(listenAddr, ":") {
			return strings.TrimPrefix(listenAddr, ":")
		}
		return ""
	}
	return port
}

// interfaceIPs is the live input for lanDialAddrs.
func interfaceIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			ips = append(ips, ipNet.IP)
		}
	}
	sort.Slice(ips, func(i, j int) bool { return ips[i].String() < ips[j].String() })
	return ips
}
