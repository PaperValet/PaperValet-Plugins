//go:build smoke

// Manual live-source harness, excluded from normal builds and `go test`:
//
//	go run -tags smoke . <ip|prefix|ASn|dns:ip>
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: go run -tags smoke . <ip|prefix|ASn|dns:ip>")
		os.Exit(1)
	}
	target := os.Args[1]
	c := newClient(20 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fmt.Println("query:", target)
	start := time.Now()
	ctxv := &plugin.CommandContext{Lang: "en-US", Ctx: ctx}

	if rest, ok := cutPrefix(target, "dns:"); ok {
		ip := extractIP(rest)
		var chain dnsChain
		err := c.ripeGet(ctx, "dns-chain", ip, &chain)
		fmt.Printf("dns %s err=%v\n%s\n", ip, err, renderDNS(ctxv, ip, chain))
		return
	}

	if asn, ok := parseASN(target); ok && extractIP(target) == "" {
		as := fmt.Sprintf("AS%d", asn)
		var ov asOverview
		var nb neighboursData
		var ap announcedPrefixes
		ovErr := c.ripeGet(ctx, "as-overview", as, &ov)
		nbErr := c.ripeGet(ctx, "asn-neighbours", as, &nb)
		apErr := c.ripeGet(ctx, "announced-prefixes", as, &ap)
		cy, cyErr := cymruLookup(ctx, as)
		fmt.Printf("cymru=%+v err=%v\n", cy, cyErr)
		for _, p := range renderASN(ctxv, asn, ov, ovErr, nb, nbErr, len(ap.Prefixes), apErr, cy, cyErr) {
			fmt.Println("--- page ---")
			fmt.Println(p)
		}
		fmt.Println("elapsed:", time.Since(start))
		return
	}

	query, hostIP, explicit := target, extractIP(target), false
	if _, _, err := net.ParseCIDR(target); err == nil {
		explicit = true
	} else {
		query = hostIP
	}

	var ni networkInfo
	niErr := c.ripeGet(ctx, "network-info", orStr(hostIP, query), &ni)
	cy, cyErr := cymruLookup(ctx, orStr(hostIP, query))
	prefix := ni.Prefix
	if prefix == "" {
		prefix = cy.Prefix
	}
	if prefix == "" && hostIP != "" {
		if ip := net.ParseIP(hostIP); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				prefix = cidr24(ip4)
			} else {
				prefix = cidr48(ip)
			}
		}
	}
	var ov prefixOverview
	var ovErr error
	if prefix != "" {
		ovErr = c.ripeGet(ctx, "prefix-overview", prefix, &ov)
	}
	var lg lookingGlass
	var lgErr error
	if prefix != "" && (ov.Announced || ovErr != nil) {
		lgErr = c.ripeGet(ctx, "looking-glass", prefix, &lg)
	}
	fmt.Printf("cymru=%+v err=%v\nni=%+v err=%v\nov announced=%v err=%v\nlg=%d rrcs err=%v\n",
		cy, cyErr, ni, niErr, ov.Announced, ovErr, len(lg.RRCs), lgErr)
	for _, p := range renderNet(ctxv, query, hostIP, explicit, cy, cyErr, ni, niErr, ov, ovErr, lg, lgErr) {
		fmt.Println("--- page ---")
		fmt.Println(p)
	}
	fmt.Println("elapsed:", time.Since(start))
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return s, false
}
