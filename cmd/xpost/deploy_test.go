package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// code drops comment lines (# ...), keeping what the shell, systemd or
// Caddy actually reads.
func code(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// Nothing under deploy/ turns on the fake poster, binds off loopback, or
// writes the client secret into the env file.
func TestDeployFilesStaySafe(t *testing.T) {
	root := filepath.Join("..", "..", "deploy")
	if _, err := os.Stat(filepath.Join(root, "bootstrap.sh")); err != nil {
		t.Fatalf("deploy/bootstrap.sh: %v", err)
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := code(string(b))
		rel, _ := filepath.Rel(root, p)
		if rel != "README.md" && strings.Contains(s, "-fake-poster") {
			t.Errorf("%s mentions -fake-poster", rel)
		}
		if m := regexp.MustCompile(`-addr\s+(\S+)`).FindAllStringSubmatch(s, -1); m != nil {
			for _, a := range m {
				if !strings.HasPrefix(a[1], "127.0.0.1:") {
					t.Errorf("%s: -addr %s is not loopback", rel, a[1])
				}
			}
		}
		if regexp.MustCompile(`echo\s+"XPOST_X_CLIENT_SECRET=`).MatchString(s) {
			t.Errorf("%s writes the client secret into an env file", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Merging and deploying must not start posting: nothing under deploy/
// sets XPOST_SCHEDULER (absent means off) or the cost rates.
func TestDeployKeepsSchedulerOff(t *testing.T) {
	root := filepath.Join("..", "..", "deploy")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		s := code(string(b))
		if rel == "README.md" {
			s = string(b)
			if regexp.MustCompile(`XPOST_SCHEDULER=on`).MatchString(s) && !strings.Contains(s, "deliberate") {
				t.Errorf("README suggests XPOST_SCHEDULER=on without saying it is a deliberate later step")
			}
			return nil
		}
		for _, k := range []string{"XPOST_SCHEDULER", "XPOST_COST_", "fake-poster"} {
			if strings.Contains(s, k) {
				t.Errorf("%s sets or mentions %s", rel, k)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
