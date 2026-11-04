// Command xpost-fakegander runs the fake gander Publisher API (package
// gfake) over TLS on loopback, for demos and screenshots. It never
// contacts a real gander site or any other host. Not for production.
//
// It makes a throwaway self-signed certificate for localhost and
// 127.0.0.1 and writes it to -cert-out, so only the demo process trusts
// it (SSL_CERT_FILE=<that file>). The one token it accepts is read from
// -token-file and never printed.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blog/gander/gfake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8743", "listen address (loopback only)")
	tokenFile := flag.String("token-file", "", "file holding the one token the fake accepts")
	certOut := flag.String("cert-out", "fakegander.pem", "where to write the self-signed certificate (PEM)")
	flag.Parse()
	host, port, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		log.Fatal("xpost-fakegander: listen on loopback only")
	}
	tok, err := os.ReadFile(*tokenFile)
	if err != nil || len(strings.TrimSpace(string(tok))) < 32 {
		log.Fatal("xpost-fakegander: -token-file must hold a token of 32 or more bytes")
	}
	cert, err := selfSigned(*certOut)
	if err != nil {
		log.Fatal(err)
	}
	f := &gfake.Server{Base: "https://localhost:" + port, Token: strings.TrimSpace(string(tok))}
	mux := http.NewServeMux()
	mux.Handle("/", f.Handler())
	// Fault injection for screenshots: POST /_fake/fault op=publish&status=429&code=rate_limited[&slug=..][&sticky=1].
	mux.HandleFunc("POST /_fake/fault", func(w http.ResponseWriter, r *http.Request) {
		st, _ := strconv.Atoi(r.FormValue("status"))
		f.AddFault(gfake.Fault{Op: r.FormValue("op"), Slug: r.FormValue("slug"), Status: st, Code: r.FormValue("code"), Message: "example message", Sticky: r.FormValue("sticky") == "1"})
	})
	mux.HandleFunc("POST /_fake/clear", func(w http.ResponseWriter, r *http.Request) { f.ClearFaults() })
	// Seed a post someone else made: POST /_fake/seed slug=..&title=..
	mux.HandleFunc("POST /_fake/seed", func(w http.ResponseWriter, r *http.Request) {
		f.Seed(gfake.Post{Slug: r.FormValue("slug"), Title: r.FormValue("title"), Description: "Invented.", Markdown: "Invented.", Date: "2026-01-05", Draft: true, Cover: []byte("x"), CoverMIME: "image/png"})
	})
	mux.HandleFunc("GET /_fake/calls", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, strings.Join(f.Calls(), "\n"))
	})
	fmt.Printf("fake gander on https://localhost:%s (certificate in %s). Faults: POST /_fake/fault; calls: GET /_fake/calls\n", port, *certOut)
	srv := &http.Server{Addr: *addr, Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func selfSigned(out string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "xpost fake gander"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour), DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(out, pemBytes, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
