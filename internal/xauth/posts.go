package xauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/poster"
	"github.com/scott/xpost/internal/store"
)

// XPoster publishes through X's create-post endpoint (Config.PostURL) and
// uploads images through X's media endpoints (Config.MediaURL,
// Config.MediaMetaURL) with the account's OAuth 2.0 user token. It is the
// only code that creates posts or uploads media, and cmd/xpost hands it to
// the dispatcher only when XPOST_SCHEDULER=on. It never logs or returns a
// token, post text or image bytes.
type XPoster struct {
	M *Manager
}

// Ready reports whether account has a usable X connection.
func (p *XPoster) Ready(ctx context.Context, account string) error {
	c, err := p.M.Store.XConnection(ctx, account)
	if err != nil {
		return ErrNotConnected
	}
	if c.Status != store.XConnected {
		return ErrReconnect
	}
	return nil
}

var digits = func(s string) bool { return s != "" && len(s) <= 30 && strings.Trim(s, "0123456789") == "" }

// Post creates one post, as a reply to r.ReplyTo when set.
func (p *XPoster) Post(ctx context.Context, r poster.Request) (poster.Result, error) {
	if len(r.MediaIDs) > 4 {
		return poster.Result{}, &poster.Error{Kind: poster.Refused, Msg: "more than 4 images"}
	}
	for _, id := range r.MediaIDs {
		if !digits(id) {
			return poster.Result{}, &poster.Error{Kind: poster.Refused, Msg: "invalid media id"}
		}
	}
	if r.ReplyTo != "" && !digits(r.ReplyTo) {
		return poster.Result{}, &poster.Error{Kind: poster.Refused, Msg: "invalid reply target"}
	}
	conn, err := p.M.Store.XConnection(ctx, r.Account)
	if err != nil {
		return poster.Result{}, &poster.Error{Kind: poster.NotSent, Msg: ErrNotConnected.Error()}
	}
	var id string
	err = p.M.Do(ctx, r.Account, func(access string) error {
		var cerr error
		id, cerr = p.M.Client.createPost(ctx, access, r.Text, r.ReplyTo, r.MediaIDs)
		return cerr
	})
	if err != nil {
		return poster.Result{}, classify(err)
	}
	return poster.Result{PostID: id, URL: p.M.Client.Cfg.StatusURL(conn.XUsername, id)}, nil
}

// classify maps errors from Manager.Do into poster kinds.
func classify(err error) error {
	var pe *poster.Error
	if errors.As(err, &pe) {
		return pe
	}
	var xe *Error
	switch {
	case errors.Is(err, ErrNotConnected), errors.Is(err, ErrReconnect), errors.Is(err, ErrNotConfigured):
		return &poster.Error{Kind: poster.NotSent, Msg: err.Error()}
	case errors.As(err, &xe) && xCallOps[xe.Op] && xe.Status == http.StatusUnauthorized:
		// Still 401 after a refresh: X refused it, so nothing was done.
		return &poster.Error{Kind: poster.Refused, Status: xe.Status, Msg: "X rejected the access token"}
	case errors.As(err, &xe):
		// A token refresh failed before any post was sent.
		return &poster.Error{Kind: poster.NotSent, Status: xe.Status, Msg: "token refresh failed"}
	}
	return &poster.Error{Kind: poster.NotSent, Msg: "token refresh failed"}
}

// xCallOps are the Error ops of calls that act on X (a 401 is retried once
// by Manager.Do after a refresh).
var xCallOps = map[string]bool{"post": true, "media": true, "media status": true, "media alt": true}

type createReq struct {
	Text  string     `json:"text"`
	Reply *replySpec `json:"reply,omitempty"`
	Media *mediaSpec `json:"media,omitempty"`
}

type mediaSpec struct {
	MediaIDs []string `json:"media_ids"`
}

type replySpec struct {
	InReplyTo string `json:"in_reply_to_tweet_id"`
}

// createPost sends one create-post request. A 401 comes back as *Error so
// Manager.Do refreshes and retries once (a 401 means nothing was created).
// Every other failure is a *poster.Error saying whether X may have posted.
func (c *Client) createPost(ctx context.Context, access, text, replyTo string, mediaIDs []string) (string, error) {
	body := createReq{Text: text}
	if replyTo != "" {
		body.Reply = &replySpec{InReplyTo: replyTo}
	}
	if len(mediaIDs) > 0 {
		body.Media = &mediaSpec{MediaIDs: mediaIDs}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.Cfg.PostURL, strings.NewReader(string(b)))
	if err != nil {
		return "", &poster.Error{Kind: poster.NotSent, Msg: "could not build the request"}
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", transportError(err)
	}
	defer resp.Body.Close()
	rb, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", &Error{Op: "post", Status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &poster.Error{Kind: poster.RateLimited, Status: resp.StatusCode, Msg: "rate limited by X", RetryAt: rateLimitReset(resp.Header, c.now())}
	case resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusRequestTimeout:
		// X may have created the post before failing: unknown.
		return "", &poster.Error{Kind: poster.Ambiguous, Status: resp.StatusCode, Msg: "X server error; the post may or may not exist"}
	case resp.StatusCode/100 != 2:
		return "", &poster.Error{Kind: poster.Refused, Status: resp.StatusCode, Msg: problem(rb)}
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if rerr != nil || json.Unmarshal(rb, &out) != nil || !digits(out.Data.ID) {
		return "", &poster.Error{Kind: poster.Ambiguous, Status: resp.StatusCode, Msg: "X accepted the request but the reply was unreadable"}
	}
	return out.Data.ID, nil
}

// ---- Media ----

// Upload sends one image to X's media upload (simple, one request: images
// are at most 5 MB). GIFs go up as tweet_gif, which X may process
// asynchronously; everything else as tweet_image.
func (p *XPoster) Upload(ctx context.Context, u poster.Upload) (poster.Uploaded, error) {
	cat := "tweet_image"
	switch u.Mime {
	case "image/gif":
		cat = "tweet_gif"
	case "image/png", "image/jpeg", "image/webp":
	default:
		return poster.Uploaded{}, &poster.Error{Kind: poster.Refused, Msg: "unsupported image type"}
	}
	if len(u.Data) == 0 || len(u.Data) > store.MaxMediaBytes {
		return poster.Uploaded{}, &poster.Error{Kind: poster.Refused, Msg: "image is empty or over 5 MB"}
	}
	var out poster.Uploaded
	err := p.M.Do(ctx, u.Account, func(access string) error {
		var cerr error
		out, cerr = p.M.Client.uploadMedia(ctx, access, u.Mime, cat, u.Data)
		return cerr
	})
	if err != nil {
		return poster.Uploaded{}, classify(err)
	}
	return out, nil
}

// MediaStatus checks an upload X is still processing.
func (p *XPoster) MediaStatus(ctx context.Context, account, mediaID string) (poster.Uploaded, error) {
	if !digits(mediaID) {
		return poster.Uploaded{}, &poster.Error{Kind: poster.Refused, Msg: "invalid media id"}
	}
	var out poster.Uploaded
	err := p.M.Do(ctx, account, func(access string) error {
		var cerr error
		out, cerr = p.M.Client.mediaStatus(ctx, access, mediaID)
		return cerr
	})
	if err != nil {
		return poster.Uploaded{}, classify(err)
	}
	return out, nil
}

// SetAlt sets an uploaded image's alt text (X allows 1000 characters).
func (p *XPoster) SetAlt(ctx context.Context, account, mediaID, alt string) error {
	if !digits(mediaID) {
		return &poster.Error{Kind: poster.Refused, Msg: "invalid media id"}
	}
	if len([]rune(alt)) > store.MaxAltLen {
		return &poster.Error{Kind: poster.Refused, Msg: "alt text is over 1000 characters"}
	}
	err := p.M.Do(ctx, account, func(access string) error {
		return p.M.Client.setAlt(ctx, access, mediaID, alt)
	})
	if err != nil {
		return classify(err)
	}
	return nil
}

// send makes one call to X and sorts the failure: a 401 comes back as
// *Error (op) so Manager.Do refreshes and retries once; 429 is rate
// limited; 5xx and 408 are ambiguous (what may have happened); other
// non-2xx are refusals.
func (c *Client) send(req *http.Request, access, op, what string) ([]byte, int, error) {
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, transportErrorFor(err, what)
	}
	defer resp.Body.Close()
	rb, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, resp.StatusCode, &Error{Op: op, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, resp.StatusCode, &poster.Error{Kind: poster.RateLimited, Status: resp.StatusCode, Msg: "rate limited by X", RetryAt: rateLimitReset(resp.Header, c.now())}
	case resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusRequestTimeout:
		return nil, resp.StatusCode, &poster.Error{Kind: poster.Ambiguous, Status: resp.StatusCode, Msg: "X server error; the " + what + " may or may not have gone through"}
	case resp.StatusCode/100 != 2:
		return nil, resp.StatusCode, &poster.Error{Kind: poster.Refused, Status: resp.StatusCode, Msg: problemFor(rb, what)}
	}
	if rerr != nil {
		return nil, resp.StatusCode, &poster.Error{Kind: poster.Ambiguous, Status: resp.StatusCode, Msg: "X accepted the " + what + " but the reply was unreadable"}
	}
	return rb, resp.StatusCode, nil
}

// mediaData is X's media upload / status reply.
type mediaData struct {
	Data struct {
		ID               string `json:"id"`
		ExpiresAfterSecs int64  `json:"expires_after_secs"`
		ProcessingInfo   *struct {
			State          string `json:"state"`
			CheckAfterSecs int64  `json:"check_after_secs"`
		} `json:"processing_info"`
	} `json:"data"`
}

func (c *Client) uploaded(rb []byte, status int, what string) (poster.Uploaded, error) {
	var m mediaData
	if json.Unmarshal(rb, &m) != nil || !digits(m.Data.ID) {
		return poster.Uploaded{}, &poster.Error{Kind: poster.Ambiguous, Status: status, Msg: "X accepted the " + what + " but the reply was unreadable"}
	}
	now := c.now()
	out := poster.Uploaded{MediaID: m.Data.ID}
	if s := m.Data.ExpiresAfterSecs; s > 0 && s < 30*24*3600 {
		out.ExpiresAt = now.Add(time.Duration(s) * time.Second)
	}
	if pi := m.Data.ProcessingInfo; pi != nil {
		switch pi.State {
		case "", "succeeded":
		case "failed":
			out.Failed = "X reported processing failed"
		default: // pending, in_progress
			out.Processing = true
			wait := time.Duration(min(max(pi.CheckAfterSecs, 1), 600)) * time.Second
			out.CheckAt = now.Add(wait)
		}
	}
	return out, nil
}

// uploadMedia POSTs multipart {media, media_category} to Config.MediaURL.
func (c *Client) uploadMedia(ctx context.Context, access, mime, category string, data []byte) (poster.Uploaded, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("media_category", category)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="media"; filename="image"`)
	h.Set("Content-Type", mime)
	pw, _ := w.CreatePart(h)
	pw.Write(data)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", c.Cfg.MediaURL, &buf)
	if err != nil {
		return poster.Uploaded{}, &poster.Error{Kind: poster.NotSent, Msg: "could not build the request"}
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	rb, status, err := c.send(req, access, "media", "upload")
	if err != nil {
		return poster.Uploaded{}, err
	}
	return c.uploaded(rb, status, "upload")
}

// mediaStatus GETs Config.MediaURL?command=STATUS&media_id=.
func (c *Client) mediaStatus(ctx context.Context, access, mediaID string) (poster.Uploaded, error) {
	q := url.Values{"command": {"STATUS"}, "media_id": {mediaID}}
	req, err := http.NewRequestWithContext(ctx, "GET", c.Cfg.MediaURL+"?"+q.Encode(), nil)
	if err != nil {
		return poster.Uploaded{}, &poster.Error{Kind: poster.NotSent, Msg: "could not build the request"}
	}
	rb, status, err := c.send(req, access, "media status", "status check")
	if err != nil {
		return poster.Uploaded{}, err
	}
	return c.uploaded(rb, status, "status check")
}

// setAlt POSTs {id, metadata: {alt_text: {text}}} to Config.MediaMetaURL.
func (c *Client) setAlt(ctx context.Context, access, mediaID, alt string) error {
	var body struct {
		ID       string `json:"id"`
		Metadata struct {
			AltText struct {
				Text string `json:"text"`
			} `json:"alt_text"`
		} `json:"metadata"`
	}
	body.ID = mediaID
	body.Metadata.AltText.Text = alt
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.Cfg.MediaMetaURL, bytes.NewReader(b))
	if err != nil {
		return &poster.Error{Kind: poster.NotSent, Msg: "could not build the request"}
	}
	req.Header.Set("Content-Type", "application/json")
	_, _, err = c.send(req, access, "media alt", "alt text")
	return err
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// transportErrorFor is transportError for a call other than create-post.
func transportErrorFor(err error, what string) error {
	pe := transportError(err).(*poster.Error)
	if pe.Kind == poster.Ambiguous {
		pe.Msg = strings.Replace(pe.Msg, "the post may or may not exist", "the "+what+" may or may not have gone through", 1)
	}
	return pe
}

// transportError: a request that never connected was not sent; anything
// after that (timeouts, resets, EOF) may have reached X.
func transportError(err error) error {
	var op *net.OpError
	var dns *net.DNSError
	if errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial") {
		return &poster.Error{Kind: poster.NotSent, Msg: "could not connect to X"}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &poster.Error{Kind: poster.Ambiguous, Msg: "timed out waiting for X; the post may or may not exist"}
	}
	return &poster.Error{Kind: poster.Ambiguous, Msg: "connection to X failed mid-request; the post may or may not exist"}
}

// rateLimitReset reads X's reset headers (epoch seconds). When the 24-hour
// user limit is exhausted its reset wins; otherwise x-rate-limit-reset;
// Retry-After (seconds) is a fallback. Zero means unknown.
func rateLimitReset(h http.Header, now time.Time) time.Time {
	epoch := func(k string) time.Time {
		v, err := strconv.ParseInt(strings.TrimSpace(h.Get(k)), 10, 64)
		if err != nil || v <= 0 {
			return time.Time{}
		}
		return time.Unix(v, 0)
	}
	var t time.Time
	if h.Get("x-user-limit-24hour-remaining") == "0" {
		t = epoch("x-user-limit-24hour-reset")
	}
	if r := epoch("x-rate-limit-reset"); r.After(t) {
		t = r
	}
	if t.IsZero() {
		if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s > 0 {
			t = now.Add(time.Duration(s) * time.Second)
		}
	}
	return t
}

// problemFor is problem for a call other than create-post.
func problemFor(b []byte, what string) string {
	msg := problem(b)
	if msg == "X refused the post" {
		return "X refused the " + what
	}
	return msg
}

// problem keeps a short, printable summary of an X error body (its title
// and detail). X's error bodies carry no credentials.
func problem(b []byte) string {
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	json.Unmarshal(b, &p)
	msg := strings.TrimSpace(p.Detail)
	if msg == "" {
		msg = strings.TrimSpace(p.Title)
	}
	if msg == "" {
		return "X refused the post"
	}
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return fmt.Sprintf("X refused it: %s", msg)
}
