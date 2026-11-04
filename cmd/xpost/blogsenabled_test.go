package main

import (
	"strings"
	"testing"
)

// XPOST_BLOGS_ENABLED: off by default; only 1 turns blogs on, and the blog
// live and import switches refuse to start without it.
func TestBlogsEnabledFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "1": true, " 1": true} {
		if got, err := blogsEnabledFromEnv(v); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"on", "true", "yes", "2"} {
		if _, err := blogsEnabledFromEnv(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	dbp := testEnv(t)
	recordDispatch(t)
	args := []string{"-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies", "-fake-poster"}
	code, logs := serveFor(t, args...)
	if code != 0 || !strings.Contains(logs, "blogs off (XPOST_BLOGS_ENABLED not 1)") || strings.Contains(logs, "fake blog publisher on") || lastBlogPub != nil {
		t.Fatalf("default: exit %d %s (publisher %#v)", code, logs, lastBlogPub)
	}
	t.Setenv(EnvBlogsEnabled, "on")
	if code, logs := serveFor(t, args...); code == 0 || !strings.Contains(logs, "XPOST_BLOGS_ENABLED must be 1 or 0") {
		t.Fatalf("typo: exit %d %s", code, logs)
	}
	t.Setenv(EnvBlogsEnabled, "")
	for _, k := range []string{EnvBlogLive, EnvBlogImport} {
		t.Setenv(k, "1")
		if code, logs := serveFor(t, args...); code == 0 || !strings.Contains(logs, "needs XPOST_BLOGS_ENABLED=1") {
			t.Fatalf("%s without blogs: exit %d %s", k, code, logs)
		}
		t.Setenv(k, "")
	}
	t.Setenv(EnvBlogsEnabled, "1")
	if code, logs := serveFor(t, args...); code != 0 || !strings.Contains(logs, "blogs on (XPOST_BLOGS_ENABLED=1)") || !strings.Contains(logs, "fake blog publisher on") || lastBlogPub == nil {
		t.Fatalf("on: exit %d %s", code, logs)
	}
}
