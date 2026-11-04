package xauth_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/poster"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
	"github.com/scott/xpost/internal/xauth/xfake"
)

func demoGIF() []byte {
	img := image.NewPaletted(image.Rect(0, 0, 16, 16), []color.Color{color.White, color.Black})
	var b bytes.Buffer
	gif.Encode(&b, img, nil)
	return b.Bytes()
}

// Upload, alt text and a post with media ids, through real HTTP to the
// fake: categories, expiry and alt text land where X would put them.
func TestXPosterUploadsAndPostsWithImages(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	p := &xauth.XPoster{M: f.m}
	ctx := context.Background()
	png, err := p.Upload(ctx, poster.Upload{Account: "sample_writer", Mime: "image/png", Data: demo.Image(0)})
	if err != nil {
		t.Fatal(err)
	}
	if png.MediaID == "" || !png.ExpiresAt.Equal(f.clock().Add(24*time.Hour)) || png.Processing {
		t.Fatalf("%+v", png)
	}
	g, err := p.Upload(ctx, poster.Upload{Account: "sample_writer", Mime: "image/gif", Data: demoGIF()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetAlt(ctx, "sample_writer", png.MediaID, "Example alt"); err != nil {
		t.Fatal(err)
	}
	m := f.fake.AllMedia()
	if len(m) != 2 || m[0].Category != "tweet_image" || m[1].Category != "tweet_gif" || m[0].Alt != "Example alt" {
		t.Fatalf("%+v", m)
	}
	if _, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example", MediaIDs: []string{png.MediaID, g.MediaID}}); kind(t, err) != poster.Refused {
		t.Fatal("X allows 4 photos or 1 GIF, not both")
	}
	r, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example", MediaIDs: []string{png.MediaID}})
	if err != nil {
		t.Fatal(err)
	}
	posts := f.fake.Posts()
	if len(posts) != 1 || posts[0].ID != r.PostID || len(posts[0].Media) != 1 || posts[0].Alts[0] != "Example alt" {
		t.Fatalf("%+v", posts)
	}
	if strings.Contains(f.logs.String(), "fake-at-") {
		t.Fatal("token logged")
	}
}

func TestXPosterUploadClassifiesFailures(t *testing.T) {
	reset := t0.Add(9 * time.Minute)
	cases := []struct {
		name  string
		fault xfake.Fault
		want  poster.Kind
		held  int // uploads the fake holds afterwards
	}{
		{"429", xfake.Fault{Kind: xfake.RateLimit, Reset: reset}, poster.RateLimited, 0},
		{"400", xfake.Fault{Kind: xfake.Status, Status: 400}, poster.Refused, 0},
		{"503", xfake.Fault{Kind: xfake.Status, Status: 503}, poster.Ambiguous, 0},
		{"drop after store", xfake.Fault{Kind: xfake.DropAfterCreate}, poster.Ambiguous, 1},
		{"drop before store", xfake.Fault{Kind: xfake.DropBeforeCreate}, poster.Ambiguous, 0},
		{"garbled", xfake.Fault{Kind: xfake.Garbled}, poster.Ambiguous, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.connect("sample_writer")
			f.fake.QueueMedia(xfake.UploadCall, xfake.MediaFault{Fault: c.fault})
			_, err := (&xauth.XPoster{M: f.m}).Upload(context.Background(), poster.Upload{Account: "sample_writer", Mime: "image/png", Data: demo.Image(1)})
			if k := kind(t, err); k != c.want {
				t.Fatalf("kind %v want %v (%v)", k, c.want, err)
			}
			if n := len(f.fake.AllMedia()); n != c.held {
				t.Fatalf("fake holds %d", n)
			}
			var pe *poster.Error
			errors.As(err, &pe)
			if c.want == poster.RateLimited && !pe.RetryAt.Equal(reset) {
				t.Fatalf("retry at %v", pe.RetryAt)
			}
			if c.want == poster.Ambiguous && !strings.Contains(err.Error(), "upload") {
				t.Fatalf("message %q", err)
			}
		})
	}
}

// A slow upload past the client timeout is ambiguous.
func TestXPosterSlowUploadIsAmbiguous(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	f.c.HTTP.Timeout = 200 * time.Millisecond
	f.fake.QueueMedia(xfake.UploadCall, xfake.MediaFault{Fault: xfake.Fault{Kind: xfake.Hang}})
	_, err := (&xauth.XPoster{M: f.m}).Upload(context.Background(), poster.Upload{Account: "sample_writer", Mime: "image/png", Data: demo.Image(1)})
	if kind(t, err) != poster.Ambiguous {
		t.Fatal(err)
	}
}

// An expired token gets one refresh and one retry for uploads too.
func TestXPosterUploadRefreshesOn401(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	f.fake.ExpireAccess("sample_writer")
	if _, err := (&xauth.XPoster{M: f.m}).Upload(context.Background(), poster.Upload{Account: "sample_writer", Mime: "image/png", Data: demo.Image(1)}); err != nil {
		t.Fatal(err)
	}
	if f.fake.UploadCalls != 2 || len(f.fake.AllMedia()) != 1 || f.fake.RefreshCalls != 1 {
		t.Fatalf("calls %d media %d refreshes %d", f.fake.UploadCalls, len(f.fake.AllMedia()), f.fake.RefreshCalls)
	}
}

// Processing: pending until checked enough, then succeeded; failed is
// reported as such.
func TestXPosterMediaStatus(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	p := &xauth.XPoster{M: f.m}
	ctx := context.Background()
	f.fake.QueueMedia(xfake.UploadCall, xfake.MediaFault{Process: true, Checks: 2}, xfake.MediaFault{Process: true, FailProcess: true, Checks: 1})
	u, err := p.Upload(ctx, poster.Upload{Account: "sample_writer", Mime: "image/gif", Data: demoGIF()})
	if err != nil || !u.Processing || !u.CheckAt.After(f.clock()) {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example", MediaIDs: []string{u.MediaID}}); kind(t, err) != poster.Refused {
		t.Fatal("the fake let a post use media still processing")
	}
	s1, _ := p.MediaStatus(ctx, "sample_writer", u.MediaID)
	s2, err := p.MediaStatus(ctx, "sample_writer", u.MediaID)
	if err != nil || !s1.Processing || s2.Processing || s2.Failed != "" {
		t.Fatalf("%+v %+v %v", s1, s2, err)
	}
	bad, _ := p.Upload(ctx, poster.Upload{Account: "sample_writer", Mime: "image/gif", Data: demoGIF()})
	if s, _ := p.MediaStatus(ctx, "sample_writer", bad.MediaID); s.Failed == "" {
		t.Fatalf("%+v", s)
	}
}

// The fake refuses media that is expired or belongs to another account,
// and refuses uploads without media.write.
func TestFakeMediaRules(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	f.connect("sample_maker")
	p := &xauth.XPoster{M: f.m}
	ctx := context.Background()
	u, err := p.Upload(ctx, poster.Upload{Account: "sample_maker", Mime: "image/png", Data: demo.Image(2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example", MediaIDs: []string{u.MediaID}}); kind(t, err) != poster.Refused {
		t.Fatal("used another account's media")
	}
	f.fake.ExpireMedia()
	if _, err := p.Post(ctx, poster.Request{Account: "sample_maker", Text: "Example", MediaIDs: []string{u.MediaID}}); kind(t, err) != poster.Refused {
		t.Fatal("used expired media")
	}
	if err := p.SetAlt(ctx, "sample_maker", u.MediaID, "Example"); kind(t, err) != poster.Refused {
		t.Fatal("alt on expired media")
	}
	if _, err := p.Upload(ctx, poster.Upload{Account: "sample_writer", Mime: "image/svg+xml", Data: []byte("<svg/>")}); kind(t, err) != poster.Refused || f.fake.UploadCalls != 1 {
		t.Fatalf("sent an unsupported type: %v calls %d", err, f.fake.UploadCalls)
	}
	if err := p.SetAlt(ctx, "sample_maker", "../1", "Example"); kind(t, err) != poster.Refused {
		t.Fatal("bad media id sent")
	}
}

// ---- Dispatcher + XPoster + fake X, with images ----

func (d *dfx) imgDraft(t *testing.T, account string, at time.Time, parts ...[]string) int64 {
	t.Helper()
	ctx := context.Background()
	var in []store.PartIn
	for i, p := range parts {
		pi := store.PartIn{Text: p[0]}
		for j, alt := range p[1:] {
			ref, _, err := d.st.AddMedia(ctx, d.tok, demo.Image(i+j))
			if err != nil {
				t.Fatal(err)
			}
			pi.Media = append(pi.Media, store.MediaIn{Ref: ref, Alt: alt})
		}
		in = append(in, pi)
	}
	dr, _, err := d.st.CreateDraft(ctx, d.tok, store.NewDraft{Account: account, Parts: in, ProposedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.st.Approve(ctx, d.owner, dr.ID, dr.Version); err != nil {
		t.Fatal(err)
	}
	return dr.ID
}

// A thread with images on two parts, with an upload whose reply is lost
// (the fake stored it) and an alt call that is rate limited: the thread
// still posts exactly once, every image on its part with its alt text.
func TestDispatchThreadWithImagesToFakeX(t *testing.T) {
	d := dsetup(t)
	d.st.Rates.UploadMicros, d.st.Rates.UploadKnown, d.st.Rates.AltMicros, d.st.Rates.AltKnown = 500_000, true, 100_000, true // invented
	id := d.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), []string{"Example one", "Example alt one", "Example alt two"}, []string{"Example two"}, []string{"Example three", ""})
	d.fake.QueueMedia(xfake.UploadCall, xfake.MediaFault{}, xfake.MediaFault{Fault: xfake.Fault{Kind: xfake.DropAfterCreate}})
	d.fake.QueueMedia(xfake.MetadataCall, xfake.MediaFault{Fault: xfake.Fault{Kind: xfake.RateLimit, Reset: t0.Add(25 * time.Minute)}})
	d.advance(20 * time.Minute)
	if rep := d.run(t); rep.Waiting != 1 || len(d.fake.Posts()) != 0 {
		t.Fatalf("%+v", rep)
	}
	for i := 0; i < 10 && d.get(t, id).Status != store.StatusPosted; i++ {
		d.advance(5 * time.Minute)
		d.run(t)
	}
	if st := d.get(t, id).Status; st != store.StatusPosted {
		t.Fatalf("status %s: %s", st, d.get(t, id).Error)
	}
	chain(t, d.fake.Posts(), "Example one", "Example two", "Example three")
	posts := d.fake.Posts()
	if len(posts[0].Media) != 2 || posts[0].Alts[0] != "Example alt one" || posts[0].Alts[1] != "Example alt two" || len(posts[1].Media) != 0 || len(posts[2].Media) != 1 {
		t.Fatalf("%+v", posts)
	}
	// One upload's reply was lost, so the fake holds one extra (unused)
	// upload, and the spend meter counted both calls (the lost one as
	// unknown, billed).
	if n := len(d.fake.AllMedia()); n != 4 {
		t.Fatalf("fake holds %d uploads", n)
	}
	sp, _ := d.st.MonthSpend(context.Background(), time.UTC, d.clock())
	if sp.Images != 4 || sp.UnknownUploads != 1 {
		t.Fatalf("%+v", sp)
	}
}

// The image upload fails for good mid-thread: part 1 is on X, part 2 is
// not; Retry posts part 2 only, as a reply to part 1.
func TestDispatchUploadRefusedMidThreadThenRetry(t *testing.T) {
	d := dsetup(t)
	d.st.Rates.UploadMicros, d.st.Rates.UploadKnown, d.st.Rates.AltMicros, d.st.Rates.AltKnown = 500_000, true, 100_000, true
	ctx := context.Background()
	id := d.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), []string{"Example one"}, []string{"Example two", ""})
	d.fake.QueueMedia(xfake.UploadCall, xfake.MediaFault{Fault: xfake.Fault{Kind: xfake.Status, Status: 400}})
	d.advance(20 * time.Minute)
	if rep := d.run(t); rep.Failed != 1 {
		t.Fatalf("%+v", rep)
	}
	if len(d.fake.Posts()) != 1 {
		t.Fatal("part 2 posted without its image")
	}
	dr := d.get(t, id)
	if err := d.st.RetryDraft(ctx, d.owner, id, dr.Version); err != nil {
		t.Fatal(err)
	}
	if rep := d.run(t); rep.Posted != 1 {
		t.Fatalf("%+v", rep)
	}
	chain(t, d.fake.Posts(), "Example one", "Example two")
}
