package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

// nodeInfo is the outcome of parsing a subscription body.
type nodeInfo struct {
	count   int
	types   []kv // protocol/type → count
	regions []kv // region → count
	names   []string
}

type kv struct {
	k string
	v int
}

// regionRules maps a display region to the keywords that identify it.
// Order matters: the first matching region wins.
var regionRules = []struct {
	name string
	keys []string
}{
	// 亚洲 Asia
	{"香港", []string{"香港", "hong kong", "hongkong", "hk", "hkg"}},
	{"台湾", []string{"台湾", "taiwan", "tw", "taipei", "tpe"}},
	{"日本", []string{"日本", "japan", "jp", "tokyo", "osaka", "jap"}},
	{"新加坡", []string{"新加坡", "singapore", "sg", "sgp"}},
	{"韩国", []string{"韩国", "korea", "kr", "seoul", "kor"}},
	{"印度", []string{"印度", "india", "in", "mumbai", "delhi", "ind"}},
	{"马来西亚", []string{"马来西亚", "malaysia", "my", "kuala lumpur", "mys"}},
	{"泰国", []string{"泰国", "thailand", "th", "bangkok", "tha"}},
	{"越南", []string{"越南", "vietnam", "vn", "hanoi", "vnm"}},
	{"印尼", []string{"印尼", "印度尼西亚", "indonesia", "id", "jakarta", "idn"}},
	{"菲律宾", []string{"菲律宾", "philippines", "ph", "manila", "phl"}},
	{"土耳其", []string{"土耳其", "turkey", "tr", "istanbul", "ankara", "tur"}},
	// 北美 North America
	{"美国", []string{"美国", "united states", "us", "usa", "los angeles", "san jose", "silicon valley"}},
	{"加拿大", []string{"加拿大", "canada", "ca", "toronto", "vancouver"}},
	// 欧洲 Europe
	{"英国", []string{"英国", "united kingdom", "uk", "london", "manchester", "gbr"}},
	{"德国", []string{"德国", "germany", "de", "frankfurt", "berlin", "deu"}},
	{"法国", []string{"法国", "france", "fr", "paris", "fra"}},
	{"荷兰", []string{"荷兰", "netherlands", "nl", "amsterdam", "nld"}},
	{"瑞士", []string{"瑞士", "switzerland", "ch", "zurich", "che"}},
	{"意大利", []string{"意大利", "italy", "it", "milan", "rome", "ita"}},
	{"西班牙", []string{"西班牙", "spain", "es", "madrid", "barcelona", "esp"}},
	{"瑞典", []string{"瑞典", "sweden", "se", "stockholm", "swe"}},
	{"挪威", []string{"挪威", "norway", "no", "oslo", "nor"}},
	{"芬兰", []string{"芬兰", "finland", "fi", "helsinki", "fin"}},
	{"丹麦", []string{"丹麦", "denmark", "dk", "copenhagen", "dnk"}},
	{"波兰", []string{"波兰", "poland", "pl", "warsaw", "pol"}},
	{"奥地利", []string{"奥地利", "austria", "at", "vienna", "aut"}},
	{"比利时", []string{"比利时", "belgium", "be", "brussels", "bel"}},
	{"爱尔兰", []string{"爱尔兰", "ireland", "ie", "dublin", "irl"}},
	{"葡萄牙", []string{"葡萄牙", "portugal", "pt", "lisbon", "prt"}},
	{"希腊", []string{"希腊", "greece", "gr", "athens", "grc"}},
	{"卢森堡", []string{"卢森堡", "luxembourg", "lu", "lux"}},
	{"乌克兰", []string{"乌克兰", "ukraine", "ua", "kiev", "ukr"}},
	// 大洋洲 Oceania
	{"澳大利亚", []string{"澳大利亚", "australia", "au", "sydney", "melbourne", "aus"}},
	{"新西兰", []string{"新西兰", "new zealand", "nz", "auckland", "nzl"}},
	// 南美/中东/非洲 South America / Middle East / Africa / Russia
	{"巴西", []string{"巴西", "brazil", "br", "sao paulo", "rio", "bra"}},
	{"阿联酋", []string{"阿联酋", "uae", "united arab emirates", "ae", "dubai", "abu dhabi", "are"}},
	{"以色列", []string{"以色列", "israel", "il", "tel aviv", "jerusalem", "isr"}},
	{"南非", []string{"南非", "south africa", "za", "johannesburg", "cape town", "zaf"}},
	{"俄罗斯", []string{"俄罗斯", "russia", "ru", "moscow", "st.petersburg", "rus"}},
}

// regionOf returns the first region whose keywords appear in name, "" if none.
func regionOf(name string) string {
	low := strings.ToLower(name)
	for _, r := range regionRules {
		for _, k := range r.keys {
			if strings.Contains(low, strings.ToLower(k)) {
				return r.name
			}
		}
	}
	return ""
}

// countRegions tallies per-region counts and appends 其他 for unmatched.
func countRegions(names []string) []kv {
	var regions []kv
	idx := map[string]int{}
	identified := 0
	add := func(r string) {
		if i, ok := idx[r]; ok {
			regions[i].v++
		} else {
			idx[r] = len(regions)
			regions = append(regions, kv{r, 1})
		}
	}
	for _, n := range names {
		if r := regionOf(n); r != "" {
			add(r)
			identified++
		}
	}
	if len(names)-identified > 0 {
		add("其他")
	}
	return regions
}

func countTypes(m map[string]int) []kv {
	var out []kv
	for k, v := range m {
		if v > 0 {
			out = append(out, kv{k, v})
		}
	}
	// deterministic order for stable output and tests
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].k < out[j-1].k; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// clashDoc is the slice of a Clash config we care about.
type clashDoc struct {
	Proxies []struct {
		Name string `yaml:"name"`
		Type string `yaml:"type"`
	} `yaml:"proxies"`
}

// nodeProtocols are the URI schemes scanned for in base64 subscriptions.
var nodeProtocols = []string{
	"vmess://", "trojan://", "ss://", "ssr://", "vless://", "hy2://",
	"hysteria://", "hy://", "tuic://", "wireguard://", "socks5://",
	"http://", "https://", "shadowtls://", "naive://",
}

// parseNodes parses a subscription body: Clash YAML first, then a relaxed
// base64 list of share links. Always returns non-nil (possibly empty).
func parseNodes(body []byte) *nodeInfo {
	if n := parseYAMLNodes(body); n != nil {
		return n
	}
	return parseBase64Nodes(body)
}

// parseYAMLNodes extracts proxies from a Clash-style YAML config; nil when
// the body is not YAML or has no proxies.
func parseYAMLNodes(body []byte) *nodeInfo {
	var doc clashDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil
	}
	if len(doc.Proxies) == 0 {
		return nil
	}
	info := &nodeInfo{count: len(doc.Proxies)}
	typeCount := map[string]int{}
	for _, px := range doc.Proxies {
		t := strings.ToLower(px.Type)
		if t == "" {
			t = "unknown"
		}
		typeCount[t]++
		info.names = append(info.names, px.Name)
	}
	info.types = countTypes(typeCount)
	info.regions = countRegions(info.names)
	return info
}

// decodeBase64Loose decodes base64 ignoring whitespace and padding,
// trying padded Std then RawStd alphabets. Empty on failure.
func decodeBase64Loose(s string) []byte {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	if clean == "" {
		return nil
	}
	if b, err := base64.StdEncoding.DecodeString(clean); err == nil {
		return b
	}
	if b, err := base64.RawStdEncoding.DecodeString(clean); err == nil {
		return b
	}
	return nil
}

// parseBase64Nodes scans a (possibly base64-encoded) list of share links.
// Undecodable bodies yield an empty nodeInfo, matching the source.
func parseBase64Nodes(body []byte) *nodeInfo {
	info := &nodeInfo{}
	text := string(body)
	if decoded := decodeBase64Loose(text); decoded != nil {
		text = string(decoded)
	}
	typeCount := map[string]int{}
	var names []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, proto := range nodeProtocols {
			if strings.HasPrefix(line, proto) {
				typeCount[strings.TrimSuffix(proto, "://")]++
				names = append(names, extractNodeNameURL(line))
				break
			}
		}
	}
	info.count = len(names)
	info.names = names
	info.types = countTypes(typeCount)
	info.regions = countRegions(names)
	return info
}

var hostSplitRe = regexp.MustCompile(`[?#/]`)

// extractNodeNameURL pulls a readable name out of a share link:
// #fragment (URL-decoded) → vmess ps/add:port → host after @ → first 60
// chars.
func extractNodeNameURL(line string) string {
	if i := strings.LastIndex(line, "#"); i > 0 {
		frag := line[i+1:]
		if d, err := url.QueryUnescape(frag); err == nil && d != "" {
			return d
		}
		if frag != "" {
			return frag
		}
	}
	if after, ok := strings.CutPrefix(line, "vmess://"); ok {
		var v struct {
			Ps   string `json:"ps"`
			Add  string `json:"add"`
			Port any    `json:"port"`
		}
		if b := decodeBase64Loose(after); b != nil && json.Unmarshal(b, &v) == nil {
			if v.Ps != "" {
				return v.Ps
			}
			if v.Add != "" && v.Port != nil {
				return v.Add + ":" + portString(v.Port)
			}
		}
	}
	if i := strings.Index(line, "@"); i > 0 {
		rest := line[i+1:]
		if j := hostSplitRe.FindStringIndex(rest); j != nil {
			rest = rest[:j[0]]
		}
		if rest != "" {
			return rest
		}
	}
	r := []rune(line)
	if len(r) > 60 {
		r = r[:60]
	}
	return strings.Map(func(rn rune) rune {
		switch rn {
		case '&', '<', '>', '"', '\'':
			return -1
		}
		return rn
	}, string(r))
}

func portString(p any) string {
	switch v := p.(type) {
	case float64:
		return strconv.Itoa(int(v))
	case string:
		return v
	}
	return ""
}
