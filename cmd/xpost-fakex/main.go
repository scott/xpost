// Command xpost-fakex runs the local fake X provider (package xfake: OAuth,
// users/me, create-post and media upload, with injectable faults) for demos and
// screenshots. It never contacts X. Not for production.
package main

import (
	"flag"
	"fmt"
	"github.com/scott/xpost/internal/demo"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/scott/xpost/internal/xauth/xfake"
)

// defaultUsers are the demo data's X handles (see internal/demo).
const defaultUsers = demo.Users

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address (loopback only)")
	users := flag.String("users", defaultUsers, "usernames the fake can sign in as")
	clientID := flag.String("client-id", "example-client-id", "client id the fake expects")
	flag.Parse()
	if !strings.HasPrefix(*addr, "127.0.0.1:") && !strings.HasPrefix(*addr, "localhost:") {
		log.Fatal("xpost-fakex: listen on loopback only")
	}
	f := &xfake.Server{ClientID: *clientID, ClientSecret: os.Getenv("XPOST_X_CLIENT_SECRET"), Users: map[string]string{}}
	for i, u := range strings.Split(*users, ",") {
		f.Users[strings.TrimSpace(u)] = fmt.Sprintf("10000000000000000%02d", i+1)
	}
	base := "http://" + *addr
	fmt.Printf("fake X on %s. For xpost serve, set:\n  XPOST_X_CLIENT_ID=%s\n  XPOST_X_AUTHORIZE_URL=%s/i/oauth2/authorize\n  XPOST_X_TOKEN_URL=%s/2/oauth2/token\n  XPOST_X_REVOKE_URL=%s/2/oauth2/revoke\n  XPOST_X_ME_URL=%s/2/users/me\n  XPOST_X_POST_URL=%s/2/tweets\n  XPOST_X_MEDIA_URL=%s/2/media/upload\n  XPOST_X_MEDIA_METADATA_URL=%s/2/media/metadata\n  XPOST_X_WEB_URL=https://x.invalid\n"+
		"Created posts: GET %s/_fake/posts; uploads: GET %s/_fake/media. Queue a fault: POST %s/_fake/fault (kind=status&status=503, kind=ratelimit&seconds=60, kind=drop-after, kind=drop-before, kind=garbled; target=post, upload, metadata or status; for uploads also kind=process&checks=2, kind=process-fail)\n",
		base, *clientID, base, base, base, base, base, base, base, base, base, base)
	log.Fatal(http.ListenAndServe(*addr, f.Handler()))
}
