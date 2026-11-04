package blog

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoRealBlogHost is TestNoRealXAPI's guard for blogs. Slice 2 adds
// one real adapter, gander's Publisher API, so:
//
//   - under internal/blog and internal/blogtext only
//     internal/blog/gander/gander.go may import net/http (and net/url),
//     and the fake gander server (gfake) net/http for serving; nothing
//     there imports net or xauth;
//   - gander.go builds exactly one request, sends it only through the
//     injected client (never one it builds, a default client or a
//     transport, never a proxy setting or InsecureSkipVerify), uses only
//     GET, PUT and POST, and never DELETE;
//   - gfake makes no outbound request at all;
//   - the Publisher API path appears only in gander.go and gfake; main
//     gives the adapter only the server's guarded client;
//   - slice 3 adds internal/blog/hookpub/hookpub.go, the webhook blog
//     adapter (the signed wake and the live page check), under the same
//     rules: net/http only, one request built in one place, sent only
//     through the injected client, GET and POST only; its fake (hfake)
//     only serves, like gfake;
//   - the only Publishers are FakePublisher, Methods and gander.Publisher;
//   - no non-test file names the blog hosts' real addresses, another
//     publisher API path, or a token prefix: those belong in fixtures only.
func TestNoRealBlogHost(t *testing.T) {
	root := filepath.Join("..", "..")
	// The owner's real blog hosts are checked by hash, so this public test
	// does not name them: sha256 prefixes of the private blog and
	// publisher hosts. Any URL host in a non-test file that hashes to one
	// of these fails.
	urlHost := regexp.MustCompile(`(?i)https?://(?:www\.)?([a-z0-9.-]+)`)
	realHost := map[string]bool{"3a78ba896adea3b7": true, "ca76c44d4c0db84d": true, "eee88eb737140286": true}
	hostHit := func(src string) string {
		for _, m := range urlHost.FindAllStringSubmatch(src, -1) {
			h := strings.TrimSuffix(strings.ToLower(m[1]), ".")
			sum := sha256.Sum256([]byte(h))
			if realHost[hex.EncodeToString(sum[:])[:16]] {
				return m[0]
			}
		}
		return ""
	}
	apiPath := regexp.MustCompile(`/v1/posts\b|/api/publish\b|gmdblog_`)
	apiBlogPath := regexp.MustCompile(`/api/blog/`)
	const ganderFile, fakeDir = "internal/blog/gander/gander.go", "internal/blog/gander/gfake/"
	const hookFile, hookFakeDir = "internal/blog/hookpub/hookpub.go", "internal/blog/hookpub/hfake/"
	adapter := map[string]bool{ganderFile: true, hookFile: true}
	seenGander, seenHook := false, false
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
		if m := hostHit(src); m != "" {
			t.Errorf("%s names a real blog host (%s); use example.test fixtures", rel, m)
		}
		if m := apiPath.FindString(src); m != "" {
			t.Errorf("%s names a blog publisher path or token prefix (%s)", rel, m)
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, b, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "internal/blog/") || strings.HasPrefix(rel, "internal/blogtext/") {
			for _, im := range f.Imports {
				v := strings.Trim(im.Path.Value, `"`)
				// samehost.go only parses URLs (net/url, no net/http).
				ok := (rel == ganderFile && (v == "net/http" || v == "net/url")) || (rel == hookFile && (v == "net/http" || v == "net/url")) || (rel == "internal/blog/samehost.go" && v == "net/url") || (strings.HasPrefix(rel, hookFakeDir) && v == "net/http") || (strings.HasPrefix(rel, fakeDir) && v == "net/http")
				if !ok && (v == "net" || strings.HasPrefix(v, "net/") || strings.Contains(v, "xauth")) {
					t.Errorf("%s imports %s; only %s and %s call a blog host", rel, v, ganderFile, hookFile)
				}
			}
		}
		if apiBlogPath.MatchString(src) && rel != ganderFile && !strings.HasPrefix(rel, fakeDir) {
			t.Errorf("%s names the Publisher API path; only %s and the fake may", rel, ganderFile)
		}
		if strings.HasPrefix(rel, fakeDir) || strings.HasPrefix(rel, hookFakeDir) {
			if m := regexp.MustCompile(`NewRequest|\.Do\(|http\.(Get|Post|Head|PostForm)\(|http\.Client|DefaultClient|Dial`).FindString(src); m != "" {
				t.Errorf("%s makes an outbound request (%s); the fake only serves", rel, m)
			}
		}
		if adapter[rel] {
			if rel == ganderFile {
				seenGander = true
			} else {
				seenHook = true
			}
			if n := strings.Count(src, "http.NewRequestWithContext("); n != 1 || strings.Count(src, "c.Do(req)") != 1 {
				t.Errorf("%s must build one request and send it once through the injected client", rel)
			}
			if m := regexp.MustCompile(`http\.Client\{|http\.Transport|DefaultClient|DefaultTransport|http\.(Get|Post|Head|PostForm)\(|Proxy|InsecureSkipVerify|TLSClientConfig|net\.Dial|"DELETE"|MethodDelete|MethodPatch`).FindString(src); m != "" {
				t.Errorf("%s uses %s; it must only use the client it is given", rel, m)
			}
		}
		if rel == hookFile && regexp.MustCompile(`MethodPut|"PUT"`).MatchString(src) {
			t.Errorf("%s may only GET and POST", rel)
		}
		if rel == "cmd/xpost/main.go" {
			if strings.Count(src, "ganderPub.HTTP = ") != 1 || !strings.Contains(src, "ganderPub.HTTP = srv.BlogHTTP\n") {
				t.Errorf("%s must give the gander adapter the server's guarded client (srv.BlogHTTP) and nothing else", rel)
			}
			if strings.Count(src, "hookPub.HTTP = ") != 1 || !strings.Contains(src, "hookPub.HTTP = srv.BlogHTTP\n") {
				t.Errorf("%s must give the webhook adapter the server's guarded client (srv.BlogHTTP) and nothing else", rel)
			}
			// The import gate (XPOST_BLOG_IMPORT): the import reads only
			// through its own adapter instances, which only ever get the
			// import-only client, and never through the publishing ones.
			for _, f := range []string{"ganderImport", "hookImport"} {
				if strings.Count(src, f+".HTTP = ") != 1 || !strings.Contains(src, f+".HTTP = srv.BlogImportHTTP\n") {
					t.Errorf("%s must give %s the import-only client (srv.BlogImportHTTP) and nothing else", rel, f)
				}
			}
			if strings.Count(src, "blogImporter(") != 2 || !strings.Contains(src, "blogImporter(imp, w.ganderImport, w.hookImport)") ||
				!strings.Contains(src, "blogPublishers(mode, live, w.ganderPub, w.hookPub, logw)") || strings.Count(src, "ImportAllow: importAllowed") != 1 ||
				strings.Count(src, "os.Getenv(EnvBlogImport)") != 1 || strings.Contains(src, "ganderImport, w.hookPub") || strings.Contains(src, "w.ganderPub, w.hookImport") {
				t.Errorf("%s must wire the import from XPOST_BLOG_IMPORT alone, through the import's own readers", rel)
			}
		}
		if rel == "internal/web/hookdial.go" {
			if !strings.Contains(src, "c.Transport = importOnly{next: c.Transport, allow: s.cfg.ImportAllow}") || !strings.Contains(src, "if t.allow == nil || !t.allow(r) {") {
				t.Errorf("%s: BlogImportHTTP must refuse every request ImportAllow does not approve", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenGander || !seenHook {
		t.Fatalf("%s or %s not found; update this test", ganderFile, hookFile)
	}
	// The only Publishers (types with a Stage method) are FakePublisher and
	// Methods in package blog, and Publisher in package gander.
	allowed := map[string]bool{"blog.FakePublisher": true, "blog.Methods": true, "gander.Publisher": true}
	for _, dir := range []string{".", "gander", filepath.Join("gander", "gfake"), "hookpub", filepath.Join("hookpub", "hfake")} {
		pkgs, err := parser.ParseDir(token.NewFileSet(), dir, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
		if err != nil {
			t.Fatal(err)
		}
		for name, pkg := range pkgs {
			for _, f := range pkg.Files {
				for _, decl := range f.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv == nil || fn.Name.Name != "Stage" {
						continue
					}
					recv := fn.Recv.List[0].Type
					if st, ok := recv.(*ast.StarExpr); ok {
						recv = st.X
					}
					if id, ok := recv.(*ast.Ident); !ok || !allowed[name+"."+id.Name] {
						t.Errorf("a Publisher other than FakePublisher, Methods and gander.Publisher (%s.%v)", name, recv)
					}
				}
			}
		}
	}
}
