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
// Geolocation is resolved exclusively through public online services (see
// providers.go for the provider list and the rationale): offline databases
// cannot keep up with Chinese allocations, notably IPv6 where city data is
// frequently wrong, and would bloat the binary. Results are cached in memory and,
// when persistence is enabled, on disk, so only the first sighting of an address
// costs a request.
package ipgeo

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	onlineEndpoint = "https://www.ipshudi.com/"

	defaultOnlineTimeout = 4 * time.Second
	// Budget for a whole lookup including throttling and retries, the single
	// HTTP request is limited by defaultOnlineTimeout.
	totalQueryTimeout = 10 * time.Second
	// Positive answers are stable, keep them for a day.
	positiveTTL = 24 * time.Hour
	// Negative answers (addresses nobody could locate) are retried sooner.
	negativeTTL = 6 * time.Hour
	// A result that only carries the ISP (ipshudi was throttled and geojs
	// answered) is retried within the hour so a recovering service can still
	// fill in the location later.
	partialTTL = 1 * time.Hour

	// Failed attempts get a couple of retries, a burst of connections is
	// refused with EOF so trying again usually succeeds.
	maxQueryAttempts = 3
	retryDelay       = 500 * time.Millisecond

	// Requests are serialized and spaced out. Once the service throttles a
	// client it stays throttled for minutes.
	minQueryInterval = 2 * time.Second

	// A provider that fails repeatedly is skipped for a while, otherwise every
	// lookup would wait for its timeout before falling through.
	providerFailures     = 3
	providerPause        = 5 * time.Minute
	globalBreakerFailure = 8
	globalBreakerPause   = 5 * time.Minute
	cacheLimit           = 10000
)

var (
	onlineEnabledFlag = true
	onlineMu          sync.Mutex

	onlineClient = &http.Client{
		Timeout: defaultOnlineTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// The service closes idle connections very quickly, reusing them
			// turns into spurious EOFs. Every query gets a fresh one instead.
			DisableKeepAlives:   true,
			MaxIdleConnsPerHost: 2,
			TLSHandshakeTimeout: 5 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Public DNS addresses are redirected to a themed page, there is no
			// point following more than one hop.
			if len(via) >= 1 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	throttleMu sync.Mutex
	lastQuery  time.Time

	cacheMu sync.RWMutex
	cache   = make(map[string]cacheEntry)

	// Addresses nobody could answer. Kept apart from the value cache so the
	// offline database still gets a chance for them.
	failCache = make(map[string]time.Time)

	callMu sync.Mutex
	calls  = make(map[string]*queryCall)

	breakerFailuresCount int
	breakerPauseUntil    time.Time
)

type cacheEntry struct {
	value    string
	deadline time.Time
}

type queryCall struct {
	wg  sync.WaitGroup
	val string
	err error
}

// errOnlineDisabled is returned while the breaker is open, so the caller falls
// back to the offline database instead of waiting for a timeout.
var errOnlineDisabled = errors.New("ipgeo: online lookup temporarily disabled")

// errUnparsableResult is returned when no provider could describe the address.
var errUnparsableResult = errors.New("ipgeo: no geolocation for address")

// SetOnlineEnabled turns the online lookup on or off. It is enabled by
// default. Build with -tags noipgeo to drop the embedded databases entirely.
func SetOnlineEnabled(enabled bool) {
	onlineMu.Lock()
	onlineEnabledFlag = enabled
	onlineMu.Unlock()
}

// SetOnlineTimeout overrides the per query timeout.
func SetOnlineTimeout(d time.Duration) {
	onlineClient.Timeout = d
}

func onlineEnabled() bool {
	onlineMu.Lock()
	defer onlineMu.Unlock()
	return onlineEnabledFlag
}

// lookupOnline resolves the IP through the online services. The returned value
// is "" (without an error) when the address is known but not Chinese, and an
// error when nothing could be determined so the caller can fall back.
func lookupOnline(ip string) (string, error) {
	if breakerOpen() {
		return "", errOnlineDisabled
	}
	if failHit(ip) {
		return "", errUnparsableResult
	}
	if v, ok := cacheLookup(ip); ok {
		return v, nil
	}

	v, err := singleQuery(ip)
	if err != nil {
		if errors.Is(err, errUnparsableResult) {
			noteSuccess()
			failStore(ip)
		} else {
			noteFailure()
		}
		return "", err
	}
	noteSuccess()
	cacheStore(ip, v)
	return v, nil
}

// singleQuery collapses concurrent lookups of the same IP into one request.
func singleQuery(ip string) (string, error) {
	callMu.Lock()
	if c, ok := calls[ip]; ok {
		callMu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &queryCall{}
	c.wg.Add(1)
	calls[ip] = c
	callMu.Unlock()

	c.val, c.err = queryProviders(context.Background(), ip)

	callMu.Lock()
	delete(calls, ip)
	callMu.Unlock()
	c.wg.Done()
	return c.val, c.err
}

// queryProviders walks the provider list matching the IP version.
func queryProviders(ctx context.Context, ip string) (string, error) {
	providers := ipv6Providers
	if !strings.Contains(ip, ":") {
		providers = ipv4Providers
	}

	ctx, cancel := context.WithTimeout(ctx, totalQueryTimeout)
	defer cancel()

	var lastErr error
	for _, p := range providers {
		if !providerAvailable(p.name) {
			continue
		}
		location, isp, found, err := p.query(ctx, ip)
		if err != nil {
			lastErr = err
			noteProviderFailure(p.name)
			continue
		}
		noteProviderSuccess(p.name)
		if !found {
			continue
		}
		return strings.TrimSpace(location + " " + isp), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errUnparsableResult
}

// Per provider health. Each service is skipped for a while once it keeps
// failing, for example while ipshudi is throttling us.
var providerHealthMu sync.Mutex
var providerFailCounts = make(map[string]int)
var providerPausedUntil = make(map[string]time.Time)

func providerAvailable(name string) bool {
	providerHealthMu.Lock()
	defer providerHealthMu.Unlock()
	return time.Now().After(providerPausedUntil[name])
}

func noteProviderFailure(name string) {
	providerHealthMu.Lock()
	defer providerHealthMu.Unlock()
	providerFailCounts[name]++
	if providerFailCounts[name] >= providerFailures {
		providerPausedUntil[name] = time.Now().Add(providerPause)
	}
}

func noteProviderSuccess(name string) {
	providerHealthMu.Lock()
	defer providerHealthMu.Unlock()
	providerFailCounts[name] = 0
	delete(providerPausedUntil, name)
}

// waitTurn serializes queries and enforces a minimum delay between them.
func waitTurn(ctx context.Context) error {
	throttleMu.Lock()
	defer throttleMu.Unlock()
	if wait := time.Until(lastQuery.Add(minQueryInterval)); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lastQuery = time.Now()
	return nil
}

// The breaker keeps a machine without internet from paying the connect timeout
// on every single login.
func breakerOpen() bool {
	cacheMu.RLock()
	defer cacheMu.RUnlock()
	return breakerFailuresCount >= globalBreakerFailure && time.Now().Before(breakerPauseUntil)
}

func noteFailure() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	breakerFailuresCount++
	if breakerFailuresCount >= globalBreakerFailure {
		breakerPauseUntil = time.Now().Add(globalBreakerPause)
	}
}

func noteSuccess() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	breakerFailuresCount = 0
	breakerPauseUntil = time.Time{}
}

func cacheLookup(ip string) (string, bool) {
	cacheMu.RLock()
	e, ok := cache[ip]
	cacheMu.RUnlock()
	if !ok || time.Now().After(e.deadline) {
		return "", false
	}
	return e.value, true
}

func cacheStore(ip string, value string) {
	ttl := positiveTTL
	switch {
	case value == "":
		ttl = negativeTTL
	case !strings.Contains(value, " "):
		// Only the ISP is known (ipshudi was throttled and geojs answered).
		// Retry sooner so a later successful lookup can add the location.
		ttl = partialTTL
	}
	cacheMu.Lock()
	if len(cache) >= cacheLimit {
		now := time.Now()
		for k, v := range cache {
			if now.After(v.deadline) {
				delete(cache, k)
			}
		}
	}
	cache[ip] = cacheEntry{value: value, deadline: time.Now().Add(ttl)}
	cacheMu.Unlock()
	markDirty()
}

func failHit(ip string) bool {
	cacheMu.RLock()
	t, ok := failCache[ip]
	cacheMu.RUnlock()
	return ok && time.Now().Before(t)
}

func failStore(ip string) {
	cacheMu.Lock()
	if len(failCache) >= cacheLimit {
		now := time.Now()
		for k, t := range failCache {
			if now.After(t) {
				delete(failCache, k)
			}
		}
	}
	failCache[ip] = time.Now().Add(negativeTTL)
	cacheMu.Unlock()
}

var (
	ignoredBlocks = regexp.MustCompile(`(?is)<script.*?</script>|<style.*?</style>|<!--.*?-->`)
	tagRe         = regexp.MustCompile(`<[^>]*>`)
)

// noise values rendered next to the result and marks for members only content.
var noiseFields = map[string]bool{
	"上报纠错":      true,
	"会员查看市区县":  true,
	"会员查看区县":   true,
	"会员查看":     true,
	"纠错":       true,
	"数据中心":     true,
	"0":        true,
	"-":        true,
	"局域网":      true,
	"保留地址":     true,
	"本机地址":     true,
	"IP地址":     true,
	"iP地址":     true,
	"未知":       true,
	"暂不提供查询":   true,
	"该IP暂无归属地": true,
}

// parseIPShudi extracts the location and the ISP from the result page.
// found reports whether the page contained a parsable result block.
func parseIPShudi(body []byte) (location string, isp string, found bool) {
	text := ignoredBlocks.ReplaceAllString(string(body), " ")
	fields := strings.FieldsFunc(tagRe.ReplaceAllString(text, "\x00"), func(r rune) bool { return r == '\x00' })

	// A country/province pair often lives in a single text node, such as
	// "<span>中国 浙江省</span>", so every field is split on whitespace again.
	var cells []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		cells = append(cells, strings.Fields(f)...)
	}

	start := indexOfCell(cells, "归属地")
	if start < 0 {
		return "", "", false
	}
	var parts []string
	for _, c := range cells[start+1:] {
		if c == "运营商" || c == "上报纠错" || c == "iP类型" || c == "IP类型" || c == "ip类型" {
			break
		}
		if noiseFields[c] {
			continue
		}
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return "", "", false
	}
	// Anything outside mainland China is displayed without a suffix.
	if parts[0] != "中国" {
		return "", "", true
	}

	location = formatLocationParts(parts[1:])
	isp = normalizeISP(fieldAfter(cells, "运营商"))
	return location, isp, true
}

func indexOfCell(cells []string, want string) int {
	for i, c := range cells {
		if c == want {
			return i
		}
	}
	return -1
}

func fieldAfter(cells []string, label string) string {
	i := indexOfCell(cells, label)
	if i < 0 || i+1 >= len(cells) {
		return ""
	}
	v := cells[i+1]
	if noiseFields[v] {
		return ""
	}
	return v
}

// administrativeSuffixes are stripped to keep the output short, "浙江省"
// becomes "浙江".
var administrativeSuffixes = []string{
	"特别行政区", "壮族自治区", "回族自治区", "维吾尔自治区", "自治区",
	"自治州", "自治县", "自治旗", "地区", "盟", "市", "省", "县",
}

// ethnicSuffixes appear inside autonomous prefecture names, "延边朝鲜族自治州"
// is shortened to "延边".
var ethnicSuffixes = []string{
	"朝鲜族", "土家族", "蒙古族", "维吾尔族", "回族", "藏族", "苗族", "彝族", "壮族",
	"布依族", "侗族", "瑶族", "白族", "哈尼族", "哈萨克族", "傣族", "黎族", "傈僳族",
	"佤族", "畲族", "拉祜族", "纳西族", "景颇族", "羌族", "满族", "撒拉族", "仡佬族",
	"达斡尔族", "锡伯族",
}

func trimAdministrativeSuffix(name string) string {
	name = strings.TrimSpace(name)
	for _, suffix := range administrativeSuffixes {
		if len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}
	for _, suffix := range ethnicSuffixes {
		if len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}
	return name
}

func formatLocationParts(parts []string) string {
	var out []string
	for _, p := range parts {
		p = trimAdministrativeSuffix(p)
		if p == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == p {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " ")
}

// ispAliases maps verbose names reported by the services onto the common short
// forms used in China.
var ispAliases = map[string]string{
	"中国联通":           "联通",
	"中国移动通信":         "移动",
	"中国移动":           "移动",
	"中国电信":           "电信",
	"中国铁通":           "铁通",
	"中国教育和科研计算机网":    "教育网",
	"阿里云":            "阿里云",
	"阿里巴巴":           "阿里云",
	"腾讯云":            "腾讯云",
	"百度云":            "百度云",
	"华为云":            "华为云",
	"华数":             "华数",
	"华数宽带":           "华数",
	"广电":             "广电",
	"CHINANET":      "电信",
	"CHINANET-BACKBONE": "电信",
	"CHINA UNICOM":  "联通",
	"CHINA MOBILE":  "移动",
	"China Unicom":  "联通",
	"China Mobile":  "移动",
	"China Telecom": "电信",
	"CHINANET-BACKBONE No.31,Jin-rong Street": "电信",
}

// ispKeywords are matched inside a free form organisation name, longest first.
var ispKeywords = []string{
	"教育网", "阿里云", "腾讯云", "百度云", "华为云", "京东云", "世纪互联",
	"长城宽带", "方正宽带", "联通", "移动", "电信", "铁通", "广电", "歌华", "华数",
}
