package ipgeo

import "testing"

// Online lookups are disabled so these tests never touch the network and do not
// depend on any embedded database. They exercise the pure address handling and
// formatting helpers; the online providers and their page parsers are covered by
// online_test.go.

func TestIsPrivate(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":      true,
		"192.168.1.100":  true,
		"10.0.0.1":       true,
		"172.16.5.4":     true,
		"169.254.1.1":    true,
		"100.64.1.1":     true,
		"::1":            true,
		"fe80::1":        true,
		"fc00::1":        true,
		"2001:db8::1":    true,
		"8.8.8.8":        false,
		"223.5.5.5":      false,
		"2400:3200::1":   false,
	}
	for ip, want := range cases {
		if got := isPrivate(parseIP(ip)); got != want {
			t.Errorf("isPrivate(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestParseIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"127.0.0.1:1234", "127.0.0.1"},
		{"[2400:3200::1]:7000", "2400:3200::1"},
		{"223.5.5.5", "223.5.5.5"},
		{" 8.8.8.8 ", "8.8.8.8"},
		{"bad", ""},
		{"", ""},
	}
	for _, c := range cases {
		ip := parseIP(c.in)
		got := ""
		if ip != nil {
			got = ip.String()
		}
		if got != c.want {
			t.Errorf("parseIP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatPrivateOrForeignUnchanged(t *testing.T) {
	if got := Format("127.0.0.1:1234"); got != "127.0.0.1:1234" {
		t.Errorf("Format(private) = %q, want unchanged", got)
	}
	if got := Format("8.8.8.8"); got != "8.8.8.8" {
		t.Errorf("Format(foreign) = %q, want unchanged", got)
	}
	if got := Format("[::1]:7000"); got != "[::1]:7000" {
		t.Errorf("Format(v6 loopback) = %q, want unchanged", got)
	}
}

func TestFormatLocationParts(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"浙江省", "杭州市", "阿里云"}, "浙江 杭州 阿里云"},
		{[]string{"浙江省", "浙江省"}, "浙江"},
		{[]string{"广西壮族自治区", "桂林市"}, "广西 桂林"},
		{[]string{"浙江省", "杭州市", "杭州市", "联通"}, "浙江 杭州 联通"},
	}
	for _, c := range cases {
		if got := formatLocationParts(c.in); got != c.want {
			t.Errorf("formatLocationParts(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeISP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"中国联通", "联通"},
		{"CHINANET", "电信"},
		{"CHINA UNICOM China169 Backbone", "联通"},
		{"China Mobile Communications Corporation", "移动"},
		{"阿里云", "阿里云"},
		{"AS4134 Chinanet", "电信"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeISP(c.in); got != c.want {
			t.Errorf("normalizeISP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
