package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// enCtx gives an English command context, so assertions match English output.
func enCtx() *plugin.CommandContext {
	return &plugin.CommandContext{Lang: "en-US"}
}

// ============================================================
// Argument parsing
// ============================================================

func TestExtractIP(t *testing.T) {
	cases := map[string]string{
		"1.1.1.1":                   "1.1.1.1",
		"8.8.8.0/24":                "8.8.8.0",
		"ip 8.8.8.8:53":             "8.8.8.8",
		"[2001:db8::1]:443":         "2001:db8::1",
		"2606:4700:4700::1111":      "2606:4700:4700::1111",
		"host 1.2.3.4, and 5.6.7.8": "1.2.3.4",
		"no ip here":                "",
		"999.1.1.1":                 "",
		"AS15169":                   "",
	}
	for in, want := range cases {
		if got := extractIP(in); got != want {
			t.Errorf("extractIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseASN(t *testing.T) {
	ok := map[string]int{
		"AS15169":    15169,
		"as13335":    13335,
		"15169":      15169,
		"as-65001":   65001,
		"4294967295": 4294967295,
	}
	for in, want := range ok {
		got, found := parseASN(in)
		if !found || got != want {
			t.Errorf("parseASN(%q) = %d/%v, want %d", in, got, found, want)
		}
	}
	bad := []string{"nope", "AS0", "0", "4294967296", "port 53", "see 6939 things"}
	for _, in := range bad {
		if got, found := parseASN(in); found {
			t.Errorf("parseASN(%q) = %d, want no match", in, got)
		}
	}
}

// ============================================================
// Cymru parsing
// ============================================================

func TestParseCymruLine(t *testing.T) {
	ipRec := parseCymruLine("13335   | 1.1.1.1          | 1.1.1.0/24          | AU | apnic     | 2011-08-11 | CLOUDFLARENET - Cloudflare, Inc., US")
	if ipRec.AS != "13335" || ipRec.Prefix != "1.1.1.0/24" || ipRec.Registry != "apnic" || ipRec.CC != "AU" {
		t.Errorf("ip record wrong: %+v", ipRec)
	}
	asRec := parseCymruLine("15169   | US | arin     | 2000-03-30 | GOOGLE - Google LLC, US")
	if asRec.AS != "15169" || asRec.CC != "US" || asRec.ASName != "GOOGLE - Google LLC, US" {
		t.Errorf("asn record wrong: %+v", asRec)
	}
}

func TestCymruLookupNoMatch(t *testing.T) {
	if _, err := cymruLookup(t.Context(), "999.999.999.999"); err == nil {
		t.Error("want error for a garbage query")
	}
}

// ============================================================
// Path statistics
// ============================================================

func TestDedupePaths(t *testing.T) {
	peers := []lgPeer{
		{ASPath: "174 6453 15169"},
		{ASPath: "174 6453 15169"},
		{ASPath: "3356 15169"},
		{ASPath: "15169 15169 174 15169"}, // prepends + leading/trailing origin
		{ASPath: "174 174 6453 15169"},    // prepended transit
	}
	stats := dedupePaths(peers, 15169)
	if len(stats) != 3 {
		t.Fatalf("want 3 distinct paths, got %d: %+v", len(stats), stats)
	}
	if stats[0].Path != "174 6453" || stats[0].Count != 3 {
		t.Errorf("top path = %+v, want 174 6453 ×3", stats[0])
	}
}

func TestTopPaths(t *testing.T) {
	stats := []pathStat{{Path: "a", Count: 1}, {Path: "b", Count: 5}, {Path: "c", Count: 3}}
	top := topPaths(stats, 2)
	if len(top) != 2 || top[0].Path != "b" || top[1].Path != "c" {
		t.Errorf("topPaths = %+v", top)
	}
}

func TestPrefixCandidates(t *testing.T) {
	c := prefixCandidates(net.ParseIP("1.1.1.1"))
	if len(c) != 2 || c[0] != "1.1.1.0/24" || c[1] != "1.1.0.0/23" {
		t.Errorf("v4 candidates = %v", c)
	}
	if got := prefixCandidates(net.ParseIP("2606:4700:4700::1111")); len(got) != 1 || got[0] != "2606:4700:4700::/48" {
		t.Errorf("v6 candidates = %v", got)
	}
}

// ============================================================
// Rendering with canned data
// ============================================================

func cannedCymruIP() cymruRecord {
	return cymruRecord{
		AS: "13335", IP: "1.1.1.1", Prefix: "1.1.1.0/24", CC: "AU", Registry: "apnic",
		Allocated: "2011-08-11", ASName: "CLOUDFLARENET - Cloudflare, Inc., US",
	}
}

func cannedNI() networkInfo { return networkInfo{ASNs: []string{"13335"}, Prefix: "1.1.1.0/24"} }

func cannedOV() prefixOverview {
	ov := prefixOverview{Announced: true, Resource: "1.1.1.0/24",
		ASNs:    []ripeASN{{ASN: 13335, Holder: "CLOUDFLARENET - Cloudflare, Inc."}},
		Related: []string{"1.0.0.0/24"}}
	ov.Block.Resource = "1.0.0.0/22"
	return ov
}

func cannedLG() lookingGlass {
	return lookingGlass{RRCs: []lgRRC{{
		RRC: "RRC01", Location: "London",
		Peers: []lgPeer{
			{ASPath: "174 6453 13335"},
			{ASPath: "174 6453 13335"},
			{ASPath: "3356 13335"},
		},
	}}}
}

func TestRenderNet(t *testing.T) {
	out := renderNet(enCtx(), "1.1.1.1", "1.1.1.1", false,
		cannedCymruIP(), nil, cannedNI(), nil, cannedOV(), nil, cannedLG(), nil)
	if len(out) != 1 {
		t.Fatalf("want 1 page, got %d", len(out))
	}
	s := out[0]
	for _, want := range []string{"1.1.1.1", "1.1.1.0/24", "AS13335", "Cloudflare, Inc., US",
		"AU", "2011-08-11", "174 6453", "×2", "bgp.tools/prefix/1.1.1.0/24", "login-walled"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestRenderNetNotAnnounced(t *testing.T) {
	// niErr==nil, ovErr==nil, no announcement and no cymru match: must still
	// say "not announced", not an em dash.
	s := renderNet(enCtx(), "203.0.113.5", "203.0.113.5", false,
		cymruRecord{}, errors.New("no ASN or IP match"), networkInfo{}, nil,
		prefixOverview{Resource: "203.0.113.0/24"}, nil, lookingGlass{}, nil)[0]
	if !strings.Contains(s, "not announced") {
		t.Errorf("missing not-announced note:\n%s", s)
	}
}

func TestRenderNetPrefixQuery(t *testing.T) {
	s := renderNet(enCtx(), "8.8.8.0/24", "8.8.8.0", true,
		cymruRecord{AS: "15169", Prefix: "8.8.8.0/24", CC: "US", Registry: "arin",
			Allocated: "2023-12-28", ASName: "GOOGLE - Google LLC, US"}, nil,
		networkInfo{ASNs: []string{"15169"}, Prefix: "8.8.8.0/24"}, nil,
		cannedOV(), nil, lookingGlass{}, nil)[0]
	if !strings.Contains(s, "Prefix: `8.8.8.0/24`") {
		t.Errorf("prefix query should echo the prefix:\n%s", s)
	}
}

func TestRenderASN(t *testing.T) {
	var ov asOverview
	ov.Holder = "GOOGLE - Google LLC, US"
	ov.Block.Resource = "15169-15169"
	var nb neighboursData
	nb.Counts = neighbourCounts{Left: 10, Right: 120, Unique: 130}
	s := renderASN(enCtx(), 15169, ov, nil, nb, nil, 3, nil,
		cymruRecord{AS: "15169", CC: "US", Registry: "arin", Allocated: "2000-03-30"}, nil)[0]
	for _, want := range []string{"AS15169", "Google LLC, US", "120", "downstream", "3",
		"bgp.tools/AS15169", "bgp.he.net/AS15169"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestRenderASNPartialErrors(t *testing.T) {
	s := renderASN(enCtx(), 64512, asOverview{}, context.Canceled, neighboursData{}, nil, 0, nil, cymruRecord{}, nil)[0]
	if !strings.Contains(s, "❌") {
		t.Errorf("errors should be listed:\n%s", s)
	}
}

func TestRenderDNS(t *testing.T) {
	chain := dnsChain{
		Forward: map[string][]string{"one.one.one.one": {"1.0.0.1", "1.1.1.1", "2606:4700:4700::1111"}},
		Reverse: map[string][]string{"1.1.1.1": {"one.one.one.one"}},
		AuthNS:  []string{"dorthy.ns.cloudflare.com"},
	}
	out := renderDNS(enCtx(), "1.1.1.1", chain)
	if len(out) == 0 {
		t.Fatal("want pages, got none")
	}
	s := out[0]
	for _, want := range []string{"1.1.1.1\tone.one.one.one", "1.0.0.1", "2606:4700:4700::1111", "dorthy.ns.cloudflare.com"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Count(s, "1.1.1.1\tone.one.one.one") != 1 {
		t.Errorf("query IP must not be duplicated:\n%s", s)
	}
}

func TestRenderDNSForwardOnly(t *testing.T) {
	chain := dnsChain{Forward: map[string][]string{"dns.google": {"8.8.8.8"}}}
	out := renderDNS(enCtx(), "8.8.8.8", chain)
	if len(out) == 0 {
		t.Fatal("forward records alone should render")
	}
	if !strings.Contains(out[0], "dns.google") {
		t.Errorf("missing name:\n%s", out[0])
	}
}

func TestRenderDNSEmpty(t *testing.T) {
	if out := renderDNS(enCtx(), "8.8.4.4", dnsChain{}); out != nil {
		t.Errorf("want nil, got %v", out)
	}
}

// ============================================================
// Page packing
// ============================================================

func TestPackPages(t *testing.T) {
	ln := strings.Repeat("x", 900)
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = ln
	}
	pages := packPages("HEADER", lines)
	if len(pages) < 2 {
		t.Fatalf("want multi-page, got %d", len(pages))
	}
	for i, p := range pages {
		if len(p) > pageHardLimit {
			t.Errorf("page %d too long: %d", i, len(p))
		}
	}
	if !strings.Contains(pages[0], "HEADER") {
		t.Error("page 0 must keep the header")
	}
	if !strings.Contains(pages[1], "📄 (2/") {
		t.Errorf("continuation must carry a marker:\n%.80s", pages[1])
	}
}

func TestPackPagesSingle(t *testing.T) {
	pages := packPages("H", []string{"a", "b"})
	if len(pages) != 1 || pages[0] != "H\na\nb" {
		t.Errorf("packPages = %q", pages[0])
	}
}

// ============================================================
// Misc
// ============================================================

func TestAsHolderName(t *testing.T) {
	if got := asHolderName("GOOGLE - Google LLC, US"); got != "Google LLC, US" {
		t.Errorf("asHolderName = %q", got)
	}
	if got := asHolderName("plain"); got != "plain" {
		t.Errorf("asHolderName = %q", got)
	}
}

func TestHelpAndBadArgs(t *testing.T) {
	ctx := enCtx()
	if !strings.Contains(help(ctx), "bgp dns 1.1.1.1") {
		t.Error("help must mention dns subcommand")
	}
	if !strings.Contains(badArgs(ctx, false), "No IP, prefix or ASN found") {
		t.Error("badArgs must explain what is missing")
	}
	if !strings.Contains(badArgs(ctx, true), "bgp dns 1.1.1.1") {
		t.Error("dns badArgs must show an example")
	}
}

var _ = plugin.Code
