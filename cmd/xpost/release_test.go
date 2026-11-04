package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/build-release.sh publishes every file deploy/bootstrap.sh
// downloads from a release (the linux binaries, xpost.env.example and
// SHA256SUMS), lists them all in SHA256SUMS, builds linux and darwin for
// amd64 and arm64, stamps the version with ldflags, and stays CGO-free.
func TestReleaseScript(t *testing.T) {
	p := filepath.Join("..", "..", "scripts", "build-release.sh")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command("shellcheck", p).CombinedOutput(); err != nil {
			t.Errorf("shellcheck: %v\n%s", err, out)
		}
	}
	for _, want := range []string{
		"set -euo pipefail", "CGO_ENABLED=0", "-X main.version=${version}",
		"for os in linux darwin", "for arch in amd64 arm64",
		`xpost-$os-$arch`, "xpost.env.example", "sha256sum", "SHA256SUMS",
		"cp -p .env.example", "assets+=(xpost.env.example",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("build-release.sh lacks %q", want)
		}
	}
	bs, err := os.ReadFile(filepath.Join("..", "..", "deploy", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"xpost-linux-", "xpost.env.example", "SHA256SUMS"} {
		if !strings.Contains(string(bs), name) {
			t.Errorf("bootstrap.sh no longer fetches %s; update the release assets", name)
		}
	}
}
