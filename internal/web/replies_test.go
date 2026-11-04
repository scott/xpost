package web

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Invented ids and handles only: every original here is example data.
const replyOrigID = "1900000000000000555"

func replyBody(account, text string, extra map[string]any) map[string]any {
	b := map[string]any{"kind": "reply", "account": account, "in_reply_to": replyOrigID, "parts": []map[string]any{{"text": text}},
		"original": map[string]any{"author_name": "Sample Builder", "author_handle": "sample_builder", "text": "Example post: what do you check before deploy?",
			"created_at": testNow.Add(-2 * time.Hour).Format(time.RFC3339), "url": "https://x.com/sample_builder/status/" + replyOrigID}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

type replyOut struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Reply struct {
		InReplyTo string `json:"in_reply_to"`
		Original  struct {
			AuthorName   string `json:"author_name"`
			AuthorHandle string `json:"author_handle"`
			Text         string `json:"text"`
			CreatedAt    string `json:"created_at"`
			Unavailable  bool   `json:"unavailable"`
		} `json:"original"`
		ReplySettings string   `json:"reply_settings"`
		PostsBy       string   `json:"posts_by"`
		ApproveBlock  string   `json:"approve_block"`
		Warnings      []string `json:"warnings"`
	} `json:"reply"`
	ScheduledAt *string `json:"scheduled_at"`
}

func (e *env) proposeReply(bearer string, body map[string]any) replyOut {
	e.t.Helper()
	r := e.api("POST", "/api/v1/drafts", bearer, body)
	if r.status != http.StatusCreated {
		e.t.Fatalf("propose reply: %d %s", r.status, r.body)
	}
	var o replyOut
	if err := json.Unmarshal([]byte(r.body), &o); err != nil {
		e.t.Fatal(err)
	}
	return o
}

// replyCardHTML is one card's markup on a queue page.
func replyCardHTML(t *testing.T, body string, id int64) string {
	t.Helper()
	i := strings.Index(body, `id="d`+strconvI(id)+`"`)
	if i < 0 {
		t.Fatalf("no card d%d", id)
	}
	j := strings.Index(body[i:], "</article>")
	return body[i : i+j]
}

func mustLack(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(body, b) {
			t.Errorf("page has %q", b)
		}
	}
}

// The agent API: in_reply_to as an id or an x.com link (stored as the
// digits), the original's fields, reply_settings, and the errors.
func TestReplyAPI(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	o := e.proposeReply(sec, replyBody(sm, "Example reply", map[string]any{"reply_settings": "following"}))
	if o.Kind != "reply" || o.Reply.InReplyTo != replyOrigID || o.Reply.Original.AuthorHandle != "sample_builder" || o.Reply.Original.AuthorName != "Sample Builder" ||
		o.Reply.Original.CreatedAt == "" || o.Reply.PostsBy != "hand" || o.ScheduledAt != nil || o.Reply.ApproveBlock != "" {
		t.Fatalf("reply out: %+v", o)
	}
	if len(o.Reply.Warnings) != 1 || !strings.Contains(o.Reply.Warnings[0], "limits replies to accounts they follow") {
		t.Fatalf("warnings %q", o.Reply.Warnings)
	}
	// A status link normalizes to the id; kind may be left out.
	b := replyBody(sm, "Example reply two", nil)
	delete(b, "kind")
	b["in_reply_to"] = "https://x.com/sample_builder/status/" + replyOrigID + "?s=20"
	o2 := e.proposeReply(sec, b)
	if o2.Kind != "reply" || o2.Reply.InReplyTo != replyOrigID {
		t.Fatalf("link: %+v", o2)
	}
	d := e.draft(o2.ID)
	if !d.IsReply() || d.Reply.InReplyTo != replyOrigID {
		t.Fatalf("stored: %+v", d.Reply)
	}
	// Duplicate: same post, same account, warns.
	if !strings.Contains(strings.Join(o2.Reply.Warnings, " "), "already has another reply to this post") {
		t.Fatalf("duplicate warning missing: %q", o2.Reply.Warnings)
	}
	// Over 280 is accepted at intake; Approve is what it blocks.
	o3 := e.proposeReply(sec, replyBody(sm, strings.Repeat("a", 296), map[string]any{"in_reply_to": "1900000000000000999", "original": nil}))
	if !o3.Reply.Original.Unavailable || o3.Reply.ApproveBlock == "" {
		t.Fatalf("unavailable: %+v", o3.Reply)
	}
	errs := []map[string]any{
		replyBody(sm, "x", map[string]any{"in_reply_to": "https://x.com/sample_builder"}),
		replyBody(sm, "x", map[string]any{"in_reply_to": "https://example.test/a/status/1"}),
		replyBody(sm, "x", map[string]any{"in_reply_to": "12ab"}),
		replyBody(sm, "x", map[string]any{"in_reply_to": ""}),
		replyBody(sm, "x", map[string]any{"reply_settings": "nobody"}),
		replyBody(sm, "x", map[string]any{"proposed_at": testNow.Add(time.Hour).Format(time.RFC3339)}),
		replyBody(sm, "x", map[string]any{"promotes": 3}),
		replyBody(sm, "x", map[string]any{"original": map[string]any{"author_handle": "sample_builder", "text": "t", "url": "https://x.com/sample_builder/status/1"}}),
		replyBody(sm, "x", map[string]any{"original": map[string]any{"author_handle": "sample_builder", "text": "t", "url": "https://x.com/other_one/status/" + replyOrigID}}),
		replyBody(sm, "x", map[string]any{"original": map[string]any{"author_handle": "sample_builder", "text": "t", "created_at": "yesterday"}}),
		replyBody(sm, "x", map[string]any{"original": map[string]any{"author_handle": "sample_builder", "text": "t", "followers": 5}}),
		replyBody(sm, "x", map[string]any{"parts": []map[string]any{{"text": "a"}, {"text": "b"}}}),
		{"account": sm, "parts": []map[string]any{{"text": "x"}}, "proposed_at": testNow.Add(time.Hour).Format(time.RFC3339), "original": map[string]any{"text": "t"}},
		{"kind": "blog", "site": "notes.example.test", "in_reply_to": replyOrigID},
	}
	for i, body := range errs {
		if r := e.api("POST", "/api/v1/drafts", sec, body); r.status != http.StatusBadRequest {
			t.Errorf("case %d: %d %s", i, r.status, r.body)
		}
	}
	// A plain X post is unchanged.
	if id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post"); e.draft(id).IsReply() {
		t.Fatal("plain post became a reply")
	}
	// drafts:write covers replies; a token without it can't.
	_, ro := e.token("sample-reader", store.ScopeLogRead)
	if r := e.api("POST", "/api/v1/drafts", ro, replyBody(sm, "x", nil)); r.status != http.StatusForbidden && r.status != http.StatusNotFound && r.status != http.StatusUnauthorized {
		t.Fatalf("no scope: %d", r.status)
	}
}

// The queue card (08a): Reply pill, the quoted original, Replies filter,
// by hand on X, $0 API. Approve opens the hand-off (08c): Open on X to
// post, Copy text, Unapprove. Opening records the time and goes to the
// x.com intent URL (08c2): Mark posted, Open on X again, Put back in queue.
func TestReplyQueueFlow(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	text := "Example reply: I re-read the diff & check the rollback."
	id := e.proposeReply(sec, replyBody(sm, text, nil)).ID
	q := c.get("/queue").body
	mustHave(t, q, `class="kp"`, `>Reply</span>`, "Replying to", "Sample Builder", "@sample_builder", "Example post: what do you check before deploy?",
		"2h ago", "View on X", "Replying from", `>Replies</a>`, `>X posts</a>`, "Posts by hand on X", "$0 API", "By hand on X",
		"Approve opens x.com with this reply filled in. You press Post there.", "/drafts/"+strconvI(id)+"/approve")
	mustLack(t, replyCardHTML(t, q, id), "/reschedule", "est. ", "Reject draft")
	rq := c.get("/queue?kind=reply").body
	mustHave(t, rq, "Example reply: I re-read", "waiting for you: X posts and replies.")
	mustLack(t, rq, ">Example post</p>")
	xq := c.get("/queue?kind=x").body
	mustLack(t, xq, "Example reply: I re-read")

	r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	a := c.get("/queue?status=approved").body
	mustHave(t, a, "Approved. Post it on X yourself.", "under @sample_builder", `action="/drafts/`+strconvI(id)+`/open-x" target="_blank"`, "Open on X to post", "Copy text",
		`data-copy="copy`+strconvI(id)+`"`, "Unapprove", "Ready for X")
	r = c.post("/queue?status=approved", path(id, "/open-x"), e.ownerForm(id, url.Values{"return": {"/queue?status=approved"}}))
	want := "https://x.com/intent/post?text=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20") + "&in_reply_to=" + replyOrigID
	if r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("open: %d %q want %q", r.status, r.loc, want)
	}
	d := e.draft(id)
	if d.Reply.OpenedAt == nil || d.Status != store.StatusApproved {
		t.Fatalf("opened: %+v %s", d.Reply, d.Status)
	}
	o := c.get("/queue?status=approved").body
	mustHave(t, o, "Opened on X at", "Pressed Post on X? Mark it posted here so the log stays right. xpost cannot see what happens on X.",
		"Mark posted", "Open on X again", "Put back in queue", "opened on X", "Waiting for you to mark it.")
	mustLack(t, o, "Open on X to post")
	// Put back in queue clears it; approve and open again.
	r = c.post("/queue?status=approved", path(id, "/unapprove"), e.ownerForm(id, url.Values{"return": {"/queue?status=approved"}}))
	if r.status != http.StatusSeeOther || e.draft(id).Status != store.StatusPending || e.draft(id).Reply.OpenedAt != nil {
		t.Fatalf("put back: %d %s", r.status, e.draft(id).Status)
	}
	e.approveNow(id)
	c.post("/queue?status=approved", path(id, "/open-x"), e.ownerForm(id, url.Values{"return": {"/queue?status=approved"}}))

	// Mark posted (08c3): the dialog, then format checks (08c4).
	m := c.get("/queue?status=approved&mark=" + strconvI(id)).body
	mustHave(t, m, "Mark as posted", "Reply to @sample_builder", "Link to your reply", `name="link"`,
		`Leave it empty if you do not have the link. The log will say "Posted manually, no link".`, `action="/drafts/`+strconvI(id)+`/mark-posted"`)
	ret := "/queue?status=approved"
	for link, msg := range map[string]string{
		"https://x.com/sample_writer":                           "This is a profile link, not a post. Paste a link like x.com/sample_writer/status/1234567890, or clear the field.",
		"https://x.com/someone_else/status/1900000000000000777": "This post is from @someone_else, not @sample_writer.",
		"https://example.test/sample_writer/status/1":           "This is not an x.com link.",
	} {
		r = c.post(ret, path(id, "/mark-posted"), e.ownerForm(id, url.Values{"return": {ret}, "link": {link}}))
		if r.status != http.StatusBadRequest || !strings.Contains(html.UnescapeString(r.body), msg) || !strings.Contains(r.body, "Mark as posted") {
			t.Errorf("%s: %d lacks %q", link, r.status, msg)
		}
		if e.draft(id).Status != store.StatusApproved {
			t.Fatalf("bad link marked it posted")
		}
	}
	r = c.post(ret, path(id, "/mark-posted"), e.ownerForm(id, url.Values{"return": {ret}, "link": {"x.com/Sample_Writer/status/1900000000000000777?s=20"}}))
	if r.status != http.StatusSeeOther {
		t.Fatalf("mark: %d %s", r.status, r.body)
	}
	d = e.draft(id)
	if d.Status != store.StatusPosted || d.Reply.PostedURL != "https://x.com/sample_writer/status/1900000000000000777" {
		t.Fatalf("posted: %s %q", d.Status, d.Reply.PostedURL)
	}
	// Posted list (08e): with the link, and without.
	id2 := e.proposeReply(sec, replyBody(jg, "Example reply from the other account", map[string]any{"in_reply_to": "1900000000000000666", "original": nil})).ID
	e.st.RetryReplyLoad(t.Context(), e.owner, id2, e.draft(id2).Version) // nothing to read: stays unavailable
	id3 := e.proposeReply(sec, replyBody(jg, "Example reply posted without a link", nil)).ID
	e.approveNow(id3)
	e.st.OpenedOnX(t.Context(), e.owner, id3, e.draft(id3).Version)
	c.post(ret, path(id3, "/mark-posted"), e.ownerForm(id3, url.Values{"return": {ret}, "link": {""}}))
	p := c.get("/queue?status=posted").body
	mustHave(t, p, "@sample_writer · in reply to @sample_builder · Posted by you on X", `href="https://x.com/sample_writer/status/1900000000000000777"`,
		"@sample_maker · in reply to @sample_builder · Posted by you on X", "Posted manually, no link")
}

// Opened over a day ago and not marked (08c5): "Not marked yet", still
// approved; it never reverts by itself.
func TestReplyNotMarkedYet(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.proposeReply(sec, replyBody(sm, "Example reply", nil)).ID
	e.approveNow(id)
	e.st.OpenedOnX(t.Context(), e.owner, id, e.draft(id).Version)
	// Opened 25 hours ago (moving the clock would end the session).
	if _, err := e.st.DB.Exec(`UPDATE draft_replies SET opened_at = ? WHERE draft_id = ?`, testNow.Add(-25*time.Hour).Unix(), id); err != nil {
		t.Fatal(err)
	}
	b := c.get("/queue?status=approved").body
	mustHave(t, b, "Not marked yet", "over a day ago. Did you post it? Mark it posted, open X again, or put it back in the queue.", "Mark posted", "Put back in queue")
	if e.draft(id).Status != store.StatusApproved {
		t.Fatal("reverted")
	}
}

// Original unavailable (08d1) blocks Approve and offers Retry load, which
// reads nothing from X. Over 280 (08d2) blocks Approve with the count and
// the overflow marked. Lint errors block; the limited-replies warning
// (08d4) does not.
func TestReplyBlockersInTheQueue(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	un := e.proposeReply(sec, replyBody(sm, "Example reply", map[string]any{"original": map[string]any{"unavailable": true}})).ID
	q := c.get("/queue").body
	mustHave(t, q, "Original post unavailable.", "It may have been deleted, or X did not return it. Last tried", "Post ID "+replyOrigID, "Retry load",
		"Can&#39;t approve: the original post didn&#39;t load. Retry, or reject this reply.", `/drafts/`+strconvI(un)+`/retry-load`)
	r := c.post("/queue", path(un, "/approve"), e.ownerForm(un, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusConflict || e.draft(un).Status != store.StatusPending {
		t.Fatalf("approve unavailable: %d", r.status)
	}
	r = c.post("/queue", path(un, "/retry-load"), e.ownerForm(un, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=notloaded") {
		t.Fatalf("retry: %d %s", r.status, r.loc)
	}
	e.st.Delete(t.Context(), e.owner, un, e.draft(un).Version)

	long := "Example reply that runs long on purpose. " + strings.Repeat("More words here. ", 15)
	over := e.proposeReply(sec, replyBody(sm, long[:296], nil)).ID
	q = c.get("/queue").body
	mustHave(t, q, "296 / 280", `class="chip t bad"`, `<span class="ov">`, "16 over the 280 limit.", "Shorten it in Edit, or ask sample-agent to rewrite it.", "Shorten by 16 to approve.")
	r = c.post("/queue", path(over, "/approve"), e.ownerForm(over, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusConflict {
		t.Fatalf("approve over: %d", r.status)
	}
	e.st.Delete(t.Context(), e.owner, over, e.draft(over).Version)

	em := e.proposeReply(sec, replyBody(sm, "Example reply \u2014 em dash", nil)).ID
	if r := c.post("/queue", path(em, "/approve"), e.ownerForm(em, url.Values{"return": {"/queue"}})); r.status != http.StatusConflict {
		t.Fatalf("approve em dash: %d", r.status)
	}
	e.st.Delete(t.Context(), e.owner, em, e.draft(em).Version)

	lim := e.proposeReply(sec, replyBody(sm, "Example reply", map[string]any{"reply_settings": "following"})).ID
	mustHave(t, c.get("/queue").body, "@sample_builder limits replies to accounts they follow. X may not let @sample_writer post this reply. You can still approve and try on X.")
	if r := c.post("/queue", path(lim, "/approve"), e.ownerForm(lim, url.Values{"return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("approve limited: %d", r.status)
	}
}

// The edit view pins the original above the editor and saves over 280
// (Approve is what the limit blocks).
func TestReplyEditView(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.proposeReply(sec, replyBody(sm, "Example reply", nil)).ID
	b := c.get(path(id, "")).body
	mustHave(t, b, "Edit reply", `<div class="rsticky"><div class="oq">`, "Replying to", "@sample_builder", "X counts links as 23. Limit 280.", "Saving keeps it in Pending.", "Checks", "History")
	mustLack(t, b, "Save and approve", "Reschedule", "Estimated cost")
	if i, j := strings.Index(b, `class="rsticky"`), strings.Index(b, `name="text_0"`); i < 0 || j < i {
		t.Fatal("the original is not above the editor")
	}
	r := c.post(path(id, ""), path(id, ""), e.ownerForm(id, url.Values{"op": {"save"}, "text_0": {strings.Repeat("d", 300)}}))
	if r.status != http.StatusSeeOther {
		t.Fatalf("save over 280: %d %s", r.status, r.body)
	}
	if d := e.draft(id); len(d.Parts) != 1 || len(d.Parts[0].Text) != 300 || d.Status != store.StatusPending {
		t.Fatalf("saved: %+v", d)
	}
	// Reschedule is refused for a reply.
	if r := c.post("/queue", path(id, "/reschedule"), e.ownerForm(id, url.Values{"op": {"move"}, "date": {"2026-06-02"}, "time": {"09:00"}})); r.status != http.StatusConflict {
		t.Fatalf("reschedule: %d", r.status)
	}
}

// Pages with replies keep the CSP and have no inline style or script.
func TestReplyPagesHaveNoInlineStyleOrScript(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.proposeReply(sec, replyBody(sm, "Example reply", nil)).ID
	un := e.proposeReply(sec, replyBody(sm, "Example reply", map[string]any{"in_reply_to": "1900000000000000999", "original": nil})).ID
	e.approveNow(id)
	e.st.OpenedOnX(t.Context(), e.owner, id, e.draft(id).Version)
	for _, p := range []string{"/queue", "/queue?kind=reply", "/queue?status=approved", "/queue?status=approved&mark=" + strconvI(id), path(id, ""), path(un, "")} {
		r := c.get(p)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d", p, r.status)
		}
		if m := styleAttr.FindString(r.body); m != "" {
			t.Errorf("%s has inline %q", p, m)
		}
		if strings.Contains(r.body, "\u2014") {
			t.Errorf("%s has an em dash", p)
		}
	}
}

// Rewrite works for replies: the request carries kind "reply" and the
// original post, and the revised text comes back as one part (over 280 is
// allowed in; it blocks Approve).
func TestReplyRewrite(t *testing.T) {
	e := newRewriteEnv(t)
	id := e.proposeReply(e.sec, replyBody(sm, "Example reply to rewrite", map[string]any{"reply_settings": "mentionedUsers"})).ID
	mustHave(t, replyCardHTML(t, e.c.get("/queue").body, id), `class="btn rwb"`)
	if r := e.rewrite(id, "Shorter, sample note"); r.status != http.StatusSeeOther {
		t.Fatalf("rewrite: %d %s", r.status, r.body)
	}
	hits := e.hook.hits()
	if len(hits) != 1 {
		t.Fatalf("webhook got %d requests", len(hits))
	}
	var p struct {
		Kind  string `json:"kind"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
		Reply struct {
			InReplyTo string `json:"in_reply_to"`
			Account   string `json:"account"`
			Original  struct {
				AuthorHandle string `json:"author_handle"`
				Text         string `json:"text"`
			} `json:"original"`
			ReplySettings string `json:"reply_settings"`
		} `json:"reply"`
	}
	if err := json.Unmarshal(hits[0].body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != "reply" || len(p.Parts) != 1 || p.Parts[0].Text != "Example reply to rewrite" || p.Reply.InReplyTo != replyOrigID || p.Reply.Account != sm ||
		p.Reply.Original.AuthorHandle != "sample_builder" || p.Reply.Original.Text == "" || p.Reply.ReplySettings != "mentionedUsers" {
		t.Fatalf("payload %s", hits[0].body)
	}
	req := e.lastRequestID(id)
	if r := e.revise(e.sec, id, req, "a", "b"); r.status != http.StatusBadRequest {
		t.Fatalf("two parts: %d %s", r.status, r.body)
	}
	if r := e.revise(e.sec, id, req, strings.Repeat("e", 290)); r.status != http.StatusOK {
		t.Fatalf("revise: %d %s", r.status, r.body)
	}
	if d := e.draft(id); !d.IsReply() || len(d.Parts) != 1 || len(d.Parts[0].Text) != 290 || d.Status != store.StatusPending {
		t.Fatalf("revised: %+v", d)
	}
}

// SSE / poll: every reply state (approved, opened on X, back in queue,
// posted) reaches open tabs as a draft change with the new version.
func TestReplyStatesReachLiveTabs(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.proposeReply(sec, replyBody(sm, "Example reply", nil)).ID
	var out livePollOut
	read := func(since int64) {
		t.Helper()
		out = livePollOut{}
		if err := json.Unmarshal([]byte(c.get("/live/poll?since="+strconvI(since)).body), &out); err != nil {
			t.Fatal(err)
		}
	}
	read(0)
	seq := out.Seq
	ret := url.Values{"return": {"/queue"}}
	for _, step := range []string{"/approve", "/open-x", "/unapprove", "/approve", "/open-x", "/mark-posted"} {
		v := e.draft(id).Version
		if r := c.post("/queue", path(id, step), e.ownerForm(id, ret)); r.status != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", step, r.status, r.body)
		}
		read(seq)
		if len(out.Changes) == 0 || out.Changes[len(out.Changes)-1].ID != id || out.Changes[len(out.Changes)-1].V != int64(v+1) {
			t.Fatalf("%s: changes %+v", step, out.Changes)
		}
		seq = out.Seq
	}
	if e.draft(id).Status != store.StatusPosted {
		t.Fatal("not posted")
	}
}
