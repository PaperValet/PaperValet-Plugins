package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// The stock wqy-zenhei.ttc has table offsets that are not 4-byte aligned
// (e.g. FFTM at 8579), which x/image's sfnt parser rejects with
// "invalid table offset". repackTTC extracts one font from a TTC (or a
// bare TTF) and rewrites its table directory so every table starts on a
// 4-byte boundary, producing sfnt-clean data.
//
// Layout produced:
//
//	[10 byte sfnt header][16*n table records][tables, each padded to 4]
func repackTTC(src []byte, fontIndex int) ([]byte, error) {
	if len(src) < 12 {
		return nil, fmt.Errorf("font too small (%d bytes)", len(src))
	}
	base := 0
	if string(src[:4]) == "ttcf" {
		n := int(binary.BigEndian.Uint32(src[8:12]))
		if fontIndex < 0 || fontIndex >= n {
			return nil, fmt.Errorf("font collection has %d fonts, want %d", n, fontIndex)
		}
		offs := binary.BigEndian.Uint32(src[12+4*fontIndex:])
		if int(offs)+12 > len(src) {
			return nil, fmt.Errorf("bad font offset %d", offs)
		}
		base = int(offs)
	}
	hdr := src[base : base+12]
	numTables := int(binary.BigEndian.Uint16(hdr[4:6]))
	if numTables == 0 || base+12+16*numTables > len(src) {
		return nil, fmt.Errorf("bad table directory (%d tables)", numTables)
	}

	type rec struct {
		tag      string
		data     []byte
		checksum uint32
	}
	recs := make([]rec, 0, numTables)
	for i := 0; i < numTables; i++ {
		r := src[base+12+16*i : base+12+16*(i+1)]
		off := binary.BigEndian.Uint32(r[8:12])
		length := binary.BigEndian.Uint32(r[12:16])
		if int64(off)+int64(length) > int64(len(src)) {
			return nil, fmt.Errorf("table %q out of range (off %d len %d)", string(r[0:4]), off, length)
		}
		recs = append(recs, rec{
			tag:      string(r[0:4]),
			data:     src[off : off+length],
			checksum: binary.BigEndian.Uint32(r[4:8]),
		})
	}

	out := make([]byte, 0, 12+16*numTables+len(src)-base)
	// sfnt header: keep sfnt version, searchRange/entrySelector/rangeShift
	// stay valid enough for sfnt (it only reads numTables).
	out = append(out, hdr...)
	// table records with recomputed offsets
	bodyLen := 0
	for _, r := range recs {
		bodyLen += (len(r.data) + 3) &^ 3
	}
	off := 12 + 16*numTables
	head := make([]byte, 16*numTables)
	for i, r := range recs {
		binary.BigEndian.PutUint32(head[16*i:], binary.BigEndian.Uint32([]byte(r.tag)[:4]))
		binary.BigEndian.PutUint32(head[16*i+4:], r.checksum)
		binary.BigEndian.PutUint32(head[16*i+8:], uint32(off))
		binary.BigEndian.PutUint32(head[16*i+12:], uint32(len(r.data)))
		off += (len(r.data) + 3) &^ 3
	}
	_ = bodyLen
	out = append(out, head...)
	for _, r := range recs {
		out = append(out, r.data...)
		for pad := (4 - len(r.data)%4) % 4; pad > 0; pad-- {
			out = append(out, 0)
		}
	}
	return out, nil
}

// loadFontFile reads fontPath and repacks it for sfnt.
func loadFontFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return repackTTC(b, 0)
}
