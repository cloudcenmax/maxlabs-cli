package webfetch

import "net"

// blockedRanges are the address spaces this tool will not connect to.
//
// Every one of them is somewhere a model has no business reaching on its own:
// the machine it runs on, the network behind it, and the metadata services that
// hand out credentials to anything that asks.
//
// Link-local matters most. On a cloud host, 169.254.169.254 returns instance
// credentials to any process that can open a socket, and it is reachable from
// every instance without authentication.
var blockedRanges = []*net.IPNet{
	// Loopback: the machine itself. The gateway runs on 127.0.0.1.
	mustParseCIDR("127.0.0.0/8"),
	mustParseCIDR("::1/128"),

	// Private networks: the machines next to it.
	mustParseCIDR("10.0.0.0/8"),
	mustParseCIDR("172.16.0.0/12"),
	mustParseCIDR("192.168.0.0/16"),
	mustParseCIDR("fc00::/7"),

	// Link-local, including the cloud metadata service.
	mustParseCIDR("169.254.0.0/16"),
	mustParseCIDR("fe80::/10"),

	// Carrier-grade NAT: not private in the RFC 1918 sense, and not the public
	// internet either.
	mustParseCIDR("100.64.0.0/10"),

	// "This network" and the unspecified address.
	mustParseCIDR("0.0.0.0/8"),
	mustParseCIDR("::/128"),

	// Multicast and broadcast are not hosts.
	mustParseCIDR("224.0.0.0/4"),
	mustParseCIDR("240.0.0.0/4"),
	mustParseCIDR("ff00::/8"),

	// Documentation and benchmarking ranges, which should never appear in a
	// real answer and are a common way to test a filter.
	mustParseCIDR("192.0.2.0/24"),
	mustParseCIDR("198.51.100.0/24"),
	mustParseCIDR("203.0.113.0/24"),
}

func mustParseCIDR(cidr string) *net.IPNet {
	_, block, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("webfetch: bad built-in range " + cidr + ": " + err.Error())
	}

	return block
}

// dns64Prefixes are the well-known and local-use NAT64 translation prefixes.
//
// An address under one of these is an IPv4 destination wearing an IPv6
// spelling. Checking only for IPv6 ranges would let 64:ff9b::7f00:1 through as
// a public address and connect to 127.0.0.1.
var dns64Prefixes = []*net.IPNet{
	mustParseCIDR("64:ff9b::/96"),   // well-known, RFC 6052
	mustParseCIDR("64:ff9b:1::/48"), // local-use, RFC 8215
}

// isPublic reports whether an address is one this package will connect to.
//
// Written as a list of refusals rather than of allowances. The set of addresses
// that are not public is small and known; a new one that is forgotten is
// reachable, where an allowlist would merely have been incomplete.
func isPublic(ip net.IP) bool {
	if ip == nil {
		return false
	}

	// An IPv4 address carried as IPv6 must be judged as IPv4, or 127.0.0.1
	// arrives as ::ffff:127.0.0.1 and matches none of the ranges below.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if embedded, ok := translatedIPv4(ip); ok {
		// A NAT64 address is judged by the IPv4 destination inside it, which is
		// the address actually reached.
		return isPublic(embedded)
	}

	for _, block := range blockedRanges {
		if block.Contains(ip) {
			return false
		}
	}

	return true
}

// translatedIPv4 extracts the IPv4 destination from a NAT64 address.
func translatedIPv4(ip net.IP) (net.IP, bool) {
	for _, prefix := range dns64Prefixes {
		if !prefix.Contains(ip) {
			continue
		}

		// RFC 6052 puts the IPv4 address in the last four bytes for the /96
		// prefix, which is the one NAT64 uses in practice.
		if len(ip) == net.IPv6len {
			return net.IP(ip[12:16]), true
		}
	}

	return nil, false
}
