package poster

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The guard against accidental X traffic, slice 4 edition. Posting and
// media upload are real code, so the rules pin them to one place:
//
//   - Only internal/xauth/config.go names X's hosts (as defaults tests
//     override).
//   - The create-post path (/2/tweets) and the two media paths
//     (/2/media/upload, /2/media/metadata) appear only in config.go (the
//     default URLs), the fake provider, and the fake's launcher. Every
//     other X posting or media path (tweet ids, deletes, likes, v1.1,
//     chunked upload, subtitles, anything else under /2/media) appears
//     nowhere.
//   - Only internal/xauth/posts.go sends requests to Config.PostURL,
//     Config.MediaURL and Config.MediaMetaURL, exactly one call site each
//     (and one GET on MediaURL for the processing check), and it is the
//     only file besides config.go (and main's startup log line) that names
//     them.
//   - Outbound HTTP is made only by internal/xauth and the fake's launcher,
//     plus one POST in internal/web/webhook.go: the rewrite request to the
//     URL the owner saved for an agent (never an X host, never a token),
//     dialed only through internal/web/hookdial.go's address check and
//     never through a proxy;
//     plus internal/blog/gander/gander.go: the gander Publisher API
//     adapter, which builds every request in one place and sends it only
//     through the guarded client web.Server.BlogHTTP gives it (the same
//     checked dialer, no proxy, no redirects), and only when
//     XPOST_BLOG_LIVE=1 (its own guard is internal/blog/guard_test.go);
//     and internal/blog/hookpub/hookpub.go, the webhook blog adapter (the
//     signed wake and the live page check), under the same rules;
//     package poster has no net/http; the web package never imports
//     package poster or names XPoster, so no HTTP handler can post.
//   - Nothing sends an HTTP DELETE.
func TestNoRealXAPI(t *testing.T) {
	hosts := regexp.MustCompile(`(?i)\b(api\.x\.com|api\.twitter\.com|upload\.twitter\.com|upload\.x\.com|(www\.)?twitter\.com|//x\.com)\b`)
	createPath := regexp.MustCompile(`/2/tweets\b`)
	mediaPath := regexp.MustCompile(`/2/media(/[A-Za-z_]+)*`)
	otherPaths := regexp.MustCompile(`/2/tweets/|/1\.1/|media/upload\.json|/2/users/[^"\s]*/(likes|retweets|bookmarks)`)
	outbound := regexp.MustCompile(`http\.(NewRequest|NewRequestWithContext|Post|Get|PostForm|Head)\(|\.HTTP\.Do\(|http\.DefaultClient`)
	postReq := regexp.MustCompile(`NewRequestWithContext\(ctx, "POST", c\.Cfg\.PostURL`)
	uploadReq := regexp.MustCompile(`NewRequestWithContext\(ctx, "POST", c\.Cfg\.MediaURL,`)
	statusReq := regexp.MustCompile(`NewRequestWithContext\(ctx, "GET", c\.Cfg\.MediaURL\+"\?"`)
	altReq := regexp.MustCompile(`NewRequestWithContext\(ctx, "POST", c\.Cfg\.MediaMetaURL,`)
	anyReq := regexp.MustCompile(`NewRequestWithContext\(`)
	xURLName := regexp.MustCompile(`\b(PostURL|MediaURL|MediaMetaURL)\b`)
	del := regexp.MustCompile(`"DELETE"|http\.MethodDelete`)
	const (
		config      = "internal/xauth/config.go"
		posts       = "internal/xauth/posts.go"
		hookFile    = "internal/web/webhook.go"
		ganderFile  = "internal/blog/gander/gander.go"
		hookpubFile = "internal/blog/hookpub/hookpub.go"
	)
	hookReq := regexp.MustCompile(`http\.NewRequestWithContext\(ctx, http\.MethodPost, hookURL, `)
	allowCreatePath := map[string]bool{config: true, "internal/xauth/xfake/xfake.go": true, "internal/xauth/xfake/media.go": true, "cmd/xpost-fakex/main.go": true}
	allowPostURL := map[string]bool{config: true, posts: true, "cmd/xpost/main.go": true}
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || strings.HasSuffix(p, "_test.go") || !(strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(b)
		seen[rel] = true
		if rel != config {
			if m := hosts.FindString(src); m != "" {
				t.Errorf("%s names an X host (%s); only %s may", rel, m, config)
			}
		}
		if !allowCreatePath[rel] && createPath.MatchString(src) {
			t.Errorf("%s names the create-post path; only %s (default URL) and the fake may", rel, config)
		}
		for _, m := range mediaPath.FindAllString(src, -1) {
			if m != "/2/media/upload" && m != "/2/media/metadata" {
				t.Errorf("%s names an X media path (%s) that xpost never calls", rel, m)
			} else if !allowCreatePath[rel] {
				t.Errorf("%s names a media path (%s); only %s (default URL) and the fake may", rel, m, config)
			}
		}
		if m := otherPaths.FindString(src); m != "" {
			t.Errorf("%s names an X posting/media path (%s) that xpost never calls", rel, m)
		}
		if !allowPostURL[rel] {
			if m := xURLName.FindString(src); m != "" {
				t.Errorf("%s uses %s; only %s calls X", rel, m, posts)
			}
		}
		if rel != posts {
			for _, re := range []*regexp.Regexp{postReq, uploadReq, statusReq, altReq} {
				if re.MatchString(src) {
					t.Errorf("%s sends a request to an X posting/media URL; only %s may", rel, posts)
				}
			}
		}
		if strings.HasSuffix(rel, ".go") && del.MatchString(src) {
			t.Errorf("%s sends an HTTP DELETE", rel)
		}
		if rel == hookFile {
			if n := len(hookReq.FindAllString(src, -1)); n != 1 || len(anyReq.FindAllString(src, -1)) != 1 {
				t.Errorf("%s must build exactly one request, the POST to the saved webhook URL", rel)
			}
			if m := regexp.MustCompile(`http\.(Post|Get|PostForm|Head)\(|http\.DefaultClient`).FindString(src); m != "" {
				t.Errorf("%s makes other outbound HTTP (%s)", rel, m)
			}
		} else if rel == ganderFile || rel == hookpubFile {
			if n := len(anyReq.FindAllString(src, -1)); n != 1 {
				t.Errorf("%s must build exactly one request (its do helper), not %d", rel, n)
			}
			if m := regexp.MustCompile(`http\.(Post|Get|PostForm|Head)\(|http\.DefaultClient|http\.DefaultTransport|http\.Client\{|http\.Transport\{`).FindString(src); m != "" {
				t.Errorf("%s makes outbound HTTP other than through the injected client (%s)", rel, m)
			}
		} else if strings.HasSuffix(rel, ".go") && !strings.HasPrefix(rel, "internal/xauth/") && !strings.HasPrefix(rel, "cmd/xpost-fakex/") {
			if m := outbound.FindString(src); m != "" {
				t.Errorf("%s makes outbound HTTP (%s); only internal/xauth may", rel, m)
			}
		}
		if strings.HasPrefix(rel, "internal/poster/") && strings.Contains(src, `"net/http"`) {
			t.Errorf("%s imports net/http", rel)
		}
		if strings.HasPrefix(rel, "internal/web/") && (strings.Contains(src, `xpost/internal/poster"`) || strings.Contains(src, "XPoster")) {
			t.Errorf("%s reaches the poster from the web package", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The webhook client dials through the destination check and never a proxy.
	if b, err := os.ReadFile(filepath.Join(root, "internal/web/hookdial.go")); err != nil {
		t.Fatal(err)
	} else {
		for _, must := range []string{"t.Proxy = nil", "t.DialContext = s.hookDial", "Control: s.hookControl", "t.DialTLSContext = nil"} {
			if !strings.Contains(string(b), must) {
				t.Errorf("internal/web/hookdial.go lost %q", must)
			}
		}
	}
	for _, f := range []string{config, posts, hookFile, ganderFile, hookpubFile} {
		if !seen[f] {
			t.Fatalf("%s not found; update this test", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, posts))
	for name, re := range map[string]*regexp.Regexp{"create-post": postReq, "media upload": uploadReq, "media status": statusReq, "media metadata": altReq} {
		if n := len(re.FindAllString(string(b), -1)); n != 1 {
			t.Fatalf("%s has %d %s call sites, want exactly 1", posts, n, name)
		}
	}
	if n := len(anyReq.FindAllString(string(b), -1)); n != 4 {
		t.Fatalf("%s builds %d requests, want exactly the 4 above", posts, n)
	}
}
