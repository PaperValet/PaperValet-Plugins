package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============================================================
// HTTP front end
// ============================================================

// client is the shared HTTP front end for RIPEstat and bgp.tools.
type client struct {
	h *http.Client
}

func newClient(timeout time.Duration) *client {
	return &client{h: &http.Client{Timeout: timeout}}
}

// getRaw fetches url with the given headers and returns the body bytes.
func (c *client) getRaw(ctx context.Context, url string, headers map[string]string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.h.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return data, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return data, nil
}

// getJSON fetches a RIPEstat data call and decodes its "data" object into
// out. It returns the top-level status code.
func (c *client) getJSON(ctx context.Context, url string, out any) (int, error) {
	data, err := c.getRaw(ctx, url, map[string]string{
		"User-Agent": "PaperValet-BGP/1.0",
		"Accept":     "application/json",
	}, 8<<20)
	if err != nil {
		return 0, err
	}
	var env struct {
		Status int             `json:"status_code"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, err
	}
	if env.Data != nil && out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return env.Status, err
		}
	}
	return env.Status, nil
}

// ============================================================
// Target parsing
// ============================================================

// parseASN extracts an AS number from text. "AS15169"/"as15169" is accepted
// anywhere; a bare number only when it is the entire input, so prose like
// "port 53" never matches.
func parseASN(text string) (int, bool) {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == '@' || r == '#'
	})
	for _, f := range fields {
		low := strings.ToLower(strings.TrimSpace(f))
		if low == "" {
			continue
		}
		explicit := strings.HasPrefix(low, "as")
		s := strings.TrimPrefix(strings.TrimPrefix(low, "as"), "-")
		if s == "" || !allDigits(s) {
			continue
		}
		n, _ := strconv.Atoi(s)
		if n <= 0 || n > 4294967295 {
			continue
		}
		if explicit || len(fields) == 1 {
			return n, true
		}
	}
	return 0, false
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// extractIP returns the first IPv4/IPv6 address in text, or "". A CIDR
// prefix yields its network address, like the TeleBox source did.
func extractIP(text string) string {
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			return false
		case r == '.' || r == ':' || r == '/' || r == '[' || r == ']':
			return false
		}
		return true
	}) {
		cand := strings.TrimSuffix(strings.TrimPrefix(f, "["), "]")
		if ip := net.ParseIP(cand); ip != nil {
			return ip.String()
		}
		if h, _, err := net.SplitHostPort(f); err == nil {
			if ip := net.ParseIP(h); ip != nil {
				return ip.String()
			}
		}
		if i := strings.IndexByte(cand, '/'); i > 0 {
			if ip := net.ParseIP(cand[:i]); ip != nil {
				return ip.String()
			}
		}
	}
	return ""
}

// ============================================================
// Team Cymru whois (port 43)
// ============================================================

const cymruAddr = "whois.cymru.com:43"

type cymruRecord struct {
	AS        string
	IP        string
	Prefix    string
	CC        string
	Registry  string
	Allocated string
	ASName    string
}

// isZero reports whether the record is an empty "no match" row.
func (r cymruRecord) isZero() bool { return r.AS == "" || r.AS == "NA" }

// cymruLookup runs a bulk verbose whois query for one IP, prefix or ASN.
func cymruLookup(ctx context.Context, query string) (cymruRecord, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", cymruAddr)
	if err != nil {
		return cymruRecord{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := conn.Write([]byte("begin\r\nverbose\r\n" + query + "\r\nend\r\n")); err != nil {
		return cymruRecord{}, err
	}
	sc := bufio.NewScanner(io.LimitReader(conn, 1<<20))
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if !strings.Contains(t, "|") {
			continue // banner, blank line or error text
		}
		first := strings.TrimSpace(t[:strings.IndexByte(t, '|')])
		if !allDigits(first) {
			continue // the "AS | IP | ..." header row
		}
		rec := parseCymruLine(t)
		if rec.isZero() {
			break
		}
		return rec, nil
	}
	return cymruRecord{}, errors.New("no ASN or IP match")
}

// parseCymruLine splits one verbose row. Two shapes exist:
// AS|IP|Prefix|CC|Registry|Allocated|ASName for IP/prefix queries and
// AS|CC|Registry|Allocated|ASName for AS queries.
func parseCymruLine(line string) cymruRecord {
	f := strings.Split(line, "|")
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	var rec cymruRecord
	if len(f) >= 7 {
		rec.AS, rec.IP, rec.Prefix, rec.CC, rec.Registry, rec.Allocated, rec.ASName =
			f[0], f[1], f[2], f[3], f[4], f[5], f[6]
	} else if len(f) >= 5 {
		rec.AS, rec.CC, rec.Registry, rec.Allocated, rec.ASName = f[0], f[1], f[2], f[3], f[4]
	}
	return rec
}

// asHolderName trims the registry slug from a Cymru/RIPE holder such as
// "CLOUDFLARENET - Cloudflare, Inc., US" down to "Cloudflare, Inc., US".
func asHolderName(name string) string {
	if i := strings.Index(name, " - "); i >= 0 {
		return strings.TrimSpace(name[i+3:])
	}
	return name
}

// ============================================================
// RIPEstat
// ============================================================

const ripeBase = "https://stat.ripe.net/data"

type ripeASN struct {
	ASN    int    `json:"asn"`
	Holder string `json:"holder"`
}

type prefixOverview struct {
	Announced bool      `json:"announced"`
	ASNs      []ripeASN `json:"asns"`
	Resource  string    `json:"resource"`
	Block     struct {
		Resource string `json:"resource"`
		Desc     string `json:"desc"`
		Name     string `json:"name"`
	} `json:"block"`
	Related []string `json:"related_prefixes"`
}

type networkInfo struct {
	ASNs   []string `json:"asns"`
	Prefix string   `json:"prefix"`
}

type asOverview struct {
	Resource string `json:"resource"`
	Holder   string `json:"holder"`
	Block    struct {
		Resource string `json:"resource"`
		Desc     string `json:"desc"`
	} `json:"block"`
}

type neighbourCounts struct {
	Left   int `json:"left"`
	Right  int `json:"right"`
	Unique int `json:"unique"`
	Uncert int `json:"uncertain"`
}

type neighboursData struct {
	Counts neighbourCounts `json:"neighbour_counts"`
}

type announcedPrefixes struct {
	Prefixes []struct {
		Prefix string `json:"prefix"`
	} `json:"prefixes"`
}

type lgPeer struct {
	ASPath  string `json:"as_path"`
	Origin  string `json:"origin"`
	Peer    string `json:"peer"`
	Updated string `json:"last_updated"`
}

type lgRRC struct {
	RRC      string   `json:"rrc"`
	Location string   `json:"location"`
	Peers    []lgPeer `json:"peers"`
}

type lookingGlass struct {
	RRCs []lgRRC `json:"rrcs"`
}

type dnsChain struct {
	Forward map[string][]string `json:"forward_nodes"`
	Reverse map[string][]string `json:"reverse_nodes"`
	AuthNS  []string            `json:"authoritative_nameservers"`
}

// ripeGet runs one RIPEstat data call into out.
func (c *client) ripeGet(ctx context.Context, call, resource string, out any) error {
	u := fmt.Sprintf("%s/%s/data.json?resource=%s", ripeBase, call, resource)
	code, err := c.getJSON(ctx, u, out)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("HTTP status %d", code)
	}
	return nil
}

// ============================================================
// AS path statistics
// ============================================================

// pathStat is one distinct transit path (origin stripped) and how many RIS
// peers observed it.
type pathStat struct {
	Path  string
	Count int
}

// collectPeers flattens the looking-glass RRC list.
func collectPeers(rrcs []lgRRC) []lgPeer {
	var out []lgPeer
	for _, r := range rrcs {
		out = append(out, r.Peers...)
	}
	return out
}

// dedupePaths groups paths by their transit AS sequence (origin and
// prepends removed) and counts observers.
func dedupePaths(peers []lgPeer, origin int) []pathStat {
	seen := map[string]*pathStat{}
	var order []string
	for _, p := range peers {
		asns := strings.Fields(p.ASPath)
		var clean []string
		for _, a := range asns {
			if n, err := strconv.Atoi(a); err == nil && n == origin && len(clean) == 0 {
				continue // peer itself is the origin
			}
			if len(clean) == 0 || clean[len(clean)-1] != a { // drop prepends
				clean = append(clean, a)
			}
		}
		if len(clean) > 0 {
			if n, err := strconv.Atoi(clean[len(clean)-1]); err == nil && n == origin {
				clean = clean[:len(clean)-1]
			}
		}
		if len(clean) == 0 {
			continue
		}
		key := strings.Join(clean, " ")
		if s, ok := seen[key]; ok {
			s.Count++
			continue
		}
		seen[key] = &pathStat{Path: key, Count: 1}
		order = append(order, key)
	}
	out := make([]pathStat, 0, len(order))
	for _, k := range order {
		out = append(out, *seen[k])
	}
	return out
}

// topPaths returns the n most observed paths.
func topPaths(stats []pathStat, n int) []pathStat {
	sort.SliceStable(stats, func(i, j int) bool { return stats[i].Count > stats[j].Count })
	if len(stats) > n {
		stats = stats[:n]
	}
	return stats
}
