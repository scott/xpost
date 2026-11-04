package web

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

func gifBytes() []byte {
	var b bytes.Buffer
	gif.Encode(&b, image.NewPaletted(image.Rect(0, 0, 8, 8), []color.Color{color.White, color.Black}), nil)
	return b.Bytes()
}

func ref(t *testing.T, r resp) string {
	t.Helper()
	var out struct {
		Ref string `json:"media_ref"`
	}
	json.Unmarshal([]byte(r.body), &out)
	if r.status != http.StatusCreated || out.Ref == "" {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}
	return out.Ref
}

func imageDraft(e *env, sec string, refs ...string) resp {
	var media []any
	for _, r := range refs {
		media = append(media, map[string]string{"ref": r, "alt": "Example alt"})
	}
	return e.api("POST", "/api/v1/drafts", sec, map[string]any{"account": sm, "proposed_at": testNow.Add(2 * time.Hour).Format(time.RFC3339),
		"parts": []map[string]any{{"text": "Example with images", "media": media}}})
}

// X allows 4 photos or 1 GIF; the API says so instead of failing at post
// time. Damaged images are refused at upload.
func TestAPIImageRules(t *testing.T) {
	e := newEnv(t, nil)
	_, a := e.token("sample-agent")
	p1, p2 := ref(t, upload(e, a, pngBytes(t))), ref(t, upload(e, a, pngBytes(t)))
	g := ref(t, upload(e, a, gifBytes()))
	if r := imageDraft(e, a, p1, g); r.status != http.StatusBadRequest || !strings.Contains(r.body, "GIF") {
		t.Fatalf("GIF with a photo: %d %s", r.status, r.body)
	}
	if r := imageDraft(e, a, p1, p2, p1, p2, p1); r.status != http.StatusBadRequest {
		t.Fatalf("5 images: %d", r.status)
	}
	if r := imageDraft(e, a, g); r.status != http.StatusCreated {
		t.Fatalf("GIF alone: %d %s", r.status, r.body)
	}
	if r := imageDraft(e, a, "med_../../../../etc/passwd"); r.status != http.StatusBadRequest {
		t.Fatalf("traversal ref: %d", r.status)
	}
	if r := upload(e, a, pngBytes(t)[:30]); r.status != http.StatusBadRequest {
		t.Fatalf("damaged png: %d", r.status)
	}
	if r := upload(e, a, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)); r.status != http.StatusBadRequest {
		t.Fatalf("svg: %d", r.status)
	}
	// A token without drafts:write can't upload.
	_, ro := e.token("sample-reader", store.ScopeLogRead)
	if r := upload(e, ro, pngBytes(t)); r.status != http.StatusForbidden {
		t.Fatalf("log-only token uploaded: %d", r.status)
	}
}

// Images are served to the owner's session only, with headers that stop
// them from running as anything but an image.
func TestMediaServedToOwnerOnly(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, a := e.token("sample-agent")
	r := ref(t, upload(e, a, pngBytes(t)))
	anon := e.client()
	req, _ := http.NewRequest("GET", e.ts.URL+"/media/"+r, nil)
	req.Header.Set("Authorization", "Bearer "+a)
	if g := e.do(anon.c, req); g.status == http.StatusOK || strings.Contains(g.h.Get("Content-Type"), "image") {
		t.Fatalf("an agent token read /media: %d", g.status)
	}
	if g := anon.get("/media/" + r); g.status == http.StatusOK || strings.Contains(g.h.Get("Content-Type"), "image") {
		t.Fatal("served without a session")
	}
	g := c.get("/media/" + r)
	if g.status != http.StatusOK || g.h.Get("Content-Type") != "image/png" || g.h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(g.h.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(g.h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("%d %v", g.status, g.h)
	}
	for _, bad := range []string{"med_nope", "..%2f..%2fetc%2fpasswd", url.PathEscape(r + "/x"), r + "0"} {
		if g := c.get("/media/" + bad); g.status != http.StatusNotFound {
			t.Errorf("%q: %d", bad, g.status)
		}
	}
}

// The owner's edit form can't put a GIF next to a photo either.
func TestOwnerEditRefusesGIFWithPhoto(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, a := e.token("sample-agent")
	r := imageDraft(e, a, ref(t, upload(e, a, pngBytes(t))))
	id := decode(t, r).ID
	d := e.draft(id)
	_, gid, err := e.st.AddMedia(context.Background(), nil, gifBytes())
	if err != nil {
		t.Fatal(err)
	}
	at := testNow.Add(2 * time.Hour)
	f := editForm("save", "Example with images", at)
	f.Set("media_0", "2")
	f.Set("mid_0_0", strconvI(d.Parts[0].Media[0].ID))
	f.Set("mid_0_1", strconvI(gid))
	resp := c.post(path(id, ""), path(id, ""), e.ownerForm(id, f))
	if resp.status == http.StatusSeeOther || !strings.Contains(resp.body, "GIF") {
		t.Fatalf("saved: %d", resp.status)
	}
	if n := len(e.draft(id).Parts[0].Media); n != 1 {
		t.Fatalf("media %d", n)
	}
}

func imageDraftAlt(e *env, sec, ref, alt, text string) resp {
	return e.api("POST", "/api/v1/drafts", sec, map[string]any{"account": sm, "proposed_at": testNow.Add(2 * time.Hour).Format(time.RFC3339),
		"parts": []map[string]any{{"text": text, "media": []any{map[string]string{"ref": ref, "alt": alt}}}}})
}

// Each media rate shows on the edit page and Log on its own: unset says
// which drafts wait and why; 0 is shown as free; a set rate is counted.
func TestMediaRatesShown(t *testing.T) {
	const upWait, altWait = "The image upload rate is not set, so this draft waits until it is.", "The alt text rate is not set, so this draft waits until it is."
	cases := []struct {
		name                string
		up, alt             int64
		upKnown, altKnown   bool
		withAlt, withoutAlt string // edit page cost for each draft
		logHas, logLacks    []string
	}{
		{"both unset", 0, 0, false, false, "unverified", "unverified",
			[]string{"XPOST_COST_MEDIA_UPLOAD_USD) is not configured", "XPOST_COST_MEDIA_METADATA_USD) is not configured"}, nil},
		{"upload 0, metadata unset", 0, 0, true, false, "unverified", "$1.00",
			[]string{"XPOST_COST_MEDIA_METADATA_USD) is not configured"}, []string{"XPOST_COST_MEDIA_UPLOAD_USD) is not configured"}},
		{"upload unset, metadata 0", 0, 0, false, true, "unverified", "unverified",
			[]string{"XPOST_COST_MEDIA_UPLOAD_USD) is not configured"}, []string{"XPOST_COST_MEDIA_METADATA_USD) is not configured"}},
		{"both set", 250_000, 50_000, true, true, "$1.30", "$1.25",
			nil, []string{"is not configured", "Text-only drafts are not affected."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(cf *Config) { cf.Scheduler = "on" })
			e.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true, UploadMicros: c.up, UploadKnown: c.upKnown, AltMicros: c.alt, AltKnown: c.altKnown}
			e.st.SetSetting(context.Background(), store.SettingCapCents, "2000")
			cl, _ := e.setupOwner()
			_, a := e.token("sample-agent")
			withAlt := decode(t, imageDraftAlt(e, a, ref(t, upload(e, a, pngBytes(t))), "Example alt", "Example with alt")).ID
			noAlt := decode(t, imageDraftAlt(e, a, ref(t, upload(e, a, pngBytes(t))), "", "Example without alt")).ID
			ed := cl.get(path(withAlt, "")).body
			if !strings.Contains(ed, "1 image upload ×") || !strings.Contains(ed, "1 alt text ×") || !strings.Contains(ed, `<b class="t">`+c.withAlt+`</b>`) {
				t.Errorf("with alt: cost %q missing", c.withAlt)
			}
			if strings.Contains(ed, upWait) == c.upKnown || strings.Contains(ed, altWait) == c.altKnown {
				t.Errorf("with alt: wait notes wrong (upload %v, alt %v)", strings.Contains(ed, upWait), strings.Contains(ed, altWait))
			}
			ed = cl.get(path(noAlt, "")).body
			if !strings.Contains(ed, `<b class="t">`+c.withoutAlt+`</b>`) || strings.Contains(ed, "1 alt text ×") || strings.Contains(ed, altWait) || strings.Contains(ed, upWait) == c.upKnown {
				t.Errorf("without alt: %q / notes", c.withoutAlt)
			}
			lg := cl.get("/log").body
			for _, w := range c.logHas {
				if !strings.Contains(lg, w) {
					t.Errorf("log lacks %q", w)
				}
			}
			for _, w := range c.logLacks {
				if strings.Contains(lg, w) {
					t.Errorf("log has %q", w)
				}
			}
			if !strings.Contains(lg, "alt text calls ×") || !strings.Contains(lg, "image uploads ×") {
				t.Error("log lacks the media spend rows")
			}
			st := cl.get("/settings").body
			if strings.Contains(st, "drafts with images wait") == c.upKnown || strings.Contains(st, "drafts with alt text wait") == c.altKnown {
				t.Error("settings hints wrong")
			}
		})
	}
}

// A part holding a GIF offers no Add tile: the GIF must stay alone.
func TestGIFPartHidesAdd(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, a := e.token("sample-agent")
	gid := decode(t, imageDraft(e, a, ref(t, upload(e, a, gifBytes())))).ID
	pid := decode(t, imageDraft(e, a, ref(t, upload(e, a, pngBytes(t))))).ID
	if b := c.get(path(gid, "")).body; strings.Contains(b, `name="add_0"`) {
		t.Error("Add offered next to a GIF")
	}
	if b := c.get(path(pid, "")).body; !strings.Contains(b, `name="add_0"`) {
		t.Error("Add missing next to a photo")
	}
}
