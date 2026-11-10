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

package ipgeo

import (
	"strings"
	"testing"
	"time"
)

// TestOffline page parsing keeps the layout knowledge in one place. The
// fixtures are the real structure of the result page.
func TestParseIPShudi(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{
			name: "ipv6 province and isp",
			body: `<div class="info"><span>归属地</span><span>中国</span><span>浙江省</span>` +
				`<span>会员查看市区县</span><a>上报纠错</a></div>` +
				`<div><span>运营商</span><span>联通</span></div>`,
			want: "浙江 联通",
			ok:   true,
		},
		{
			name: "ipv4 province city and isp",
			body: `<div><span>归属地</span><span>中国</span><span>江苏省</span><span>南通市</span>` +
				`<a>上报纠错</a><span>运营商</span><span>移动</span><span>iP类型</span><span>数据中心</span></div>`,
			want: "江苏 南通 移动",
			ok:   true,
		},
		{
			name: "municipality without duplicated city",
			body: `<div><span>归属地</span><span>中国</span><span>上海市</span><a>上报纠错</a>` +
				`<span>运营商</span><span>联通</span></div>`,
			want: "上海 联通",
			ok:   true,
		},
		{
			name: "foreign address is not described",
			body: `<div><span>归属地</span><span>美国</span><span>加利福尼亚州</span>` +
				`<span>运营商</span><span>Google</span></div>`,
			want: "",
			ok:   true,
		},
		{
			name: "themed page without result block",
			body: `<html><body><div>全国DNS服务器IP地址大全</div></body></html>`,
			want: "",
			ok:   false,
		},
	}
	for _, c := range cases {
		loc, isp, ok := parseIPShudi([]byte(c.body))
		got := strings.TrimSpace(loc + " " + isp)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestTrimAdministrativeSuffix(t *testing.T) {
	cases := map[string]string{
		"浙江省":      "浙江",
		"上海市":      "上海",
		"内蒙古自治区":   "内蒙古",
		"广西壮族自治区":  "广西",
		"新疆维吾尔自治区": "新疆",
		"宁夏回族自治区":  "宁夏",
		"香港特别行政区":  "香港",
		"杭州市":      "杭州",
		"北京市":      "北京",
		"延边朝鲜族自治州": "延边",
	}
	for in, want := range cases {
		if got := trimAdministrativeSuffix(in); got != want {
			t.Errorf("trimAdministrativeSuffix(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestLookupOnlinePreference documents that IPv6 is located by the Chinese
// service (ipshudi/ip138), never by a global database that misplaces Chinese
// mobile IPv6 allocations in Beijing/Hebei. When ipshudi is reachable it yields
// the correct province; when it is throttled/blocked we fall back to the ISP
// only (geojs) rather than a wrong province.
func TestLookupOnlinePreference(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping online lookup in short mode")
	}
	SetOnlineEnabled(true)
	defer SetOnlineEnabled(false)

	got := Lookup("2408:8740:41ff:9:2::16c")
	if !strings.Contains(got, "联通") {
		t.Errorf("Lookup(reported ipv6) = %q, want it to name 联通", got)
	}
	// The global databases answer Beijing for this address; we must never echo
	// that wrong province when only the ISP is known.
	if strings.Contains(got, "北京") {
		t.Errorf("Lookup(reported ipv6) = %q, must not show the wrong province 北京", got)
	}
	t.Logf("2408:8740:41ff:9:2::16c => %q", got)
	// IPv4 must carry city and ISP from the authoritative answer.
	if got := Lookup("58.247.214.47"); got != "上海 联通" {
		t.Errorf("Lookup(ipv4) = %q, want %q", got, "上海 联通")
	}
}

// TestOnlineLookup hits the live service, so it only runs in the default mode.
// Run `go test -short` to skip it.
func TestOnlineLookup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping online lookup in short mode")
	}
	cases := []struct {
		ip           string
		wantContains string
		wantEmpty    bool
		allowError   bool
	}{
		// Reported by a user: offline databases claim Beijing, it is Zhejiang.
		{ip: "2408:8740:41ff:9:2::16c", wantContains: "联通"},
		{ip: "58.247.214.47", wantContains: "上海 联通"},
		{ip: "2409:891b:5645:b7:1c49:83c:e49d:e9b9", wantContains: "移动"},
		{ip: "240e:335:2c02:f961:3d64:1521:78d:b4eb", wantContains: "电信"},
		// Well known public DNS addresses are redirected to themed pages, they
		// are served by the offline database instead.
		{ip: "8.8.8.8", wantEmpty: true, allowError: true},
		{ip: "127.0.0.1", wantEmpty: true, allowError: true},
	}
	for _, c := range cases {
		value, err := lookupOnline(c.ip)
		if err != nil {
			if !c.allowError {
				t.Errorf("lookupOnline(%s) error: %v", c.ip, err)
			}
			continue
		}
		t.Logf("%-42s => %q", c.ip, value)
		switch {
		case c.wantEmpty && value != "":
			t.Errorf("lookupOnline(%s) = %q, want empty", c.ip, value)
		case !c.wantEmpty && !strings.Contains(value, c.wantContains):
			t.Errorf("lookupOnline(%s) = %q, want it to contain %q", c.ip, value, c.wantContains)
		}
	}

	// The second call must be served from the cache.
	start := time.Now()
	if _, err := lookupOnline("58.247.214.47"); err != nil {
		t.Fatalf("cached lookup failed: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("cached lookup took %v, expected it to be instant", d)
	}
}
