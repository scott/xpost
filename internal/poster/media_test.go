package poster

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/store"
)

// imgPart is a part for imgDraft: text plus n demo images with alt texts.
type imgPart struct {
	text string
	alts []string // one per image ("" for none)
}

func (f *fx) imgDraft(t *testing.T, account string, at time.Time, parts ...imgPart) int64 {
	t.Helper()
	ctx := context.Background()
	var in []store.PartIn
	for i, p := range parts {
		pi := store.PartIn{Text: p.text}
		for j, alt := range p.alts {
			ref, _, err := f.st.AddMedia(ctx, f.tok, demo.Image(i+j))
			if err != nil {
				t.Fatal(err)
			}
			pi.Media = append(pi.Media, store.MediaIn{Ref: ref, Alt: alt})
		}
		in = append(in, pi)
	}
	d, _, err := f.st.CreateDraft(ctx, f.tok, store.NewDraft{Account: account, Parts: in, ProposedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func (f *fx) fake() *Fake { return &Fake{Now: func() time.Time { return f.now }} }

func (f *fx) uploads(t *testing.T, id int64) map[string]*store.MediaUpload {
	t.Helper()
	u, err := f.st.DraftUploads(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *fx) spend(t *testing.T) store.Spend {
	t.Helper()
	sp, err := f.st.MonthSpend(context.Background(), time.UTC, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// Images upload before their part, get their alt text, and are attached in
// order; a thread's images go on the right parts and the reply chain holds.
func TestImagesUploadThenPost(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute),
		imgPart{"Example one", []string{"Example alt one", ""}}, imgPart{"Example two", nil}, imgPart{"Example three", []string{"Example alt three"}})
	fake := f.fake()
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, f.disp(fake)); rep.Posted != 1 {
		t.Fatalf("%+v", rep)
	}
	ups, calls := fake.Uploads(), fake.Calls()
	if len(ups) != 3 || len(calls) != 3 {
		t.Fatalf("%d uploads, %d posts", len(ups), len(calls))
	}
	if len(calls[0].MediaIDs) != 2 || len(calls[1].MediaIDs) != 0 || len(calls[2].MediaIDs) != 1 {
		t.Fatalf("media per part: %v %v %v", calls[0].MediaIDs, calls[1].MediaIDs, calls[2].MediaIDs)
	}
	if calls[2].ReplyTo == "" || calls[1].ReplyTo == "" || calls[0].ReplyTo != "" {
		t.Fatal("reply chain broken")
	}
	if fake.Alt(calls[0].MediaIDs[0]) != "Example alt one" || fake.Alt(calls[0].MediaIDs[1]) != "" || fake.Alt(calls[2].MediaIDs[0]) != "Example alt three" {
		t.Fatal("alt text not set as written")
	}
	for _, u := range ups {
		if u.Mime != "image/png" || len(u.Data) == 0 || !strings.HasPrefix(u.IdempotencyKey, "d") || !strings.Contains(u.IdempotencyKey, "-m") {
			t.Fatalf("upload %+v", u.IdempotencyKey)
		}
	}
	r := f.rows(t, id)
	if r[0].Images != 2 || r[1].Images != 0 || r[2].Images != 1 {
		t.Fatalf("logged images %d %d %d", r[0].Images, r[1].Images, r[2].Images)
	}
	sp := f.spend(t)
	if sp.Images != 3 || sp.Alts != 2 || sp.Micros != 3*1_000_000+3*500_000+2*100_000 {
		t.Fatalf("spend %+v", sp)
	}
}

// Without the image rate, drafts with images wait (the cap could not count
// them) while text-only drafts still post.
func TestUnknownImageRateHoldsOnlyImageDrafts(t *testing.T) {
	f := setup(t)
	f.enable(t)
	img := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example image", []string{""}})
	txt := f.draft(t, "sample_maker", t0.Add(20*time.Minute), true, "Example text")
	fake := f.fake()
	f.now = t0.Add(20 * time.Minute)
	f.run(t, f.disp(fake))
	if status(t, f, img) != store.StatusApproved || status(t, f, txt) != store.StatusPosted || len(fake.Uploads()) != 0 {
		t.Fatalf("img %s txt %s uploads %d", status(t, f, img), status(t, f, txt), len(fake.Uploads()))
	}
	if sp := f.spend(t); !strings.Contains(sp.UploadBlock(), "XPOST_COST_MEDIA_UPLOAD_USD") || !strings.Contains(sp.AltBlock(), "XPOST_COST_MEDIA_METADATA_USD") {
		t.Fatal(sp.MediaBlocks())
	}
	f.enableImages()
	f.run(t, f.disp(fake))
	if status(t, f, img) != store.StatusPosted {
		t.Fatal("did not post once the rate was set")
	}
}

// The cap counts the images still to upload: a draft that fits as text but
// not with its images waits.
func TestCapCountsImages(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.st.Rates.UploadMicros, f.st.Rates.UploadKnown = 5_000_000, true // invented
	f.st.Rates.AltKnown = true
	ctx := context.Background()
	if err := f.st.SetCapCents(ctx, f.owner, 1000); err != nil { // $10
		t.Fatal(err)
	}
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"", ""}}) // $1 + 2 x $5
	fake := f.fake()
	f.now = t0.Add(20 * time.Minute)
	f.run(t, f.disp(fake))
	if status(t, f, id) != store.StatusApproved || len(fake.Uploads()) != 0 {
		t.Fatal("started a draft whose images pass the cap")
	}
	if err := f.st.SetCapCents(ctx, f.owner, 1100); err != nil {
		t.Fatal(err)
	}
	f.run(t, f.disp(fake))
	if status(t, f, id) != store.StatusPosted {
		t.Fatal("did not post within the cap")
	}
}

// 429 on an upload: nothing is posted, the account waits for X's reset,
// then the upload and the post go out once.
func TestUploadRateLimitWaitsForReset(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"Example alt"}})
	reset := t0.Add(40 * time.Minute)
	n := 0
	fake := f.fake()
	fake.UploadFail = func(Upload) error {
		if n++; n == 1 {
			return &Error{Kind: RateLimited, Status: 429, Msg: "rate limited by X", RetryAt: reset}
		}
		return nil
	}
	disp := f.disp(fake)
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Waiting != 1 || len(fake.Calls()) != 0 {
		t.Fatalf("%+v", rep)
	}
	r := f.rows(t, id)
	if len(r) != 1 || r[0].Status != store.LogRetry || !strings.Contains(r[0].Error, "image 1 upload") || !r[0].RetryAt.Equal(reset) {
		t.Fatalf("%+v", r)
	}
	if f.spend(t).Images != 0 {
		t.Fatal("a 429 upload counted as billed")
	}
	f.now = reset.Add(-time.Minute)
	f.run(t, disp)
	if n != 1 {
		t.Fatal("retried before the reset")
	}
	f.now = reset
	if rep := f.run(t, disp); rep.Posted != 1 || len(fake.Calls()) != 1 || len(fake.Uploads()) != 2 {
		t.Fatalf("%+v posts %d uploads %d", rep, len(fake.Calls()), len(fake.Uploads()))
	}
}

// An upload X refuses fails the part without posting; the owner's Retry
// uploads it again and posts.
func TestUploadRefusedThenRetry(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	ctx := context.Background()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{""}})
	refuse := true
	fake := f.fake()
	fake.UploadFail = func(Upload) error {
		if refuse {
			return &Error{Kind: Refused, Status: 400, Msg: "X refused it: example"}
		}
		return nil
	}
	disp := f.disp(fake)
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Failed != 1 || len(fake.Calls()) != 0 {
		t.Fatalf("%+v", rep)
	}
	d := f.get(t, id)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "image 1 upload") {
		t.Fatal(d.Error)
	}
	if r := f.rows(t, id); r[0].Status != store.LogFailed {
		t.Fatalf("%+v", r[0])
	}
	refuse = false
	if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	if rep := f.run(t, disp); rep.Posted != 1 || len(fake.Calls()) != 1 {
		t.Fatalf("%+v", rep)
	}
}

// An upload with an unknown outcome is counted as billed and simply sent
// again: an upload alone posts nothing, so the post still goes out once.
func TestUnknownUploadIsUploadedAgainNeverPostedTwice(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{""}})
	n := 0
	fake := f.fake()
	fake.UploadFail = func(Upload) error {
		if n++; n == 1 {
			return &Error{Kind: Ambiguous, Msg: "timed out waiting for X; the upload may or may not have gone through"}
		}
		return nil
	}
	disp := f.disp(fake)
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Waiting != 1 || rep.Review != 0 {
		t.Fatalf("%+v", rep)
	}
	key := MediaKey(f.get(t, id), 1, 1)
	if u := f.uploads(t, id)[key]; u.Status != store.UpUnknown || !strings.Contains(u.Error, "uploaded again") {
		t.Fatalf("%+v", u)
	}
	if sp := f.spend(t); sp.Images != 1 || sp.UnknownUploads != 1 {
		t.Fatalf("unknown upload not counted as billed: %+v", sp)
	}
	f.now = f.now.Add(Backoff(1))
	if rep := f.run(t, disp); rep.Posted != 1 {
		t.Fatalf("%+v", rep)
	}
	if len(fake.Calls()) != 1 || len(fake.Uploads()) != 2 {
		t.Fatalf("posts %d uploads %d", len(fake.Calls()), len(fake.Uploads()))
	}
	if sp := f.spend(t); sp.Images != 2 {
		t.Fatalf("both upload calls should count: %+v", sp)
	}
}

// A crash during an upload: after the lease lapses the upload is unknown,
// uploaded again, and the draft posts once without review.
func TestCrashMidUploadResumesWithoutReview(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	ctx := context.Background()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"Example alt"}})
	crash := f.fake()
	crash.UploadFail = func(Upload) error { runtime.Goexit(); return nil }
	f.now = t0.Add(20 * time.Minute)
	runCrashing(t, f, f.disp(crash))
	key := MediaKey(f.get(t, id), 1, 1)
	if u := f.uploads(t, id)[key]; u.Status != store.UpUploading {
		t.Fatalf("%+v", u)
	}
	fake := f.fake()
	disp := f.disp(fake)
	f.now = f.now.Add(DefaultLease)
	if err := disp.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if u := f.uploads(t, id)[key]; u.Status != store.UpUnknown {
		t.Fatalf("%+v", u)
	}
	if rep := f.run(t, disp); rep.Posted != 1 || rep.Review != 0 || len(fake.Calls()) != 1 {
		t.Fatalf("%+v", rep)
	}
	if fake.Alt(fake.Calls()[0].MediaIDs[0]) != "Example alt" {
		t.Fatal("alt text lost")
	}
}

// A crash during the post itself (images already up) still goes to review;
// after "it did not post" and Retry, the uploaded images are reused, not
// uploaded again.
func TestCrashMidPostWithImagesReviewThenReuse(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	ctx := context.Background()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example crash", []string{""}})
	cp := &crashPoster{crashOn: "Example crash"}
	cp.Fake.Now = func() time.Time { return f.now }
	f.now = t0.Add(20 * time.Minute)
	runCrashing(t, f, f.disp(cp))
	f.now = f.now.Add(DefaultLease)
	fake := f.fake()
	disp := f.disp(fake)
	if err := disp.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	d := f.get(t, id)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "Needs review") {
		t.Fatal(d.Error)
	}
	f.run(t, disp)
	if len(fake.Calls()) != 0 || len(fake.Uploads()) != 0 {
		t.Fatal("acted on a part under review")
	}
	if err := f.st.ResolveUnknown(ctx, f.owner, id, d.Version, 1, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	if rep := f.run(t, disp); rep.Posted != 1 || len(fake.Uploads()) != 0 || len(fake.Calls()) != 1 {
		t.Fatalf("%+v uploads %d", rep, len(fake.Uploads()))
	}
	if got := fake.Calls()[0].MediaIDs; len(got) != 1 || got[0] != f.uploads(t, id)[MediaKey(d, 1, 1)].XMediaID {
		t.Fatalf("media ids %v", got)
	}
}

// A thread whose part 2 fails: Retry posts only part 2, never touching part
// 1 or its images; part 2's images are uploaded once.
func TestPartialThreadWithImagesRetriesOnlyTheRest(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	ctx := context.Background()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example one", []string{"", ""}}, imgPart{"Example two", []string{"Example alt two"}})
	refuse := true
	fake := f.fake()
	fake.Fail = func(r Request) error {
		if r.Text == "Example two" && refuse {
			return &Error{Kind: Refused, Status: 400, Msg: "X refused it: example"}
		}
		return nil
	}
	disp := f.disp(fake)
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Failed != 1 {
		t.Fatalf("%+v", rep)
	}
	if len(fake.Uploads()) != 3 {
		t.Fatalf("uploads %d", len(fake.Uploads()))
	}
	refuse = false
	d := f.get(t, id)
	if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour) // well within the 24h media lifetime
	if rep := f.run(t, disp); rep.Posted != 1 {
		t.Fatalf("%+v", rep)
	}
	calls := fake.Calls()
	if len(calls) != 3 || calls[2].Text != "Example two" || calls[2].ReplyTo != calls[0].ReplyTo && calls[2].ReplyTo == "" {
		t.Fatalf("%+v", calls)
	}
	if len(fake.Uploads()) != 3 {
		t.Fatalf("re-uploaded on retry: %d uploads", len(fake.Uploads()))
	}
	posted := 0
	for _, c := range calls {
		if c.Text == "Example one" {
			posted++
		}
	}
	if posted != 1 {
		t.Fatal("part 1 posted twice")
	}
}

// An uploaded image whose X media id expired (or is about to) before its
// part posts is uploaded again.
func TestExpiredUploadIsUploadedAgain(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	ctx := context.Background()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example one", nil}, imgPart{"Example two", []string{""}})
	refuse := true
	fake := f.fake()
	fake.MediaTTL = 2 * time.Hour
	fake.Fail = func(r Request) error {
		if r.Text == "Example two" && refuse {
			return &Error{Kind: Refused, Status: 400, Msg: "X refused it: example"}
		}
		return nil
	}
	disp := f.disp(fake)
	f.now = t0.Add(20 * time.Minute)
	f.run(t, disp)
	refuse = false
	d := f.get(t, id)
	if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2*time.Hour - MediaMargin + time.Second) // inside the margin
	if rep := f.run(t, disp); rep.Posted != 1 || len(fake.Uploads()) != 2 {
		t.Fatalf("%+v uploads %d", rep, len(fake.Uploads()))
	}
	if got := fake.Calls()[2].MediaIDs[0]; got != f.uploads(t, id)[MediaKey(d, 2, 1)].XMediaID || got == fake.Calls()[1].MediaIDs[0] {
		t.Fatal("posted the expired media id")
	}
}

// X still processing an upload: the draft waits without using up attempts,
// checks again, then posts.
func TestProcessingWaitsWithoutCountingAttempts(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"Example alt"}})
	fake := f.fake()
	fake.ProcessChecks = 2
	disp := f.disp(fake)
	disp.MaxAttempts = 1 // a counted attempt would fail the draft at once
	f.now = t0.Add(20 * time.Minute)
	for i := 0; i < 2; i++ { // the upload, then one check still processing
		if rep := f.run(t, disp); rep.Waiting != 1 {
			t.Fatalf("run %d: %+v", i, rep)
		}
		if d := f.get(t, id); d.Attempts != 0 || d.Status != store.StatusPosting {
			t.Fatalf("attempts %d status %s", d.Attempts, d.Status)
		}
		f.now = f.now.Add(10 * time.Second)
	}
	if rep := f.run(t, disp); rep.Posted != 1 || len(fake.Uploads()) != 1 {
		t.Fatalf("%+v", rep)
	}
}

// X processing fails: the part fails, nothing posts.
func TestProcessingFailureFailsThePart(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{""}})
	fake := f.fake()
	fake.UploadFail = func(Upload) error { return nil }
	disp := f.disp(&failingProcessor{Fake: fake})
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Failed != 1 || len(fake.Calls()) != 0 {
		t.Fatalf("%+v", rep)
	}
	if d := f.get(t, id); !strings.Contains(d.Error, "could not process image 1") {
		t.Fatal(d.Error)
	}
}

type failingProcessor struct{ *Fake }

func (p *failingProcessor) Upload(ctx context.Context, u Upload) (Uploaded, error) {
	r, err := p.Fake.Upload(ctx, u)
	r.Failed = "X reported processing failed"
	return r, err
}

// Alt text: a refusal fails the part; a transient failure retries the alt
// call only (the image is not uploaded again).
func TestAltTextFailures(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := setup(t)
		f.enable(t)
		f.enableImages()
		id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"Example alt"}})
		fake := f.fake()
		fake.AltFail = func(string, string) error { return &Error{Kind: Refused, Status: 400, Msg: "X refused the alt text"} }
		f.now = t0.Add(20 * time.Minute)
		if rep := f.run(t, f.disp(fake)); rep.Failed != 1 || len(fake.Calls()) != 0 {
			t.Fatalf("%+v", rep)
		}
		if d := f.get(t, id); !strings.Contains(d.Error, "image 1 alt text") {
			t.Fatal(d.Error)
		}
	})
	t.Run("transient", func(t *testing.T) {
		f := setup(t)
		f.enable(t)
		f.enableImages()
		f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"Example alt"}})
		n := 0
		fake := f.fake()
		fake.AltFail = func(string, string) error {
			if n++; n == 1 {
				return &Error{Kind: Ambiguous, Msg: "timed out"}
			}
			return nil
		}
		disp := f.disp(fake)
		f.now = t0.Add(20 * time.Minute)
		if rep := f.run(t, disp); rep.Waiting != 1 {
			t.Fatalf("%+v", rep)
		}
		f.now = f.now.Add(Backoff(1))
		if rep := f.run(t, disp); rep.Posted != 1 || len(fake.Uploads()) != 1 {
			t.Fatalf("%+v uploads %d", rep, len(fake.Uploads()))
		}
		if fake.Alt(fake.Calls()[0].MediaIDs[0]) != "Example alt" {
			t.Fatal("alt not set")
		}
	})
}

// Repeated upload failures give up after MaxAttempts, posting nothing.
func TestUploadGivesUpAfterMaxAttempts(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{""}})
	fake := f.fake()
	fake.UploadFail = func(Upload) error { return &Error{Kind: NotSent, Msg: "could not connect to X"} }
	disp := f.disp(fake)
	disp.MaxAttempts = 3
	f.now = t0.Add(20 * time.Minute)
	for i := 0; i < 5; i++ {
		f.run(t, disp)
		f.now = f.now.Add(MaxBackoff)
	}
	d := f.get(t, id)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "gave up after 3 attempts") || len(fake.Uploads()) != 3 || len(fake.Calls()) != 0 {
		t.Fatalf("%s %q uploads %d", d.Status, d.Error, len(fake.Uploads()))
	}
}

// Racing dispatchers upload each image once and post each part once.
func TestConcurrentDispatchersUploadOnce(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	for i := 0; i < 2; i++ { // one per account: spacing holds a second one
		f.imgDraft(t, []string{"sample_writer", "sample_maker"}[i%2], t0.Add(time.Duration(20+20*i)*time.Minute), imgPart{"Example a", []string{"Example alt", ""}}, imgPart{"Example b", []string{""}})
	}
	fake := f.fake()
	f.now = t0.Add(3 * time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := f.disp(fake)
			for j := 0; j < 3; j++ {
				d.RunDue(context.Background(), f.now)
			}
		}()
	}
	wg.Wait()
	ups := map[string]int{}
	for _, u := range fake.Uploads() {
		ups[u.IdempotencyKey]++
	}
	posts := map[string]int{}
	for _, c := range fake.Calls() {
		posts[c.IdempotencyKey]++
	}
	if len(ups) != 6 || len(posts) != 4 {
		t.Fatalf("%d uploads, %d posts", len(ups), len(posts))
	}
	for k, n := range ups {
		if n != 1 {
			t.Errorf("%s uploaded %d times", k, n)
		}
	}
	for k, n := range posts {
		if n != 1 {
			t.Errorf("%s posted %d times", k, n)
		}
	}
}

// Each media rate fails closed on its own: an unset upload rate holds
// drafts with images, an unset metadata rate holds drafts with alt text,
// an explicit 0 is free, and text-only drafts always post.
func TestMediaRatesFailClosedSeparately(t *testing.T) {
	type rate struct {
		micros int64
		known  bool
	}
	unset, zero, set := rate{0, false}, rate{0, true}, rate{250_000, true}
	cases := []struct {
		name                 string
		up, alt              rate
		textOK, imgOK, altOK bool
		micros               int64 // spend after the run
	}{
		{"both unset", unset, unset, true, false, false, 1_000_000},
		{"upload unset, metadata 0", unset, zero, true, false, false, 1_000_000},
		{"upload unset, metadata set", unset, set, true, false, false, 1_000_000},
		{"upload 0, metadata unset", zero, unset, true, true, false, 2_000_000},
		{"upload set, metadata unset", set, unset, true, true, false, 2_250_000},
		{"both 0", zero, zero, true, true, true, 3_000_000},
		{"upload 0, metadata set", zero, set, true, true, true, 3_250_000},
		{"both set", set, set, true, true, true, 3_750_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.enable(t)
			f.st.Rates.UploadMicros, f.st.Rates.UploadKnown = c.up.micros, c.up.known
			f.st.Rates.AltMicros, f.st.Rates.AltKnown = c.alt.micros, c.alt.known
			// Spaced 15 minutes apart on two accounts, so all can post.
			txt := f.draft(t, "sample_maker", t0.Add(20*time.Minute), true, "Example text")
			img := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example image, no alt", []string{""}})
			alt := f.imgDraft(t, "sample_maker", t0.Add(40*time.Minute), imgPart{"Example image with alt", []string{"Example alt"}})
			fake := f.fake()
			f.now = t0.Add(20 * time.Minute)
			f.run(t, f.disp(fake))
			f.now = t0.Add(40 * time.Minute)
			f.run(t, f.disp(fake))
			got := func(id int64) bool { return status(t, f, id) == store.StatusPosted }
			if got(txt) != c.textOK || got(img) != c.imgOK || got(alt) != c.altOK {
				t.Fatalf("posted: text %v image %v alt %v", got(txt), got(img), got(alt))
			}
			for id, ok := range map[int64]bool{img: c.imgOK, alt: c.altOK} {
				if d := f.get(t, id); !ok && (d.Status != store.StatusApproved || len(f.rows(t, id)) != 0) {
					t.Fatalf("held draft %d touched: %s", id, d.Status)
				}
			}
			if !c.altOK && len(fake.Uploads()) != btoi(c.imgOK) {
				t.Fatalf("uploaded %d images for held drafts", len(fake.Uploads()))
			}
			sp := f.spend(t)
			if sp.Micros != c.micros {
				t.Fatalf("spend %d, want %d (%+v)", sp.Micros, c.micros, sp)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The cap covers alt text calls up front, for the whole thread.
func TestCapCountsAltCalls(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.st.Rates.UploadMicros, f.st.Rates.UploadKnown = 0, true
	f.st.Rates.AltMicros, f.st.Rates.AltKnown = 2_000_000, true // invented
	ctx := context.Background()
	f.st.SetCapCents(ctx, f.owner, 500) // $5
	// 2 parts ($2) + 2 alt text calls ($4) = $6
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example one", []string{"Example alt"}}, imgPart{"Example two", []string{"Example alt two"}})
	fake := f.fake()
	f.now = t0.Add(20 * time.Minute)
	f.run(t, f.disp(fake))
	if status(t, f, id) != store.StatusApproved || len(fake.Uploads()) != 0 {
		t.Fatal("started a thread whose alt text calls pass the cap")
	}
	f.st.SetCapCents(ctx, f.owner, 600)
	f.run(t, f.disp(fake))
	if status(t, f, id) != store.StatusPosted {
		t.Fatalf("did not post within the cap: %s", f.get(t, id).Error)
	}
	if sp := f.spend(t); sp.Alts != 2 || sp.Micros != 6_000_000 {
		t.Fatalf("%+v", sp)
	}
}

// An alt text call with an unknown outcome is counted as billed; setting
// it again is harmless, and is billed again. The post goes out once.
func TestUnknownAltCallIsBilled(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.enableImages()
	id := f.imgDraft(t, "sample_writer", t0.Add(20*time.Minute), imgPart{"Example", []string{"Example alt"}})
	fake := f.fake()
	n := 0
	fake.AltFail = func(string, string) error {
		if n++; n == 1 {
			return &Error{Kind: Ambiguous, Msg: "no reply from X (example)"}
		}
		return nil
	}
	f.now = t0.Add(20 * time.Minute)
	f.run(t, f.disp(fake))
	if sp := f.spend(t); sp.Alts != 1 || sp.UnknownAlts != 1 || len(fake.Calls()) != 0 {
		t.Fatalf("after unknown: %+v posts %d", sp, len(fake.Calls()))
	}
	for i := 0; i < 5 && status(t, f, id) != store.StatusPosted; i++ {
		f.now = f.now.Add(5 * time.Minute)
		f.run(t, f.disp(fake))
	}
	sp := f.spend(t)
	if status(t, f, id) != store.StatusPosted || len(fake.Calls()) != 1 || sp.Alts != 2 || sp.UnknownAlts != 1 || sp.Micros != 1_000_000+500_000+2*100_000 {
		t.Fatalf("%s posts %d %+v", status(t, f, id), len(fake.Calls()), sp)
	}
}
