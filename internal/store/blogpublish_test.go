package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

func TestBlogZoneAndPublishToken(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := store.BlogIn{Name: "notes.example.test", SiteURL: "https://notes.example.test", Method: store.MethodWebhook}
	for _, z := range []string{"Mars/Base", "Local", "America/Denver/x"} {
		in.Zone = z
		if _, err := f.st.CreateBlog(ctx, f.owner, in); err == nil {
			t.Errorf("zone %q accepted", z)
		}
	}
	// A drafts:write token cannot be the Publishing token.
	in.Zone, in.PublishTokenID = "Pacific/Auckland", f.a.ID
	if _, err := f.st.CreateBlog(ctx, f.owner, in); err == nil {
		t.Fatal("a drafts:write token was bound")
	}
	pub, _, err := f.st.CreateAgentToken(ctx, f.owner, "sample-publisher", []string{store.ScopeBlogsPublish})
	if err != nil {
		t.Fatal(err)
	}
	in.PublishTokenID = pub.ID
	b, err := f.st.CreateBlog(ctx, f.owner, in)
	if err != nil || b.Zone != "Pacific/Auckland" || b.PublishTokenID != pub.ID {
		t.Fatalf("create: %+v %v", b, err)
	}
	// The default slot is 07:00 in the blog's zone, not xpost's.
	f.ready(t, b)
	d := f.propose(t, post("notes.example.test", "sample-week"))
	if at := d.ScheduledAt.In(b.Location(time.UTC)); at.Hour() != 7 || at.Minute() != 0 {
		t.Fatalf("default slot %v", at)
	}
	// An API blog never keeps a Publishing token.
	in.Method, in.APIBase = store.MethodAPI, "https://api.example.test"
	b2, err := f.st.UpdateBlog(ctx, f.owner, b.ID, in)
	if err != nil || b2.PublishTokenID != 0 {
		t.Fatalf("api blog token %d %v", b2.PublishTokenID, err)
	}
	// A revoked token cannot be bound.
	in.Method = store.MethodWebhook
	f.st.RevokeAgentToken(ctx, f.owner, pub.ID)
	if _, err := f.st.UpdateBlog(ctx, f.owner, b.ID, in); err == nil {
		t.Fatal("a revoked token was bound")
	}
}
