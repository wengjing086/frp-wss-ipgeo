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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// Several free services are combined, because none of them covers everything
// and each of them has blind spots:
//
//	pconline  IPv4 only, city level + ISP, no rate limiting observed; the
//	          authoritative source for IPv4
//	ipshudi   IPv4 and IPv6, the ONLY source that locates Chinese IPv6 ranges
//	          correctly (city level for IPv4, province level for IPv6) plus the
//	          ISP; it is the primary source for both families. It throttles
//	          bursts hard (a single query often ends in EOF), so it is probed
//	          first and the rest are fallbacks
//	ip-api    IPv4 only fallback, province + ISP; its IPv6 geolocation is
//	          wrong for China (it maps whole China Unicom/Telecom/Mobile IPv6
//	          blocks to Beijing/Hebei), so it is NOT used for IPv6
//	geojs     IPv4 and IPv6, no location data at all, only its ASN gives the
//	          ISP; kept as the last resort so a throttled ipshudi at least
//	          yields the operator instead of a wrong province
//
// Lookups walk the list for the matching IP version and use the first answer
// that carries a location.

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// errNoResult means the service answered but has no result for the address.
// It is not retried, another provider may know it.
var errNoResult = errors.New("ipgeo: no result from provider")

type provider struct {
	name  string
	url   func(ip string) string
	parse func(body []byte) (location string, isp string, found bool)
}

func (p provider) query(ctx context.Context, ip string) (location string, isp string, found bool, err error) {
	for attempt := 0; attempt < maxQueryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", "", false, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		if err := waitTurn(ctx); err != nil {
			return "", "", false, err
		}
		body, err := httpGet(ctx, p.url(ip))
		if err != nil {
			if errors.Is(err, errNoResult) {
				return "", "", false, nil
			}
			// Networking trouble, often a refused burst of connections.
			if attempt == maxQueryAttempts-1 {
				return "", "", false, err
			}
			continue
		}
		location, isp, found := p.parse(body)
		if !found {
			return "", "", false, nil
		}
		return location, isp, true, nil
	}
	return "", "", false, err
}

var ipv4Providers = []provider{
	{
		name:  "pconline",
		url:   func(ip string) string { return "https://whois.pconline.com.cn/ipJson.jsp?ip=" + ip + "&json=true" },
		parse: parsePconline,
	},
	{
		name:  "ipshudi",
		url:   func(ip string) string { return onlineEndpoint + ip + ".htm" },
		parse: parseIPShudi,
	},
	{
		name:  "ipapi",
		url:   func(ip string) string { return "http://ip-api.com/json/" + ip + "?fields=status,countryCode,regionName,org,isp" },
		parse: parseIPApi,
	},
	{
		name:  "geojs",
		url:   func(ip string) string { return "https://get.geojs.io/v1/ip/geo/" + ip + ".json" },
		parse: parseGeoJS,
	},
}

// pconline answers IPv4 only: given an IPv6 address it silently resolves it to
// some unrelated IPv4 ("::1" family treated as text), so it must never be used
// for IPv6 or it would report a completely wrong location.
//
// ipshudi is the primary source for IPv6 (it is the only one with accurate
// Chinese IPv6 data); geojs is the last resort and yields the ISP only, so a
// throttled ipshudi never produces a wrong province. ip-api is deliberately
// absent here because its IPv6 location is wrong for China.
var ipv6Providers = []provider{
	{
		name:  "ipshudi",
		url:   func(ip string) string { return onlineEndpoint + ip + ".htm" },
		parse: parseIPShudi,
	},
	{
		name:  "geojs",
		url:   func(ip string) string { return "https://get.geojs.io/v1/ip/geo/" + ip + ".json" },
		parse: parseGeoJS,
	},
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	resp, err := onlineClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A themed page (public DNS addresses) or an error page.
		return nil, errNoResult
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// parsePconline reads the GBK encoded JSON answer, for example
// {"pro":"浙江省","city":"杭州市","addr":"浙江省杭州市华数宽带"}.
func parsePconline(body []byte) (string, string, bool) {
	decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(body), simplifiedchinese.GBK.NewDecoder()))
	if err != nil {
		return "", "", false
	}
	var resp struct {
		Pro       string `json:"pro"`
		City      string `json:"city"`
		Addr      string `json:"addr"`
		ProCode   string `json:"proCode"`
		ErrorCode string `json:"err"`
	}
	if err := json.Unmarshal(decoded, &resp); err != nil {
		return "", "", false
	}
	// The service marks addresses outside mainland China this way.
	if resp.Pro == "" || resp.ProCode == "999999" {
		return "", "", true
	}
	countryUs := resp.ErrorCode == "noprovince"
	if countryUs {
		return "", "", true
	}

	location := formatLocationParts([]string{resp.Pro, resp.City})
	isp := extractISPFromAddr(resp.Addr, resp.Pro, resp.City)
	return location, isp, true
}

// extractISPFromAddr strips the known location from the free form address, the
// remainder is the ISP name.
func extractISPFromAddr(addr, province, city string) string {
	rest := strings.TrimSpace(addr)
	for _, known := range []string{province, city} {
		if known != "" {
			rest = strings.Replace(rest, known, "", 1)
		}
	}
	return normalizeISP(rest)
}

// parseGeoJS only yields the ISP through its ASN description, there is no city
// data for China in it. It is the safety net when every other source fails.
func parseGeoJS(body []byte) (string, string, bool) {
	var resp struct {
		CountryCode  string `json:"country_code"`
		Organization string `json:"organization"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", false
	}
	if resp.CountryCode != "" && resp.CountryCode != "CN" {
		return "", "", true
	}
	return "", normalizeISP(resp.Organization), true
}

// parseIPApi returns province level location and the ISP. ip-api.com is the
// reliable fallback that works for both IPv4 and IPv6 with a generous rate
// limit; province names arrive in English and are mapped to Chinese.
func parseIPApi(body []byte) (string, string, bool) {
	var resp struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		RegionName  string `json:"regionName"`
		Org         string `json:"org"`
		ISP         string `json:"isp"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", false
	}
	if resp.Status != "success" {
		return "", "", false
	}
	if resp.CountryCode != "" && resp.CountryCode != "CN" {
		return "", "", true
	}
	location := mapProvince(resp.RegionName)
	isp := normalizeISP(resp.Org)
	if isp == "" {
		isp = normalizeISP(resp.ISP)
	}
	return location, isp, true
}

// provinceCN maps the English province names returned by ip-api.com onto the
// short Chinese form used everywhere else in this package.
var provinceCN = map[string]string{
	"Anhui":         "安徽",
	"Beijing":       "北京",
	"Chongqing":     "重庆",
	"Fujian":        "福建",
	"Gansu":         "甘肃",
	"Guangdong":     "广东",
	"Guangxi":       "广西",
	"Guizhou":       "贵州",
	"Hainan":        "海南",
	"Hebei":         "河北",
	"Heilongjiang":  "黑龙江",
	"Henan":         "河南",
	"Hubei":         "湖北",
	"Hunan":         "湖南",
	"Inner Mongolia": "内蒙古",
	"Nei Mongol":    "内蒙古",
	"Jiangsu":       "江苏",
	"Jiangxi":       "江西",
	"Jilin":         "吉林",
	"Liaoning":      "辽宁",
	"Ningxia":       "宁夏",
	"Qinghai":       "青海",
	"Shaanxi":       "陕西",
	"Shandong":      "山东",
	"Shanghai":      "上海",
	"Shanxi":        "山西",
	"Sichuan":       "四川",
	"Tianjin":       "天津",
	"Xinjiang":      "新疆",
	"Xizang":        "西藏",
	"Yunnan":        "云南",
	"Zhejiang":      "浙江",
	"Hong Kong":     "香港",
	"Macau":         "澳门",
	"Macao":         "澳门",
	"Taiwan":        "台湾",
}

func mapProvince(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		return ""
	}
	if cn, ok := provinceCN[region]; ok {
		return cn
	}
	// Be tolerant of casing differences.
	if cn, ok := provinceCN[strings.ToLower(region)]; ok {
		return cn
	}
	return region
}

var asnPrefix = regexp.MustCompile(`(?i)^as\d+\s+`)

// normalizeISP maps whatever a service reports onto a short Chinese name.
// Matching is case insensitive, the services report "CHINANET", "Chinanet" and
// "ChinaNet" interchangeably.
func normalizeISP(isp string) string {
	isp = strings.TrimSpace(isp)
	if isp == "" {
		return ""
	}
	if v, ok := lookupISP(isp); ok {
		return v
	}
	isp = asnPrefix.ReplaceAllString(isp, "")
	if v, ok := lookupISP(isp); ok {
		return v
	}
	// Match the longest alias prefix, "CHINA UNICOM China169 Backbone" should
	// resolve to 联通 through its first two words.
	words := strings.Fields(isp)
	for i := len(words); i > 1; i-- {
		if v, ok := lookupISP(strings.Join(words[:i], " ")); ok {
			return v
		}
	}
	for _, kw := range ispKeywords {
		if strings.Contains(isp, kw) {
			return kw
		}
	}
	// ASN descriptions are free form English, "HeiLongJiang Mobile
	// Communication Company Limited" has to end up as 移动.
	lower := strings.ToLower(isp)
	for _, kw := range englishISPKeywords {
		if strings.Contains(lower, kw.pattern) {
			return kw.name
		}
	}
	if len(words) > 0 {
		return words[0]
	}
	return ""
}

// englishISPKeywords are checked in order, the most specific first.
var englishISPKeywords = []struct {
	pattern string
	name    string
}{
	{"aliyun", "阿里云"},
	{"alibaba", "阿里云"},
	{"tencent", "腾讯云"},
	{"huawei", "华为云"},
	{"unicom", "联通"},
	{"mobile", "移动"},
	{"chinanet", "电信"},
	{"telecom", "电信"},
	{"tietong", "铁通"},
	{"railway", "铁通"},
	{"cernet", "教育网"},
}

func lookupISP(name string) (string, bool) {
	for alias, short := range ispAliases {
		if strings.EqualFold(name, alias) {
			return short, true
		}
	}
	return "", false
}
