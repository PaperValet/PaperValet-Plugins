package main

// font.go — wqy TTC loading, PNG encoding, tg entity helpers.

import (
	"encoding/binary"
	"image"
	"image/png"
	"io"
	"os"

	"github.com/gotd/td/tg"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// openWQY parses the wqy-zenhei TTC and returns its first (Zen Hei) face.
// sfnt.ParseCollection rejects this legacy TTC build, so font 0 is
// relocated into a standalone TTF buffer first: the SFNT header and table
// directory are copied, every table body is appended (4-byte aligned) and
// the directory offsets are rewritten.
func openWQY() (*sfnt.Font, error) {
	data, err := os.ReadFile(wqyFontPath)
	if err != nil {
		return nil, err
	}
	ttf, err := ttcFont0(data)
	if err != nil {
		return nil, err
	}
	return sfnt.Parse(ttf)
}

// ttcFont0 relocates font 0 of a TTC into a self-contained TTF slice.
func ttcFont0(data []byte) ([]byte, error) {
	if len(data) < 12 || string(data[:4]) != "ttcf" {
		return data, nil // already a plain font
	}
	if len(data) < 16 {
		return nil, sfnt.ErrNotFound
	}
	off := int(binary.BigEndian.Uint32(data[12:16]))
	numTables := int(binary.BigEndian.Uint16(data[off+4 : off+6]))
	dirEnd := off + 12 + 16*numTables
	if off < 0 || dirEnd > len(data) {
		return nil, sfnt.ErrNotFound
	}
	type tbl struct {
		tag      string
		checksum uint32
		srcOff   int
		length   int
	}
	tables := make([]tbl, numTables)
	for i := range tables {
		e := off + 12 + 16*i
		tables[i] = tbl{
			tag:      string(data[e : e+4]),
			checksum: binary.BigEndian.Uint32(data[e+4 : e+8]),
			srcOff:   int(binary.BigEndian.Uint32(data[e+8 : e+12])),
			length:   int(binary.BigEndian.Uint32(data[e+12 : e+16])),
		}
		if tables[i].srcOff+tables[i].length > len(data) {
			return nil, sfnt.ErrNotFound
		}
	}
	var res, dir, body []byte
	res = append(res, data[off:off+12]...)
	pos := 12 + 16*numTables
	for _, tb := range tables {
		dir = append(dir, tb.tag...)
		var b [12]byte
		binary.BigEndian.PutUint32(b[0:4], tb.checksum)
		binary.BigEndian.PutUint32(b[4:8], uint32(pos))
		binary.BigEndian.PutUint32(b[8:12], uint32(tb.length))
		dir = append(dir, b[:]...)
		body = append(body, data[tb.srcOff:tb.srcOff+tb.length]...)
		for pad := (4 - tb.length%4) % 4; pad > 0; pad-- {
			body = append(body, 0)
		}
		pos += tb.length + (4-tb.length%4)%4
	}
	res = append(res, dir...)
	res = append(res, body...)
	return res, nil
}

// textFace caches the parsed wqy font and its size-specific faces.
type textFace struct {
	ttf   *sfnt.Font
	faces map[int]font.Face
}

// faceCache is the shared face cache; loadFonts fills ttf.
var faceCache = &textFace{faces: map[int]font.Face{}}

func (t *textFace) face(size int) font.Face {
	if f, ok := t.faces[size]; ok {
		return f
	}
	if t.ttf == nil {
		return nil
	}
	f, err := opentype.NewFace(t.ttf, &opentype.FaceOptions{Size: float64(size), DPI: 72})
	if err != nil {
		return nil
	}
	t.faces[size] = f
	return f
}

// encodePNG writes img to w as PNG.
func encodePNG(w io.Writer, img image.Image) error {
	return png.Encode(w, img)
}

// entityKindOf maps a tg entity to our internal style kind.
func entityKindOf(e tg.MessageEntityClass) string {
	switch e.(type) {
	case *tg.MessageEntityBold:
		return "bold"
	case *tg.MessageEntityItalic:
		return "italic"
	case *tg.MessageEntityUnderline:
		return "underline"
	case *tg.MessageEntityStrike:
		return "strikethrough"
	case *tg.MessageEntityCode:
		return "code"
	case *tg.MessageEntityPre:
		return "pre"
	case *tg.MessageEntitySpoiler:
		return "spoiler"
	case *tg.MessageEntityCustomEmoji:
		return "" // rendered via emoji font when possible; else plain
	case *tg.MessageEntityBlockquote:
		return ""
	}
	return ""
}

// entityOffset/entityLength extract UTF-16 offsets from a tg entity.
func entityOffset(e tg.MessageEntityClass) int {
	if o, ok := e.(interface{ GetOffset() int }); ok {
		return o.GetOffset()
	}
	return 0
}

func entityLength(e tg.MessageEntityClass) int {
	if l, ok := e.(interface{ GetLength() int }); ok {
		return l.GetLength()
	}
	return 0
}
