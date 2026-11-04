package store_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/store"
)

func pngOf(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

func gifOf(w, h int) []byte {
	var b bytes.Buffer
	gif.Encode(&b, image.NewPaletted(image.Rect(0, 0, w, h), []color.Color{color.White, color.Black}), nil)
	return b.Bytes()
}

// webpOf builds a minimal VP8X WebP header of the given size.
func webpOf(w, h int) []byte {
	b := make([]byte, 30)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 22)
	copy(b[8:], "WEBPVP8X")
	binary.LittleEndian.PutUint32(b[16:], 10)
	put24 := func(x []byte, v int) { x[0], x[1], x[2] = byte(v), byte(v>>8), byte(v>>16) }
	put24(b[24:], w-1)
	put24(b[27:], h-1)
	return b
}

func TestAddMediaValidates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ok := map[string][]byte{"png": demo.Image(0), "gif": gifOf(64, 64), "webp": webpOf(640, 480), "png max side": pngOf(store.MaxImageSide, 4), "png max pixels": pngOf(4096, 4096)}
	for name, data := range ok {
		if _, _, err := f.st.AddMedia(ctx, f.a, data); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	bad := map[string][]byte{
		"empty":        nil,
		"1x1":          pngOf(1, 1),
		"too wide":     pngOf(store.MaxImageSide+1, 4),
		"too many px":  pngOf(4097, 4096),
		"big gif":      gifOf(store.MaxGIFWidth+1, 16),
		"damaged png":  demo.Image(0)[:40],
		"svg":          []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html":         []byte("<!DOCTYPE html><html><body>example</body></html>"),
		"fake webp":    append([]byte("RIFF\x16\x00\x00\x00WEBPVP8 "), make([]byte, 18)...),
		"over 5 MB":    append(demo.Image(0), make([]byte, store.MaxMediaBytes)...),
		"tiny webp":    webpOf(2, 2),
		"text":         []byte("example text"),
		"jpeg garbage": append([]byte{0xff, 0xd8, 0xff}, make([]byte, 64)...),
	}
	for name, data := range bad {
		if _, _, err := f.st.AddMedia(ctx, f.a, data); !errors.Is(err, store.ErrBadInput) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestMediaRefsCannotTraverse(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ref, _, err := f.st.AddMedia(ctx, f.a, demo.Image(0))
	if err != nil || !store.ValidRef(ref) {
		t.Fatalf("%q %v", ref, err)
	}
	if _, data, err := f.st.Media(ctx, ref); err != nil || !bytes.Equal(data, demo.Image(0)) {
		t.Fatal("stored image not returned")
	}
	for _, r := range []string{"", "med_", "../../etc/passwd", "med_" + strings.Repeat("a", 21) + "/", "med_" + strings.Repeat("a", 21) + "%", ref + "x", "MED" + ref[3:], "med_../../../../../../../../x"} {
		if store.ValidRef(r) {
			t.Errorf("%q valid", r)
		}
		if _, _, err := f.st.Media(ctx, r); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%q: %v", r, err)
		}
	}
}

// X allows up to 4 photos or 1 GIF per post; drafts and owner edits are
// held to that.
func TestGIFMustBeAlone(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p, pid, _ := f.st.AddMedia(ctx, f.a, demo.Image(0))
	g, gid, _ := f.st.AddMedia(ctx, f.a, gifOf(32, 32))
	_, _, err := f.st.CreateDraft(ctx, f.a, store.NewDraft{Account: "sample_writer", ProposedAt: t0.Add(time.Hour), Parts: []store.PartIn{{Text: "Example", Media: []store.MediaIn{{Ref: p}, {Ref: g}}}}})
	if !errors.Is(err, store.ErrBadInput) || !strings.Contains(err.Error(), "GIF") {
		t.Fatalf("GIF with a photo accepted: %v", err)
	}
	d, _, err := f.st.CreateDraft(ctx, f.a, store.NewDraft{Account: "sample_writer", ProposedAt: t0.Add(time.Hour), Parts: []store.PartIn{{Text: "Example", Media: []store.MediaIn{{Ref: g, Alt: "Example alt"}}}, {Text: "Example two", Media: []store.MediaIn{{Ref: p}}}}})
	if err != nil {
		t.Fatal(err)
	}
	e := store.Edit{When: t0.Add(time.Hour), Parts: []store.PartIn{{Text: "Example", Media: []store.MediaIn{{ID: gid}, {ID: pid}}}}}
	if err := f.st.Save(ctx, f.owner, d.ID, d.Version, e, false); !errors.Is(err, store.ErrBadInput) {
		t.Fatalf("owner edit mixed a GIF: %v", err)
	}
}

// Each media rate is independent: unset (unverified), "0" (free) or a
// dollar amount; bad values are refused with the env name.
func TestMediaRates(t *testing.T) {
	r, _ := store.ParseRates("0.10", "0.20")
	for _, bad := range []string{"-1", "abc", "1e3", "NaN", "100.01"} {
		if _, err := r.WithMediaRates(bad, ""); err == nil || !strings.Contains(err.Error(), "XPOST_COST_MEDIA_UPLOAD_USD") {
			t.Errorf("upload %q: %v", bad, err)
		}
		if _, err := r.WithMediaRates("", bad); err == nil || !strings.Contains(err.Error(), "XPOST_COST_MEDIA_METADATA_USD") {
			t.Errorf("metadata %q: %v", bad, err)
		}
	}
	cases := []struct {
		up, meta              string
		upM, altM             int64
		upKnown, altKnown     bool
		uploadBlock, altBlock bool
	}{
		{"", "", 0, 0, false, false, true, true},
		{"0", "", 0, 0, true, false, false, true},
		{"", "0", 0, 0, false, true, true, false},
		{"0", "0", 0, 0, true, true, false, false},
		{" 0.25 ", "$0.005", 250_000, 5_000, true, true, false, false},
	}
	for _, c := range cases {
		got, err := r.WithMediaRates(c.up, c.meta)
		if err != nil || got.UploadMicros != c.upM || got.AltMicros != c.altM || got.UploadKnown != c.upKnown || got.AltKnown != c.altKnown || !got.Known {
			t.Errorf("%q %q: %+v %v", c.up, c.meta, got, err)
		}
		sp := store.Spend{Rates: got}
		if (sp.UploadBlock() != "") != c.uploadBlock || (sp.AltBlock() != "") != c.altBlock || len(sp.MediaBlocks()) != btoi(c.uploadBlock)+btoi(c.altBlock) {
			t.Errorf("%q %q: blocks %v", c.up, c.meta, sp.MediaBlocks())
		}
	}
	// Post rates unset stay unset; the media rates don't verify them.
	if got, _ := (store.Rates{}).WithMediaRates("0", "0"); got.Known {
		t.Fatal("media rates made the post rates known")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Alt text calls have their own ledger rows: billed from before the call,
// unknown and interrupted ones count, not-sent and refused ones don't.
func TestAltCallLedger(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.st.Rates = store.Rates{PostMicros: 100_000, LinkPostMicros: 200_000, Known: true, UploadMicros: 0, UploadKnown: true, AltMicros: 10_000, AltKnown: true}
	f.st.BeginUpload(ctx, up("u"), f.now, 0)
	f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: store.UpUploaded, XMediaID: "9", ExpiresAt: f.now.Add(time.Hour)})
	for _, st := range []string{store.UpUploaded, store.UpRetry, store.UpFailed, store.UpUnknown} {
		id, err := f.st.BeginAlt(ctx, "u")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.st.FinishAlt(ctx, id, st); err != nil {
			t.Fatal(err)
		}
		if err := f.st.FinishAlt(ctx, id, st); !errors.Is(err, store.ErrConflict) {
			t.Fatal("alt call finished twice")
		}
	}
	if id, _ := f.st.BeginAlt(ctx, "u"); f.st.FinishAlt(ctx, id, "posted") == nil {
		t.Fatal("bogus status accepted")
	}
	// That last one is still in flight: a crash makes it unknown (billed).
	if _, err := f.st.InterruptedUploads(ctx, 1); err != nil {
		t.Fatal(err)
	}
	sp, err := f.st.MonthSpend(ctx, time.UTC, f.now)
	if err != nil {
		t.Fatal(err)
	}
	// Billed: uploaded, unknown, interrupted. Upload rate 0 is free.
	if sp.Images != 1 || sp.Alts != 3 || sp.UnknownAlts != 2 || sp.Micros != 30_000 {
		t.Fatalf("%+v", sp)
	}
}

func up(key string) store.MediaUpload {
	return store.MediaUpload{DraftID: 1, Account: "sample_writer", Part: 1, Slot: 1, MediaID: 1, IdemKey: key}
}

// The upload state machine: an intent is recorded (and billed) before the
// call; only definite non-uploads, unknowns and expired uploads reopen.
func TestUploadStateMachine(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.st.Rates = store.Rates{PostMicros: 100_000, LinkPostMicros: 200_000, Known: true, UploadMicros: 50_000, UploadKnown: true, AltKnown: true}
	m := 15 * time.Minute
	if err := f.st.BeginUpload(ctx, up("u"), f.now, m); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginUpload(ctx, up("u"), f.now, m); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("in-flight upload reopened: %v", err)
	}
	if err := f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: store.UpUploaded}); !errors.Is(err, store.ErrBadInput) {
		t.Fatal("uploaded without an id")
	}
	exp := f.now.Add(time.Hour)
	if err := f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: store.UpUploaded, XMediaID: "123", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: store.UpFailed}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("finished twice")
	}
	if err := f.st.BeginUpload(ctx, up("u"), f.now, m); !errors.Is(err, store.ErrConflict) {
		t.Fatal("usable upload reopened")
	}
	u, _ := f.st.Upload(ctx, "u")
	if !u.Usable(f.now, m) || u.Usable(exp.Add(-10*time.Minute), m) {
		t.Fatalf("usable wrong: %+v", u)
	}
	if err := f.st.BeginUpload(ctx, up("u"), exp.Add(-10*time.Minute), m); err != nil {
		t.Fatalf("expiring upload not reopened: %v", err)
	}
	for _, st := range []string{store.UpRetry, store.UpFailed, store.UpUnknown} {
		if err := f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: st, Error: "example"}); err != nil {
			t.Fatal(err)
		}
		if err := f.st.BeginUpload(ctx, up("u"), f.now, m); err != nil {
			t.Fatalf("after %s: %v", st, err)
		}
	}
	// Crash: the run died mid-call. The upload becomes unknown, billed.
	n, err := f.st.InterruptedUploads(ctx, 1)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	u, _ = f.st.Upload(ctx, "u")
	if u.Status != store.UpUnknown || u.Attempts != 5 {
		t.Fatalf("%+v", u)
	}
	sp, err := f.st.MonthSpend(ctx, time.UTC, f.now)
	if err != nil {
		t.Fatal(err)
	}
	// Calls: uploaded, then (expired) retry, failed, unknown, interrupted:
	// retry and failed are definite non-uploads; the rest are billed.
	if sp.Images != 3 || sp.UnknownUploads != 2 || sp.Micros != 150_000 {
		t.Fatalf("%+v", sp)
	}
	if err := f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: "posted"}); !errors.Is(err, store.ErrBadInput) {
		t.Fatal("bogus status accepted")
	}
}

// Processing and alt-text updates touch only uploaded images; a failed
// processing makes the image unusable.
func TestUploadProcessingAndAlt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.st.BeginUpload(ctx, up("u"), f.now, 0)
	f.st.FinishUpload(ctx, "u", store.UploadOutcome{Status: store.UpUploaded, XMediaID: "9", ExpiresAt: f.now.Add(time.Hour), Processing: true, CheckAt: f.now.Add(time.Minute)})
	if u, _ := f.st.Upload(ctx, "u"); u.Usable(f.now, 0) || !u.Processing {
		t.Fatal("processing image usable")
	}
	f.st.UploadProcessed(ctx, "u", false, time.Time{}, "")
	f.st.UploadAltDone(ctx, "u")
	if u, _ := f.st.Upload(ctx, "u"); !u.Usable(f.now, 0) || !u.AltDone {
		t.Fatalf("%+v", u)
	}
	f.st.UploadProcessed(ctx, "u", false, time.Time{}, "X could not process the image")
	if u, _ := f.st.Upload(ctx, "u"); u.Usable(f.now, 0) || u.Status != store.UpFailed {
		t.Fatalf("%+v", u)
	}
}

// NotPosted never hides a posted, in-flight or unknown part.
func TestNotPostedGuards(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e := store.LogEntry{DraftID: 1, Account: "sample_writer", Part: 1, Parts: 1, IdemKey: "k", Images: 2}
	if err := f.st.NotPosted(ctx, e); !errors.Is(err, store.ErrBadInput) {
		t.Fatal("no status accepted")
	}
	e.Status = store.LogRetry
	if err := f.st.NotPosted(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.Status = store.LogFailed
	if err := f.st.NotPosted(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginPart(ctx, e); err != nil {
		t.Fatalf("not-posted key could not be reopened: %v", err)
	}
	if err := f.st.NotPosted(ctx, e); !errors.Is(err, store.ErrConflict) {
		t.Fatal("hid an in-flight part")
	}
	for _, st := range []string{store.LogUnknown, store.LogPosted} {
		f.st.FinishPart(ctx, "k", store.Outcome{Status: st, PostID: "1"})
		if err := f.st.NotPosted(ctx, e); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("hid a %s part", st)
		}
	}
	r, _ := f.st.LogByKey(ctx, "k")
	if r.Images != 2 || r.Attempts != 3 {
		t.Fatalf("%+v", r)
	}
}
