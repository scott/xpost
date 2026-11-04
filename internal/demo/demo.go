// Package demo seeds invented example data for local development and
// screenshots: placeholder copy, example agent names and fake post ids.
// Nothing here is real, and it never calls X.
package demo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Image draws a placeholder picture: a two-colour gradient with a hill and
// a sun, like the mocks. palette is 0..3.
func Image(palette int) []byte {
	pals := [][2]color.RGBA{
		{{0x9f, 0xb4, 0xe8, 255}, {0x5b, 0x78, 0xc7, 255}},
		{{0xb7, 0xd8, 0xcf, 255}, {0x5e, 0x9e, 0x8f, 255}},
		{{0xe6, 0xcf, 0xa8, 255}, {0xc0, 0x8f, 0x4d, 255}},
		{{0xd2, 0xc4, 0xe6, 255}, {0x8a, 0x74, 0xb8, 255}},
	}
	p := pals[palette%len(pals)]
	const w, h = 320, 200
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64(x+y) / float64(w+h)
			c := color.RGBA{lerp(p[0].R, p[1].R, t), lerp(p[0].G, p[1].G, t), lerp(p[0].B, p[1].B, t), 255}
			// hills
			hill := float64(h) * (0.62 + 0.12*tri(float64(x)/float64(w)))
			if float64(y) > hill {
				c = mix(c, color.RGBA{255, 255, 255, 255}, 0.42)
			}
			// sun
			dx, dy := float64(x-w*3/4), float64(y-h/4)
			if dx*dx+dy*dy < 16*16 {
				c = mix(c, color.RGBA{255, 255, 255, 255}, 0.7)
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }
func mix(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{lerp(a.R, b.R, t), lerp(a.G, b.G, t), lerp(a.B, b.B, t), 255}
}
func tri(x float64) float64 {
	switch {
	case x < 0.3:
		return 1 - x/0.3*1.6
	case x < 0.52:
		return -0.6 + (x-0.3)/0.22*1.2
	case x < 0.72:
		return 0.6 - (x-0.52)/0.2*1.0
	}
	return -0.4 + (x-0.72)/0.28*1.4
}

// Seed adds example tokens, drafts and log rows. It needs the owner (for the
// approvals it makes) and refuses to run twice.
// The invented example X handles the demo data uses. Seed adds them (a
// fresh install has no accounts), and cmd/xpost-fakex signs in as
// them by default (its -users flag is Users), so the two always match.
const (
	HandleA = "sample_writer"
	HandleB = "sample_maker"
	Users   = HandleA + "," + HandleB
)

func Seed(ctx context.Context, st *store.Store) ([]string, error) {
	owner, err := st.Owner(ctx)
	if err != nil {
		return nil, errors.New("create the owner first (xpost create-owner)")
	}
	var n int
	st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM drafts`).Scan(&n)
	if n > 0 {
		return nil, errors.New("this database already has drafts; seed only a fresh demo database")
	}
	// A fresh install has no X accounts; the demo adds its two sample ones.
	for _, h := range []string{HandleA, HandleB} {
		if _, err := st.AddAccount(ctx, h); err != nil && !errors.Is(err, store.ErrAccountExists) {
			return nil, err
		}
	}
	loc := st.Zone(ctx, time.Local)
	realNow := st.Now
	defer func() { st.Now = realNow }()
	now := realNow()
	day := func(d int, hh, mm int) time.Time {
		l := now.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day()+d, hh, mm, 0, 0, loc)
	}
	at := func(t time.Time) { st.Now = func() time.Time { return t } }

	at(now.Add(-72 * time.Hour))
	a1, _, err := st.CreateAgentToken(ctx, owner, "sample-agent-1", []string{store.ScopeDraftsWrite, store.ScopeLogRead})
	if err != nil {
		return nil, err
	}
	a2, _, err := st.CreateAgentToken(ctx, owner, "sample-agent-2", []string{store.ScopeDraftsWrite})
	if err != nil {
		return nil, err
	}
	at(now.Add(-2 * time.Hour))
	if _, _, err := st.CreateAgentToken(ctx, owner, "example-readonly", []string{store.ScopeLogRead}); err != nil {
		return nil, err
	}
	st.DB.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at = ? WHERE id = ?`, now.Add(-12*time.Minute).Unix(), a1.ID)
	st.DB.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at = ? WHERE id = ?`, now.Add(-72*time.Hour).Unix(), a2.ID)

	img := func(tok *store.AgentToken, pal int) string {
		ref, _, err := st.AddMedia(ctx, tok, Image(pal))
		if err != nil {
			panic(err)
		}
		return ref
	}
	type spec struct {
		tok     *store.AgentToken
		acct    string
		when    time.Time
		recv    time.Duration // before now
		parts   []store.PartIn
		approve bool
		reject  string
		rejectd bool
		note    string
	}
	m := func(refs ...string) []store.MediaIn {
		var out []store.MediaIn
		for _, r := range refs {
			out = append(out, store.MediaIn{Ref: r})
		}
		return out
	}
	lorem := "Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore."
	specs := []spec{
		{tok: a1, acct: "sample_maker", when: day(1, 9, 5), recv: 26 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a short note about a demo workspace. Quis autem vel eum iure reprehenderit."}}},
		{tok: a1, acct: "sample_writer", when: day(1, 10, 30), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: shipping notes for v0.3 of a sample project. " + lorem}, {Text: "Part two, example only. Sample notes live at https://example.com/sample-project/v0-3/notes"}, {Text: "Part three, example only. Excepteur sint occaecat cupidatat non proident."}}},
		{tok: a1, acct: "sample_writer", when: day(1, 9, 0), recv: 95 * time.Minute,
			parts: []store.PartIn{{Text: "Example post: shipping notes for v0.4 of a sample project. " + lorem, Media: m(img(a1, 0), img(a1, 1))}, {Text: "Part two, example only. Ut enim ad minim veniam, quis nostrud exercitation."}, {Text: "Part three, example only. Duis aute irure dolor in reprehenderit."}}},
		{tok: a1, acct: "sample_maker", when: day(1, 9, 15), recv: 92 * time.Minute,
			parts: []store.PartIn{{Text: "Example post: a short tip about a sample feature. Ut enim ad minim veniam, quis nostrud exercitation ullamco.", Media: []store.MediaIn{{Ref: img(a1, 2), Alt: "Placeholder illustration of hills under a sun"}}}}},
		{tok: a2, acct: "sample_writer", when: day(2, 12, 30), recv: 5 * time.Hour, note: "Placeholder note from the agent: link goes to sample notes.",
			parts: []store.PartIn{{Text: "Example post: placeholder link to sample release notes https://example.com/sample-notes Duis aute irure dolor in reprehenderit."}}},
		{tok: a1, acct: "sample_maker", when: day(3, 10, 0), recv: 8 * time.Hour,
			parts: []store.PartIn{{Text: "Example post: four screenshots from a demo workspace. Excepteur sint occaecat cupidatat non proident, sunt in culpa qui officia.", Media: m(img(a1, 3), img(a1, 1), img(a1, 0), img(a1, 2))}, {Text: "Part two, example only. Sunt in culpa qui officia deserunt mollit anim."}}},
		{tok: a2, acct: "sample_writer", when: day(2, 17, 0), recv: 20 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: an afternoon placeholder. Nemo enim ipsam voluptatem quia voluptas."}, {Text: "Part two, example only."}}},
		{tok: a1, acct: "sample_maker", when: day(4, 15, 0), recv: 20 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a sample checklist for a demo release. Sed ut perspiciatis unde omnis."}}},
		// More approved examples spread over the week, so the Schedule grid
		// looks like Sol's mock (invented times and copy).
		{tok: a2, acct: "sample_maker", when: day(1, 14, 0), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: an afternoon tip about a sample feature. Nam libero tempore."}}},
		{tok: a2, acct: "sample_maker", when: day(2, 8, 30), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a morning placeholder. Temporibus autem quibusdam."}}},
		{tok: a1, acct: "sample_writer", when: day(3, 9, 30), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: first of two sample notes. Itaque earum rerum hic tenetur."}}},
		{tok: a1, acct: "sample_writer", when: day(3, 9, 45), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: second of two sample notes, 15 minutes later. Sapiente delectus."}}},
		{tok: a2, acct: "sample_writer", when: day(4, 11, 0), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a late-morning placeholder. Ut aut reiciendis voluptatibus."}}},
		{tok: a1, acct: "sample_writer", when: day(5, 9, 0), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a Friday placeholder. Maiores alias consequatur."}}},
		{tok: a1, acct: "sample_maker", when: day(5, 9, 0), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a Friday tip, example only. Aut perferendis doloribus."}}},
		{tok: a2, acct: "sample_maker", when: day(7, 18, 0), recv: 30 * time.Hour, approve: true,
			parts: []store.PartIn{{Text: "Example post: a Sunday evening placeholder. Asperiores repellat."}}},
		{tok: a2, acct: "sample_writer", when: day(2, 8, 30), recv: 40 * time.Hour, rejectd: true, reject: "Example reason: too close to the release notes post. Try Thursday.",
			parts: []store.PartIn{{Text: "Example post: a rejected placeholder about a sample launch."}}},
		{tok: a1, acct: "sample_maker", when: day(3, 14, 0), recv: 44 * time.Hour, rejectd: true,
			parts: []store.PartIn{{Text: "Example post: another rejected placeholder, no reason given."}}},
	}
	var lines []string
	for _, sp := range specs {
		at(now.Add(-sp.recv))
		d, _, err := st.CreateDraft(ctx, sp.tok, store.NewDraft{Account: sp.acct, Parts: sp.parts, ProposedAt: sp.when, Note: sp.note})
		if err != nil {
			return nil, fmt.Errorf("demo draft: %w", err)
		}
		at(now.Add(-sp.recv / 2))
		switch {
		case sp.approve:
			if err := st.Approve(ctx, owner, d.ID, d.Version); err != nil {
				return nil, fmt.Errorf("demo approve: %w", err)
			}
		case sp.rejectd:
			if err := st.Reject(ctx, owner, d.ID, d.Version, sp.reject); err != nil {
				return nil, err
			}
		}
	}
	// Drafts that already went out (or failed), behind the example log rows
	// below: the queue's Posted tab and the Log page show the same items.
	base := now.Add(-30 * time.Minute)
	done := []struct {
		key    int64 // the example log rows' draft number
		tok    *store.AgentToken
		acct   string
		at     time.Time
		parts  []store.PartIn
		status string
		err    string
	}{
		{9004, a2, "sample_writer", base.Add(-26 * time.Hour), []store.PartIn{{Text: "Example post: sample notes https://example.com/notes"}}, store.StatusPosted, ""},
		{9001, a1, "sample_writer", base, []store.PartIn{{Text: "Example post: shipping notes for v0.2 of a sample project. Lorem ipsum dolor sit amet."}, {Text: "Part two, example only."}, {Text: "Part three, example only."}}, store.StatusPosted, ""},
		{9003, a1, "sample_maker", base.Add(-5 * time.Hour), []store.PartIn{{Text: "Example post: a short tip about a sample feature.", Media: []store.MediaIn{{Ref: img(a1, 2), Alt: "Placeholder illustration of hills under a sun"}}}}, store.StatusPosted, ""},
		{9002, a1, "sample_maker", base.Add(-75 * time.Minute), []store.PartIn{{Text: "Example post: a sample checklist. Sed ut perspiciatis unde omnis."}, {Text: "Part two, example only."}}, store.StatusFailed, `Part 2 of 2 failed: 403 Forbidden, "duplicate content" (example error).`},
	}
	draftOf := map[int64]int64{}
	for _, x := range done {
		at(x.at.Add(-3 * time.Hour))
		d, _, err := st.CreateDraft(ctx, x.tok, store.NewDraft{Account: x.acct, Parts: x.parts, ProposedAt: x.at})
		if err != nil {
			return nil, fmt.Errorf("demo posted draft: %w", err)
		}
		at(x.at.Add(-2 * time.Hour))
		if err := st.Approve(ctx, owner, d.ID, d.Version); err != nil {
			return nil, fmt.Errorf("demo posted approve: %w", err)
		}
		at(x.at)
		if d, err = st.Draft(ctx, d.ID); err != nil {
			return nil, err
		}
		if ok, err := st.Claim(ctx, store.Due{ID: d.ID, Version: d.Version, Status: d.Status, Account: d.Account}, x.at.Add(time.Minute)); err != nil || !ok {
			return nil, fmt.Errorf("demo posted claim: %v", err)
		}
		if err := st.FinishDraft(ctx, d.ID, x.status, x.err); err != nil {
			return nil, err
		}
		draftOf[x.key] = d.ID
	}
	st.Now = realNow
	lines = append(lines, fmt.Sprintf("%d example drafts (pending, approved, rejected, posted, failed) from sample-agent-1 and sample-agent-2", len(specs)+len(done)))
	lines = append(lines, "3 example agent tokens (secrets discarded: create your own on the Log tab)")

	// Example post log rows (fake ids, never real X posts).
	images := map[int64]int{9003: 1} // example rows that carried an image
	logRow := func(id int64, acct string, part, parts int, t time.Time, status, errText string, hasURL bool, preview string) error {
		did := id
		if d, ok := draftOf[id]; ok {
			did = d
		}
		e := store.LogEntry{DraftID: did, Account: acct, Part: part, Parts: parts, At: t, Status: status, Error: errText, HasURL: hasURL, Preview: preview, Example: true,
			IdemKey: fmt.Sprintf("demo-%d-%d-%d", id, part, t.Unix()), Images: images[id]}
		if e.Images > 0 {
			if err := st.RecordMediaCall(ctx, t, e.IdemKey+"-m1", store.CallUpload, store.UpUploaded, true); err != nil {
				return err
			}
			if err := st.RecordMediaCall(ctx, t, e.IdemKey+"-m1", store.CallMetadata, store.UpUploaded, true); err != nil {
				return err
			}
		}
		if status == store.StatusPosted {
			pid := fmt.Sprintf("19000000000000%05d", id*10+int64(part))
			e.PostID, e.URL, e.CostMicros = pid, "https://x.invalid/"+acct+"/status/"+pid, st.PartCost(hasURL)
		}
		return st.RecordPart(ctx, e)
	}
	rows := []func() error{
		func() error {
			return logRow(9001, "sample_writer", 1, 3, base, "posted", "", false, "Example post: shipping notes for v0.2 of a sample project. Lorem ipsum dolor sit amet.")
		},
		func() error {
			return logRow(9001, "sample_writer", 2, 3, base, "posted", "", false, "Part two, example only.")
		},
		func() error {
			return logRow(9001, "sample_writer", 3, 3, base, "posted", "", false, "Part three, example only.")
		},
		func() error {
			return logRow(9002, "sample_maker", 1, 2, base.Add(-75*time.Minute), "posted", "", false, "Example post: a sample checklist. Sed ut perspiciatis unde omnis.")
		},
		func() error {
			return logRow(9002, "sample_maker", 2, 2, base.Add(-75*time.Minute), "failed", `403 Forbidden, "duplicate content" (example error). Part 1 posted. Not retried automatically.`, false, "Part two, example only.")
		},
		func() error {
			return logRow(9003, "sample_maker", 1, 1, base.Add(-5*time.Hour), "posted", "", false, "Example post: a short tip about a sample feature.")
		},
		func() error {
			return logRow(9004, "sample_writer", 1, 1, base.Add(-26*time.Hour), "posted", "", true, "Example post: sample notes https://example.com/notes")
		},
	}
	for _, f := range rows {
		if err := f(); err != nil {
			return nil, err
		}
	}
	// Last month, for the "for reference" line.
	l := now.In(loc)
	prev := time.Date(l.Year(), l.Month()-1, 1, 8, 0, 0, 0, loc)
	for i := 0; i < 12; i++ {
		if err := logRow(int64(8000+i), "sample_writer", 1, 1, prev.Add(time.Duration(i)*time.Hour), "posted", "", i%3 == 0, "Example post from last month."); err != nil {
			return nil, err
		}
	}
	lines = append(lines, "example post log rows (fake post ids on x.invalid)")
	bl, err := seedBlogs(ctx, st, owner, a1, now, loc)
	if err != nil {
		return nil, err
	}
	rl, err := seedReplies(ctx, st, owner, a1, now)
	if err != nil {
		return nil, err
	}
	st.Now = realNow
	return append(append(lines, bl...), rl...), nil
}
