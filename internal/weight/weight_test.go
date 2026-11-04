package weight

import (
	"strings"
	"testing"
)

func TestCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
		urls int
	}{
		{"", 0, 0},
		{"hello", 5, 0},
		{"café", 4, 0},
		{"例文", 4, 0},
		{"✅🚀", 4, 0},
		{"❤️", 2, 0}, // heart + variation selector 16
		{"see https://example.com/a/very/long/path/that/is/long ok", 4 + 23 + 3, 1},
		{"https://example.com.", 23 + 1, 1},
		{"http://localhost/x", 23, 1}, // no dotted TLD: max(own length, 23)
		{"http://localhost/" + strings.Repeat("x", 30), 47, 1},
		{"go to example.com now", 6 + 23 + 4, 1},
		{"v0.4 e.g. 3.14", 14, 0},
	}
	for _, c := range cases {
		got := Count(c.in)
		want := c.want
		if got.Weighted != want || got.URLs != c.urls {
			t.Errorf("Count(%q) = %+v, want %d (%d urls)", c.in, got, want, c.urls)
		}
	}
}

func TestOverAndMockPart3(t *testing.T) {
	if (Result{Weighted: 300}).Over() != 20 || (Result{Weighted: 280}).Over() != 0 {
		t.Fatal("Over")
	}
	if n := Count(strings.Repeat("a", 280)).Weighted; n != Limit {
		t.Fatalf("280 ascii = %d", n)
	}
	if n := Count(strings.Repeat("例", 141)).Weighted; n != 282 {
		t.Fatalf("141 CJK = %d", n)
	}
}

func TestBlogURLCountsAsLink(t *testing.T) {
	cases := map[string]int{
		BlogURL:                      23,
		"New post: " + BlogURL:       33,
		BlogURL + " and " + BlogURL:  51,
		"Read it (" + BlogURL + ")":  33,
		"no placeholder {{blog_url}": 26,
	}
	for in, want := range cases {
		if got := Count(in); got.Weighted != want {
			t.Errorf("Count(%q) = %d, want %d", in, got.Weighted, want)
		}
	}
	if !HasURL("x " + BlogURL) {
		t.Error("a blog link should count as a link")
	}
}
