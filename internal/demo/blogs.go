package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/scott/xpost/internal/store"
)

// demoLong is an invented post with every block the preview styles.
const demoLong = `Example opening paragraph, invented for this demo. Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris.

Second example paragraph. Duis aute irure dolor in reprehenderit in voluptate velit esse cillum dolore eu fugiat nulla pariatur. Excepteur sint occaecat cupidatat non proident.

## Sample heading one

Example paragraph under a heading, with a long link that must break instead of scrolling: [https://example.com/sample/docs/a-very-long-path/that-keeps-going/for-the-demo?ref=example_workspace&utm=none](https://example.com/sample/docs/a-very-long-path/that-keeps-going/for-the-demo?ref=example_workspace&utm=none).

> Example quote, invented for the demo. A pull quote wraps on small screens and keeps its left rule.

- First sample point, short.
- Second sample point with a little more text so it wraps on a phone.
- Third sample point.

## Sample heading two

Example paragraph before a code block. Sed ut perspiciatis unde omnis iste natus error sit voluptatem.

` + "```" + `
$ xpost-sample --account demo --dry-run --note "a deliberately long sample command line that would normally scroll sideways on a phone"
{ "sample": true, "note": "example JSON, invented for this demo and long enough to wrap" }
` + "```" + `

Closing example paragraph. Nam libero tempore, cum soluta nobis est eligendi optio cumque nihil impedit quo minus id quod maxime placeat.`

// seedBlogs adds three example blogs (two ready, one untested) and blog
// posts in each state: pending, lint blocked, approved, published, failed
// and an owner-written post on the untested blog. Publishing is simulated
// with the store calls the fake publisher's dispatcher makes.
func seedBlogs(ctx context.Context, st *store.Store, owner *store.User, tok *store.AgentToken, now time.Time, loc *time.Location) ([]string, error) {
	at := func(t time.Time) { st.Now = func() time.Time { return t } }
	day := func(d, hh, mm int) time.Time {
		l := now.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day()+d, hh, mm, 0, 0, loc)
	}
	at(now.Add(-96 * time.Hour))
	notes, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "notes.example.com", SiteURL: "https://notes.example.com", Method: store.MethodWebhook, PromoAccount: "sample_writer",
		Topics: []string{"example banned topic"}})
	if err != nil {
		return nil, err
	}
	guide, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "guide.example.org", SiteURL: "https://guide.example.org", Method: store.MethodAPI, APIBase: "https://guide.example.org",
		PromoAccount: "sample_maker", CoverAltRequired: true})
	if err != nil {
		return nil, err
	}
	drafts, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "drafts.example.net", SiteURL: "https://drafts.example.net", Method: store.MethodAPI})
	if err != nil {
		return nil, err
	}
	// Example connection values (sealed like real ones; never used: the
	// demo has no real publisher).
	st.SetBlogSecret(ctx, owner, notes.ID, store.BlogHookURL, "https://hooks.example.com/sample-blog")
	st.SetBlogSecret(ctx, owner, notes.ID, store.BlogHookKey, "example-signing-key-not-a-secret-000")
	st.SetBlogSecret(ctx, owner, guide.ID, store.BlogAPIToken, "example-token-not-a-secret-0000")
	for _, b := range []*store.Blog{notes, guide} {
		if err := st.RecordBlogTest(ctx, owner, b.ID, true, "Test passed. Nothing was published.", 640); err != nil {
			return nil, err
		}
	}
	cover := func(pal int) string {
		ref, _, err := st.AddMedia(ctx, tok, Image(pal))
		if err != nil {
			panic(err)
		}
		return ref
	}
	type spec struct {
		site, slug, title, desc, md string
		tags                        []string
		pal                         int // -1: no cover
		alt                         string
		created                     time.Time
		when                        time.Time
	}
	mk := func(sp spec) (*store.Draft, error) {
		at(sp.created)
		nb := store.NewBlogDraft{Site: sp.site, Slug: sp.slug, Title: sp.title, Description: sp.desc, Tags: sp.tags, Markdown: sp.md, ProposedAt: sp.when}
		if sp.pal >= 0 {
			nb.Cover = &store.MediaIn{Ref: cover(sp.pal), Alt: sp.alt}
		}
		d, _, err := st.CreateBlogDraft(ctx, tok, nb, loc)
		if err != nil {
			return nil, fmt.Errorf("demo blog %s: %w", sp.slug, err)
		}
		return d, nil
	}
	// promo proposes an X post linking to blog post b, at the blog's time
	// plus the default offset; approve approves it at once.
	np := 0
	promo := func(b *store.Draft, acct, text string, created time.Time, approve bool, offset *int) error {
		at(created)
		p, _, err := st.CreateDraft(ctx, tok, store.NewDraft{Account: acct, Parts: []store.PartIn{{Text: text}}, Promotes: b.ID, PromoOffset: offset})
		if err != nil {
			return fmt.Errorf("demo promo: %w", err)
		}
		np++
		if approve {
			return st.Approve(ctx, owner, p.ID, p.Version)
		}
		return nil
	}
	n := 0
	// Pending, lint passed (07a, 07b), with two pending promos (07d).
	bw, err := mk(spec{site: notes.Name, slug: "sample-build-week", title: "Example post: notes from a sample build week", desc: "Invented sample summary: what one demo week of building looked like, in one line.",
		md: demoLong, tags: []string{"agents", "sample"}, pal: 0, alt: "An abstract sample illustration.", created: now.Add(-20 * time.Minute), when: day(3, 7, 0)})
	if err != nil {
		return nil, err
	}
	if err := promo(bw, "sample_writer", "Example promo: a new sample post about one demo build week. Lorem ipsum dolor sit amet. "+store.BlogURLToken, now.Add(-19*time.Minute), false, nil); err != nil {
		return nil, err
	}
	if err := promo(bw, "sample_maker", "Example promo: notes from a sample build week are up. "+store.BlogURLToken, now.Add(-18*time.Minute), false, nil); err != nil {
		return nil, err
	}
	n++
	// Pending, lint blocked: an em dash blocks, an en dash warns (07a2).
	lb, err := mk(spec{site: guide.Name, slug: "sample-shared-docs-guide", title: "Example post: a sample guide to shared docs", desc: "Invented sample summary: how a demo team shares and comments on docs.",
		md:   "Example opening paragraph for a second site. Sed ut perspiciatis unde omnis iste natus error sit voluptatem accusantium doloremque laudantium, totam rem aperiam.\n\n## Sample heading\n\nSample sentence about the review flow, and a second clause.\n\n> Example quote, invented for the demo.\n\nSample years 2019\u20132024 in a list.",
		tags: []string{"docs", "sample"}, pal: 1, alt: "Example alt text: two sample documents side by side.", created: now.Add(-90 * time.Minute), when: day(1, 7, 0)})
	if err != nil {
		return nil, err
	}
	// Intake refuses an em dash, so this state comes from a rule or text
	// change after intake; the demo sets the text directly.
	st.DB.ExecContext(ctx, `UPDATE blog_posts SET markdown = replace(markdown, ?, ?) WHERE draft_id = ?`, "flow, and", "flow \u2014 and", lb.ID)
	// Approved while its blog post is still pending: the calendar marks it
	// Waiting on blog (07c). +2h45m keeps it clear of the 9:05 and 9:15 posts.
	wait := 165
	if err := promo(lb, "sample_maker", "Example promo: a sample guide to shared docs, with demo tips. "+store.BlogURLToken, now.Add(-80*time.Minute), true, &wait); err != nil {
		return nil, err
	}
	n++
	// Approved, publishes later.
	ap, err := mk(spec{site: notes.Name, slug: "sample-reading-list", title: "Example post: a sample reading list", desc: "Invented sample summary: five demo links worth a look.",
		md: "Example opening paragraph for an approved post. Quis autem vel eum iure reprehenderit qui in ea voluptate velit esse.\n\nA second sample paragraph.", tags: []string{"sample"}, pal: 2,
		alt: "A sample stack of books.", created: now.Add(-26 * time.Hour), when: day(4, 7, 0)})
	if err != nil {
		return nil, err
	}
	if err := st.Approve(ctx, owner, ap.ID, ap.Version); err != nil {
		return nil, err
	}
	// Approved early: posts after the blog is live.
	if err := promo(ap, "sample_writer", "Example promo: a sample reading list, five demo links. "+store.BlogURLToken, now.Add(-25*time.Hour), true, nil); err != nil {
		return nil, err
	}
	n++
	// Published yesterday at 7:00 (07e), and failed this morning (07e2).
	pubAt, failAt := day(-1, 7, 0), day(0, 7, 0)
	if !failAt.Before(now) {
		pubAt, failAt = day(-2, 7, 0), day(-1, 7, 0)
	}
	publish := func(d *store.Draft, at0 time.Time, ok bool, url, why string) error {
		at(at0.Add(-30 * time.Hour))
		if err := st.Approve(ctx, owner, d.ID, d.Version); err != nil {
			return err
		}
		at(at0)
		due, err := st.DueBlogs(ctx, at0)
		if err != nil {
			return err
		}
		for _, du := range due {
			if du.ID == d.ID {
				if _, err := st.Claim(ctx, du, at0.Add(5*time.Minute)); err != nil {
					return err
				}
			}
		}
		if ok {
			st.MarkStaged(ctx, d.ID, fmt.Sprintf("example-ref-%d", d.ID))
			at(at0.Add(3 * time.Minute))
		} else {
			at(at0.Add(31 * time.Minute))
		}
		return st.FinishBlog(ctx, d.ID, ok, url, fmt.Sprintf("example-ref-%d", d.ID), why)
	}
	pd, err := mk(spec{site: notes.Name, slug: "sample-weekly-recap", title: "Example post: a sample weekly recap", desc: "Invented sample summary: a short recap of a demo week.",
		md:   "Example opening paragraph for a published post. Nemo enim ipsam voluptatem quia voluptas sit aspernatur aut odit aut fugit, sed quia consequuntur magni dolores eos qui ratione.\n\nMore sample text.",
		tags: []string{"recap"}, pal: 2, alt: "A sample sunrise over hills.", created: pubAt.Add(-40 * time.Hour), when: pubAt})
	if err != nil {
		return nil, err
	}
	if err := publish(pd, pubAt, true, notes.LiveURL("sample-weekly-recap"), ""); err != nil {
		return nil, err
	}
	fd, err := mk(spec{site: guide.Name, slug: "sample-october-changelog", title: "Example post: a sample changelog for October", desc: "Invented sample summary: what changed in a demo product last month.",
		md:   "Example opening paragraph for the post that failed to publish. Quis autem vel eum iure reprehenderit qui in ea voluptate velit esse quam nihil molestiae consequatur.\n\nMore sample text.",
		tags: []string{"changelog"}, pal: 3, alt: "A sample chart going up.", created: failAt.Add(-40 * time.Hour), when: failAt})
	if err != nil {
		return nil, err
	}
	// Approved, then held when the publish fails. The example log's
	// @sample_maker posts sit at fixed distances from now, so pick an
	// offset that keeps this promo clear of them whatever the clock says.
	off := heldOffset(failAt, now.Add(-30*time.Minute-5*time.Hour), now.Add(-30*time.Minute-75*time.Minute))
	if err := promo(fd, "sample_maker", "Example promo: the sample October changelog. Duis aute irure dolor. "+store.BlogURLToken, failAt.Add(-39*time.Hour), true, &off); err != nil {
		return nil, err
	}
	fd, _ = st.Draft(ctx, fd.ID)
	if err := publish(fd, failAt, false, "", "The guide.example.org Publisher API answered 503 Service Unavailable (example error)."); err != nil {
		return nil, err
	}
	n += 2
	// Owner-written, on the untested blog (07g5).
	od, err := mk(spec{site: drafts.Name, slug: "sample-owner-note", title: "Example post: an owner-written sample note", desc: "Invented sample summary: written in xpost, not by an agent.",
		md: "Example opening paragraph written by the owner. Temporibus autem quibusdam et aut officiis debitis aut rerum necessitatibus saepe eveniet.\n\nMore sample text.", pal: -1,
		created: now.Add(-3 * time.Hour), when: day(5, 7, 0)})
	if err != nil {
		return nil, err
	}
	st.DB.ExecContext(ctx, `UPDATE drafts SET token_id = NULL, source = ? WHERE id = ?`, store.OwnerSource, od.ID)
	st.DB.ExecContext(ctx, `UPDATE draft_events SET actor = 'owner' WHERE draft_id = ? AND kind = 'proposed'`, od.ID)
	n++
	return []string{"3 example blogs (notes.example.com and guide.example.org ready, drafts.example.net untested)", fmt.Sprintf("%d example blog posts (pending, lint blocked, approved, published, failed, owner-written)", n),
		fmt.Sprintf("%d example promo posts (pending, approved early, waiting on blog, held)", np)}, nil
}

// heldOffset returns a promo offset in minutes (2h first) that keeps
// blog+offset at least 30 minutes from every time in busy.
func heldOffset(blog time.Time, busy ...time.Time) int {
	for _, m := range []int{120, 180, 60, 240} {
		t := blog.Add(time.Duration(m) * time.Minute)
		ok := true
		for _, b := range busy {
			if d := t.Sub(b); d < 30*time.Minute && d > -30*time.Minute {
				ok = false
			}
		}
		if ok {
			return m
		}
	}
	return 120
}
