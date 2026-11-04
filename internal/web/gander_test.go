package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog/gander"
	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/blog/hookpub"
	"github.com/scott/xpost/internal/store"
)

const ganderTok = "example-gander-token-0000000000000000"

// ganderEnv runs xpost on a loopback base (so the guarded dialer may reach
// the loopback fake) with Settings' Test wired to the gander adapter over
// Server.BlogHTTP. live false is XPOST_BLOG_LIVE off.
func ganderEnv(t *testing.T, base string, live bool) (*env, *gfake.Server, *store.Blog, *client) {
	return ganderEnvOpts(t, base, live, true)
}

// importAllowed is main's import-only check.
func importAllowed(r *http.Request) bool { return gander.ImportRead(r) || hookpub.ImportRead(r) }

// ganderEnvOpts also sets the import gate (XPOST_BLOG_IMPORT): on, the
// import reads through its own adapter instance and the import-only client,
// as main wires it.
func ganderEnvOpts(t *testing.T, base string, live, imp bool) (*env, *gfake.Server, *store.Blog, *client) {
	fake := &gfake.Server{Token: ganderTok, Now: func() time.Time { return testNow }}
	ts := httptest.NewTLSServer(fake.Handler())
	t.Cleanup(ts.Close)
	fake.Base = ts.URL
	gp := &gander.Publisher{Now: func() time.Time { return testNow }}
	gi := &gander.Publisher{Now: func() time.Time { return testNow }}
	e := newEnv(t, func(c *Config) {
		c.BaseURL = base
		c.WebhookHTTP = ts.Client() // contributes only its TLS roots
		c.BlogTest = func(ctx context.Context, b *store.Blog) (string, error) {
			if !live {
				return "", store.ErrLiveOff
			}
			return gp.Test(ctx, b)
		}
		if imp {
			c.BlogImport = gi.List
		}
		c.ImportAllow = importAllowed
	})
	gi.HTTP = e.srv.BlogImportHTTP
	gp.HTTP = e.srv.BlogHTTP
	gp.Token = func(ctx context.Context, id int64) (string, error) {
		return e.st.BlogSecret(ctx, id, store.BlogAPIToken)
	}
	gi.Token = gp.Token
	c, _ := e.setupOwner()
	ctx := context.Background()
	b, err := e.st.CreateBlog(ctx, e.owner, store.BlogIn{Name: "guide.example.test", SiteURL: ts.URL, Method: store.MethodAPI, APIBase: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetBlogSecret(ctx, e.owner, b.ID, store.BlogAPIToken, ganderTok); err != nil {
		t.Fatal(err)
	}
	return e, fake, b, c
}

func TestSettingsTestThroughGander(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "http://localhost:8090", true)
	ctx := context.Background()
	form := "/settings/blogs/" + strconvI(b.ID)
	if r := c.post("/settings/blogs", form, url.Values{"op": {"test"}}); !strings.Contains(r.loc, "tested") {
		t.Fatalf("pass: %d %s", r.status, r.loc)
	}
	b, _ = e.st.BlogByID(ctx, b.ID)
	if b.TestState != store.TestPassed || !strings.HasPrefix(b.TestDetail, "Listed posts, 200 OK") {
		t.Fatalf("pass: %s %q", b.TestState, b.TestDetail)
	}
	// A wrong token: failed with the status and code only.
	if err := e.st.SetBlogSecret(ctx, e.owner, b.ID, store.BlogAPIToken, "example-wrong-token-1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if r := c.post("/settings/blogs", form, url.Values{"op": {"test"}}); !strings.Contains(r.loc, "testfailed") {
		t.Fatalf("fail: %s", r.loc)
	}
	b, _ = e.st.BlogByID(ctx, b.ID)
	if b.TestState != store.TestFailed || b.TestDetail != "Listing posts returned 401 Unauthorized (invalid_token). Check the API token." {
		t.Fatalf("fail: %s %q", b.TestState, b.TestDetail)
	}
	page := c.get("/settings/blogs?open=" + strconvI(b.ID)).body
	for _, bad := range []string{ganderTok, "example-wrong-token", "not recognized"} {
		if strings.Contains(page, bad) || strings.Contains(e.logs.String(), bad) {
			t.Errorf("page or logs show %q", bad)
		}
	}
	if n := len(fake.Calls()); n != 2 {
		t.Fatalf("%d calls for two tests", n)
	}
}

// Live off: Test says so, makes no call, and keeps the last result.
func TestSettingsTestLiveOff(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "http://localhost:8090", false)
	r := c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"test"}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Live publishing is disabled on this server.") || !strings.Contains(r.body, "tnote") {
		t.Fatalf("off: %d", r.status)
	}
	b, _ = e.st.BlogByID(context.Background(), b.ID)
	if b.TestState == store.TestFailed || b.TestState == store.TestPassed {
		t.Fatalf("off recorded a result: %s", b.TestState)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("off made %d calls", n)
	}
}

// On a public base URL the guarded dialer refuses the loopback fake, and
// the note names no address.
func TestSettingsTestGuardedDialer(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "https://xpost.example.test", true)
	c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"test"}})
	b, _ = e.st.BlogByID(context.Background(), b.ID)
	if b.TestState != store.TestFailed || b.TestDetail != "Listing posts was refused: the address is private or reserved. Nothing more was sent." {
		t.Fatalf("%s %q", b.TestState, b.TestDetail)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("the fake got %d calls past the dialer", n)
	}
}

// A blog post blocked by a taken slug shows the blocked state, not Retry.
func TestBlockedSlugCard(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("notes.example.test")
	bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("notes.example.test", "sample-week", "Example opening.")))
	e.approveNow(bo.ID)
	ctx := context.Background()
	bd := e.draft(bo.ID)
	if err := e.st.PublishNow(ctx, e.owner, bd.ID, bd.Version); err != nil {
		t.Fatal(err)
	}
	due, _ := e.st.DueBlogs(ctx, e.clock())
	e.st.Claim(ctx, due[0], e.clock().Add(time.Minute))
	e.st.FinishBlog(ctx, bo.ID, false, "", "", store.BlogSlugTaken+" xpost will not overwrite a post it did not create.")
	q := c.get("/queue?kind=blog").body
	for _, want := range []string{">blocked</span>", "Blocked at ", "the slug is taken. Nothing was published.", "Change slug", "/drafts/" + strconvI(bo.ID) + "/edit"} {
		if !strings.Contains(q, want) {
			t.Errorf("blocked card lacks %q", want)
		}
	}
	if strings.Contains(q, "Retry publish") {
		t.Error("blocked card offers Retry publish")
	}
}
