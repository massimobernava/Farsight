// farsight-grafana-proxy sits in front of Grafana (which binds loopback
// only, never the Tailscale IP — see debian/postinst) and identifies the
// caller the same way farsight-server does: `tailscale whois` on the
// connection's source address, no login page, no password. The resolved
// login is injected as a header Grafana's auth.proxy trusts (see
// docs/MULTI-TENANCY.md "Opzione B"). It does not decide who gets in —
// that's Grafana's own auth.proxy + whichever Orgs farsight-server already
// provisioned the user into (see POST /tenants/{tenant}/members); this
// only answers "who is this," same as identify() in farsight-server.
package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/farsight/farsight/internal/config"
	"github.com/farsight/farsight/internal/tailscaleip"
	"github.com/farsight/farsight/internal/webassets"
)

const defaultConfigPath = "/etc/farsight/server.conf"

// grafanaIconPaths are the exact paths Grafana's own <head> hardcodes for
// its favicon/touch-icon — not just /favicon.ico, which most browsers
// only fall back to when no <link rel="icon"> is present, and Grafana
// always declares one. Grafana OSS has no supported way to override these
// (white-labeling is Enterprise-only); intercepting them here — replacing
// Grafana's response with Farsight's own icon before it ever reaches
// Grafana — works regardless of license and survives Grafana upgrades
// (these paths, unlike its hashed JS/CSS bundles, aren't versioned).
var grafanaIconPaths = map[string]bool{
	"/public/build/img/fav32.png":            true,
	"/public/build/img/fav16.png":            true,
	"/public/build/img/apple-touch-icon.png": true,
	"/favicon.ico":                           true,
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("farsight-grafana-proxy: %v", err)
	}
}

func run() error {
	confPath := os.Getenv("FARSIGHT_SERVER_CONF")
	if confPath == "" {
		confPath = defaultConfigPath
	}
	cfg, err := config.ParseFile(confPath)
	if err != nil {
		return err
	}

	internalURL := cfg.Get("GRAFANA_INTERNAL_URL", "http://127.0.0.1:3001")
	proxyPort := cfg.Get("GRAFANA_PROXY_PORT", "3000")
	headerName := cfg.Get("GRAFANA_PROXY_HEADER", "X-WEBAUTH-USER")
	bindIface := cfg.Get("BIND_INTERFACE", "tailscale")

	target, err := url.Parse(internalURL)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bindIP := "127.0.0.1"
	if bindIface == "tailscale" {
		ip, err := tailscaleip.Current(ctx)
		if err != nil {
			return err
		}
		bindIP = ip
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		// Always strip any client-supplied value first — otherwise a
		// caller could just set this header themselves and skip identity
		// resolution entirely. Grafana's own `whitelist` setting (only
		// trust this header from 127.0.0.1) is the other half of closing
		// that gap; this half is "never forward what they sent us."
		req.Header.Del(headerName)
		login, err := tailscaleip.WhoIs(req.Context(), req.RemoteAddr)
		if err != nil {
			log.Printf("whois failed for %s: %v", req.RemoteAddr, err)
			return
		}
		req.Header.Set(headerName, login)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if grafanaIconPaths[req.URL.Path] {
			w.Header().Set("Content-Type", "image/x-icon")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(webassets.FaviconICO)
			return
		}
		rp.ServeHTTP(w, req)
	})

	addr := bindIP + ":" + proxyPort
	srv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("farsight-grafana-proxy listening on http://%s -> %s (Tailscale-only)", addr, internalURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
