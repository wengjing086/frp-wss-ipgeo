// Copyright 2026 The frp Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ipgeo adds a "(province city isp)" suffix to client addresses shown
// by frps, restricted to mainland China addresses.
//
// Geolocation is resolved through public online services (see online.go for the
// provider list and the rationale). Offline databases cannot keep up with
// Chinese allocations, notably IPv6, where city level data is frequently wrong,
// and shipping one would bloat the binary for no benefit. Results are cached in
// memory and, when persistence is enabled, on disk, so only the first sighting
// of an address costs a request.
package ipgeo

import (
	"net"
	"strings"
)

// Format returns the IP (or host:port) as-is when no Chinese geolocation is
// known, and appends " (location isp)" otherwise.
func Format(addr string) string {
	geo := Lookup(addr)
	if geo == "" {
		return addr
	}
	return addr + " (" + geo + ")"
}

// Lookup returns the geolocation description of the given IP in the form
// "province city isp" (e.g. "浙江 杭州 阿里云"). It returns an empty string
// when the IP is private/reserved, not a Chinese address, or unknown.
//
// The online service is queried because it carries authoritative and freshly
// updated allocations, especially for Chinese IPv6. Results are cached, so only
// the first sighting of an address costs a request; after that the answer is
// served from cache.
func Lookup(ipStr string) string {
	ip := parseIP(ipStr)
	if ip == nil || isPrivate(ip) {
		return ""
	}
	if onlineEnabled() {
		if value, err := lookupOnline(ip.String()); err == nil {
			return value
		}
	}
	return ""
}

// parseIP strips an optional port and returns a canonical net.IP, or nil when
// the input is not a valid IP literal.
func parseIP(addr string) net.IP {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	// Strip the port if present. IPv6 addresses are wrapped in brackets.
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4
	}
	return ip.To16()
}

// isPrivate reports whether the IP is a loopback, private, link-local,
// multicast, unspecified, CGNAT or otherwise reserved address that should not
// be decorated with a geolocation suffix.
func isPrivate(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		// Carrier grade NAT 100.64.0.0/10.
		if ipv4[0] == 100 && ipv4[1] >= 64 && ipv4[1] <= 127 {
			return true
		}
		switch ipv4[0] {
		case 0: // 0.0.0.0/8
			return true
		case 169: // 169.254.0.0/16 link-local
			return ipv4[1] == 254
		case 198: // 198.18.0.0/15 benchmarking
			return ipv4[1] == 18 || ipv4[1] == 19
		}
		return false
	}
	// 2001:db8::/32 is reserved for documentation.
	if len(ip) == 16 && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return true
	}
	return false
}
