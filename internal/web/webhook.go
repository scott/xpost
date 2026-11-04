package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Rewrite webhooks: xpost POSTs JSON to the agent's URL, signed with the
// agent's sender key:
//
//	X-Xpost-Event:     rewrite.requested | rewrite.test
//	X-Xpost-Timestamp: unix seconds
//	X-Xpost-Signature: sha256=<hex HMAC-SHA256(key, timestamp + "." + body)>
//
// The key itself is never sent. If the owner saved an Authorization header
// value for the agent (receivers such as webhook routines require one), it
// is sent as "Authorization: <value>" on every event; otherwise no
// Authorization header is sent. Any 2xx means the agent accepted it.
// Redirects are not followed. Neither the key, the payload nor the URL is
// logged.

// webhookTimeout bounds one delivery.
const webhookTimeout = 10 * time.Second

// Webhook event names.
const (
	hookRewrite = "rewrite.requested"
	hookTest    = "rewrite.test"
)

// webhookPart is one part in a rewrite request: its text and how many
// images it carries (images can't change, so only the count is sent).
type webhookPart struct {
	Text   string `json:"text"`
	Images int    `json:"images"`
}

type webhookCallback struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Scope  string `json:"scope"`
	Body   string `json:"body"`
}

// webhookPost is a blog post in a rewrite request. The slug and cover are
// sent for context only: a rewrite can't change them.
type webhookPost struct {
	Site        string   `json:"site"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Markdown    string   `json:"markdown"`
	Cover       bool     `json:"cover"`
}

// webhookReply is the reply half of a rewrite request: the post the reply
// answers, as the agent sent it. A reply stays one part with no images.
type webhookReply struct {
	InReplyTo     string      `json:"in_reply_to"`
	Account       string      `json:"account"`
	Original      apiOriginal `json:"original"`
	ReplySettings string      `json:"reply_settings,omitempty"`
}

// webhookRewrite is a rewrite request. An X draft carries parts; a blog
// post carries kind "blog" and post instead; a reply carries kind "reply",
// its one part and reply (X requests are unchanged).
type webhookRewrite struct {
	Type      string          `json:"type"`
	Kind      string          `json:"kind,omitempty"`
	RequestID int64           `json:"request_id"`
	DraftID   int64           `json:"draft_id"`
	Parts     []webhookPart   `json:"parts,omitempty"`
	Post      *webhookPost    `json:"post,omitempty"`
	Reply     *webhookReply   `json:"reply,omitempty"`
	Note      string          `json:"note"`
	ExpiresAt string          `json:"expires_at"`
	Callback  webhookCallback `json:"callback"`
}

type webhookTestBody struct {
	Type string `json:"type"`
	At   string `json:"at"`
}

func (s *Server) rewritePayload(ctx context.Context, d *store.Draft, req *store.RewriteRequest) webhookRewrite {
	p := webhookRewrite{Type: hookRewrite, RequestID: req.ID, DraftID: d.ID, Note: req.Note, ExpiresAt: rfc(req.ExpiresAt),
		Callback: webhookCallback{Method: "POST", URL: s.cfg.BaseURL + "/api/v1/drafts/" + strconv.FormatInt(d.ID, 10) + "/revise", Scope: store.ScopeDraftsRevise,
			Body: `{"request_id": ` + strconv.FormatInt(req.ID, 10) + `, "parts": [{"text": "..."}]}`}}
	if d.IsBlog() && d.Post != nil {
		bp := d.Post
		tags := bp.Tags
		if tags == nil {
			tags = []string{}
		}
		site := d.Account
		if b, err := s.st.BlogForAccount(ctx, d.Account); err == nil {
			site = b.Name
		}
		p.Kind = store.KindBlog
		p.Post = &webhookPost{Site: site, Slug: bp.Slug, Title: bp.Title, Description: bp.Description, Tags: tags, Markdown: bp.Markdown, Cover: bp.Cover != nil}
		p.Callback.Body = `{"request_id": ` + strconv.FormatInt(req.ID, 10) + `, "post": {"title": "...", "description": "...", "tags": ["..."], "markdown": "..."}}`
		return p
	}
	for _, pt := range d.Parts {
		p.Parts = append(p.Parts, webhookPart{Text: pt.Text, Images: len(pt.Media)})
	}
	if rp := d.Reply; rp != nil {
		p.Kind = store.KindReply
		p.Reply = &webhookReply{InReplyTo: rp.InReplyTo, Account: d.Account, ReplySettings: rp.ReplySettings,
			Original: apiOriginal{AuthorName: rp.AuthorName, AuthorHandle: rp.AuthorHandle, Text: rp.Text, URL: rp.URL, Unavailable: !rp.Available}}
		if rp.CreatedAt != nil {
			p.Reply.Original.CreatedAt = rfc(*rp.CreatedAt)
		}
	}
	return p
}

// signWebhook is the X-Xpost-Signature value for body at ts.
func signWebhook(key string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// checkWebhookURL accepts https URLs. Plain http is accepted only for a
// loopback host while xpost itself runs on a loopback base URL (local
// development and the screenshot fake). An IP literal in a blocked range
// is refused here already; hostnames are checked again at dial time
// (hookdial.go), where their real addresses are known.
var errPrivateURL = errors.New("That address is private or reserved. Use a public https URL.")

func (s *Server) checkWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("Enter a full URL such as https://agents.example.com/xpost-rewrite.")
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); (err == nil && !s.destAllowed(ip)) || (host == "localhost" && !s.loopbackBase()) {
		return errPrivateURL
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(host) && s.loopbackBase() {
			return nil
		}
	}
	return errors.New("The rewrite webhook URL must start with https://.")
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) loopbackBase() bool {
	u, err := url.Parse(s.cfg.BaseURL)
	return err == nil && u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// hookResult is one delivery's outcome for the owner: OK, and a short
// detail ("HTTP 500", "timeout", "no connection").
type hookResult struct {
	OK      bool
	Refused bool // the destination is in a blocked range
	Detail  string
	Took    time.Duration
}

// deliver POSTs body to the token's webhook. It logs only the token id,
// the event and the outcome.
func (s *Server) deliver(ctx context.Context, tokenID int64, eventName string, v any) hookResult {
	hookURL, key, err := s.st.WebhookSecret(ctx, tokenID)
	if err != nil {
		return hookResult{Detail: "not set"}
	}
	auth, err := s.st.WebhookAuth(ctx, tokenID)
	if err != nil {
		return hookResult{Detail: "could not open the Authorization value"}
	}
	if err := s.checkWebhookURL(hookURL); err != nil {
		if errors.Is(err, errPrivateURL) {
			return hookResult{Refused: true, Detail: errDestRefused.Error()}
		}
		return hookResult{Detail: "URL not allowed"}
	}
	body, err := json.Marshal(v)
	if err != nil {
		return hookResult{Detail: "could not build the request"}
	}
	ts := s.st.Now().Unix()
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hookURL, bytes.NewReader(body))
	if err != nil {
		return hookResult{Detail: "URL not allowed"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "xpost/"+s.cfg.Version)
	req.Header.Set("X-Xpost-Event", eventName)
	req.Header.Set("X-Xpost-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Xpost-Signature", signWebhook(key, ts, body))
	if auth != "" {
		req.Header.Set("Authorization", auth) // as saved; never logged
	}
	start := time.Now()
	resp, err := s.webhookClient().Do(req)
	took := time.Since(start)
	res := hookResult{Took: took}
	if err != nil {
		var ne net.Error
		var de *net.DNSError
		switch {
		case errors.Is(err, errDestRefused):
			res.Refused, res.Detail = true, errDestRefused.Error()
		case errors.As(err, &de):
			res.Detail = "host not found"
		case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
			res.Detail = "timeout"
		default:
			res.Detail = "no connection"
		}
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
		res.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	s.log.Printf("rewrite webhook: token %d %s: %s in %d ms", tokenID, eventName, res.Detail, took.Milliseconds())
	return res
}
