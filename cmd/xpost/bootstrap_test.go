package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// .env.example (the template deploy/bootstrap.sh copies to /etc/xpost/env)
// carries key names only: the three lines bootstrap fills are empty, every
// other key is commented out with no value or a harmless default, every
// key is one xpost reads, posting is never switched on, and blogs are
// listed as an off option.
func TestEnvExample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	src := readSources(t)
	line := regexp.MustCompile(`^(# )?(XPOST_[A-Z0-9_]+)=(.*)$`)
	active := map[string]bool{}
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			if strings.HasPrefix(l, "XPOST_") || (l != "" && !strings.HasPrefix(l, "#")) {
				t.Errorf("unexpected line %q", l)
			}
			continue
		}
		k, v := m[2], m[3]
		seen[k] = true
		if !strings.Contains(src, `"`+k+`"`) {
			t.Errorf("%s is not a key xpost reads", k)
		}
		if m[1] == "" {
			active[k] = true
			if v != "" {
				t.Errorf("active line %s has a value", k)
			}
		} else if v != "" && v != "0" && !strings.HasPrefix(v, "/etc/xpost/") {
			t.Errorf("%s: example value %q is not a harmless default", k, v)
		}
	}
	for _, k := range []string{"XPOST_BASE_URL", "XPOST_DB", "XPOST_KEY_FILE"} {
		if !active[k] {
			t.Errorf("%s must be an empty active line for bootstrap to fill", k)
		}
	}
	if len(active) != 3 {
		t.Errorf("active lines %v: only the three bootstrap fills", active)
	}
	for _, k := range []string{"XPOST_X_CLIENT_ID", "XPOST_X_CLIENT_SECRET_FILE", "XPOST_SCHEDULER", "XPOST_COST_POST_USD", "XPOST_TZ", "XPOST_BLOG_LIVE", "XPOST_BLOG_IMPORT"} {
		if !seen[k] {
			t.Errorf("%s missing", k)
		}
	}
	if !strings.Contains(string(b), "\n# XPOST_BLOGS_ENABLED=0\n") {
		t.Error("XPOST_BLOGS_ENABLED must be listed commented out, off")
	}
}

func readSources(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	root := filepath.Join("..", "..")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			b, _ := os.ReadFile(p)
			sb.Write(b)
		}
		return nil
	})
	return sb.String()
}

// deploy/bootstrap.sh: valid bash (and shellcheck-clean when it is
// installed), refuses xtrace, never turns posting on, writes the env file
// mode 600, and never names a host other than the release and Caddy repos.
func TestBootstrapScript(t *testing.T) {
	p := filepath.Join("..", "..", "deploy", "bootstrap.sh")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v %s", err, out)
	}
	if sc, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command(sc, "-S", "warning", p).CombinedOutput(); err != nil {
			t.Errorf("shellcheck: %s", out)
		}
	}
	for _, want := range []string{"refusing to run with xtrace", "set -euo pipefail", `install -m 0600 -o root -g root "$tmp/env"`, "never changes a value", "checksum mismatch", "caddy validate", "xpost.env.example"} {
		if !strings.Contains(s, want) {
			t.Errorf("bootstrap.sh lacks %q", want)
		}
	}
	if regexp.MustCompile(`XPOST_SCHEDULER=|-fake-poster|XPOST_BLOGS_ENABLED=1`).MatchString(s) {
		t.Error("bootstrap.sh turns posting or blogs on")
	}
	for _, h := range regexp.MustCompile(`https://([a-z0-9.-]+)`).FindAllStringSubmatch(s, -1) {
		switch h[1] {
		case "github.com", "dl.cloudsmith.io", "post.example.com", "stats.example.test":
		default:
			if !strings.HasPrefix(h[1], "$") {
				t.Errorf("bootstrap.sh names host %s", h[1])
			}
		}
	}
}
