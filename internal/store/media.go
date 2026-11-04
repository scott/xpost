package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // DecodeConfig for the allowed types
	_ "image/jpeg"
	_ "image/png"
	"net/http"

	"github.com/scott/xpost/internal/auth"
)

// Image limits, kept inside X's (images: JPG, PNG, GIF, WEBP up to 5 MB;
// animated GIFs up to 1280x1080 for a reliable post; 4 photos or 1 GIF per
// post). Checked on upload so a bad file is refused at intake, not at
// post time.
const (
	MinImageSide = 4
	MaxImageSide = 8192
	MaxGIFWidth  = 1280
	MaxGIFHeight = 1080
	// MaxPixels bounds the full decode that catches truncated or damaged
	// PNG, JPEG and GIF files (about 64 MB of RGBA at most).
	MaxPixels = 4096 * 4096
)

// ImageSize reads an image's dimensions from its header without decoding
// the pixels. mime must be one of the allowed types.
func ImageSize(mime string, data []byte) (w, h int, err error) {
	if mime == "image/webp" {
		return webpSize(data)
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return c.Width, c.Height, nil
}

// webpSize parses a WebP header (RIFF, then a VP8, VP8L or VP8X chunk).
func webpSize(b []byte) (int, int, error) {
	bad := errors.New("not a valid WebP image")
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, bad
	}
	if n := int(binary.LittleEndian.Uint32(b[4:8])); n < 22 || n+8 > len(b) {
		return 0, 0, bad
	}
	p := b[20:]
	le24 := func(x []byte) int { return int(x[0]) | int(x[1])<<8 | int(x[2])<<16 }
	switch string(b[12:16]) {
	case "VP8 ":
		if p[3] != 0x9d || p[4] != 0x01 || p[5] != 0x2a {
			return 0, 0, bad
		}
		return int(binary.LittleEndian.Uint16(p[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(p[8:10]) & 0x3fff), nil
	case "VP8L":
		if p[0] != 0x2f {
			return 0, 0, bad
		}
		bits := binary.LittleEndian.Uint32(p[1:5])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	case "VP8X":
		return le24(p[4:7]) + 1, le24(p[7:10]) + 1, nil
	}
	return 0, 0, bad
}

// checkImage validates an upload: allowed type (sniffed), readable header,
// sane dimensions. It returns the type.
func checkImage(data []byte) (string, error) {
	mime := SniffImage(data)
	if mime == "" {
		return "", badInput("only PNG, JPEG, GIF and WebP images are accepted")
	}
	w, h, err := ImageSize(mime, data)
	if err != nil {
		return "", badInput("the image could not be read; it may be damaged")
	}
	if w < MinImageSide || h < MinImageSide || w > MaxImageSide || h > MaxImageSide {
		return "", badInput(fmt.Sprintf("images must be between %d and %d pixels on each side (this one is %dx%d)", MinImageSide, MaxImageSide, w, h))
	}
	if w*h > MaxPixels {
		return "", badInput(fmt.Sprintf("images can be up to %d megapixels (this one is %dx%d)", MaxPixels/1_000_000, w, h))
	}
	if mime == "image/gif" && (w > MaxGIFWidth || h > MaxGIFHeight) {
		return "", badInput(fmt.Sprintf("GIFs can be up to %dx%d (this one is %dx%d)", MaxGIFWidth, MaxGIFHeight, w, h))
	}
	// Decode the pixels too (the first frame of a GIF): a header can be
	// fine while the rest is cut off. Go has no WebP decoder, so WebP is
	// checked by its header only; X refuses a damaged one at upload.
	if mime != "image/webp" {
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return "", badInput("the image could not be read; it may be damaged")
		}
	}
	return mime, nil
}

// checkMediaMix enforces X's per-post rule: up to 4 photos, or 1 GIF on
// its own. Every GIF is treated as animated.
func checkMediaMix(ctx context.Context, tx *sql.Tx, parts []PartIn) error {
	for i, p := range parts {
		gifs := 0
		for _, m := range p.Media {
			var mime string
			if err := tx.QueryRowContext(ctx, `SELECT mime FROM media WHERE id = ?`, m.ID).Scan(&mime); err != nil {
				return badInput(fmt.Sprintf("part %d: unknown image", i+1))
			}
			if mime == "image/gif" {
				gifs++
			}
		}
		if gifs > 0 && len(p.Media) > 1 {
			return badInput(fmt.Sprintf("part %d: a GIF must be the only image in its part (X allows up to 4 photos or 1 GIF)", i+1))
		}
	}
	return nil
}

// Allowed image types (sniffed, never trusted from the client).
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// SniffImage returns the image type of data, or "" if it is not an allowed
// image.
func SniffImage(data []byte) string {
	t := http.DetectContentType(data)
	if imageTypes[t] {
		return t
	}
	return ""
}

// AddMedia stores an image. tok is the uploading agent, nil for the owner.
func (s *Store) AddMedia(ctx context.Context, tok *AgentToken, data []byte) (string, int64, error) {
	if len(data) == 0 {
		return "", 0, badInput("the file is empty")
	}
	if len(data) > MaxMediaBytes {
		return "", 0, badInput("images can be up to 5 MB")
	}
	mime, err := checkImage(data)
	if err != nil {
		return "", 0, err
	}
	raw, err := auth.NewToken()
	if err != nil {
		return "", 0, err
	}
	ref := "med_" + raw[:22]
	var tid any
	if tok != nil {
		tid = tok.ID
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO media(ref, token_id, mime, size, data, created_at) VALUES (?,?,?,?,?,?)`, ref, tid, mime, len(data), data, s.now())
	if err != nil {
		return "", 0, err
	}
	id, _ := res.LastInsertId()
	return ref, id, nil
}

// ValidRef reports whether ref has the shape AddMedia gives refs. Anything
// else is refused before it reaches a query.
func ValidRef(ref string) bool {
	if len(ref) != 26 || ref[:4] != "med_" {
		return false
	}
	for _, c := range ref[4:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Media returns an image's bytes for the owner's browser.
func (s *Store) Media(ctx context.Context, ref string) (string, []byte, error) {
	if !ValidRef(ref) {
		return "", nil, ErrNotFound
	}
	var mime string
	var data []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT mime, data FROM media WHERE ref = ?`, ref).Scan(&mime, &data); err != nil {
		return "", nil, ErrNotFound
	}
	return mime, data, nil
}
