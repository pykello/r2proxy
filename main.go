package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "serve":
		cmdServe(args)
	case "stats":
		cmdStats(args)
	case "tail":
		cmdTail(args)
	case "rules":
		cmdRules(args)
	case "tenant":
		cmdTenant(args)
	case "version", "-v", "--version":
		fmt.Println("r2proxy", version)
	case "help", "-h", "--help":
		usage()
	default:
		fatal("unknown command %q (try: serve stats tail rules tenant)", cmd)
	}
}

func cmdServe(args []string) {
	fs := newFlagSet("serve")
	listen := fs.String("listen", envOr("R2PROXY_LISTEN", "0.0.0.0:8080"), "proxy (data-plane) listen address")
	adminListen := fs.String("admin-listen", envOr("R2PROXY_ADMIN_LISTEN", "0.0.0.0:8081"), "admin API + UI listen address")
	configPath := fs.String("config", envOr("R2PROXY_CONFIG", "r2proxy.json"), "path to config/state file")
	adminToken := fs.String("admin-token", os.Getenv("R2PROXY_ADMIN_TOKEN"), "global admin token (generated if empty)")

	// Bootstrap: auto-create a default tenant on first run from these.
	endpoint := fs.String("endpoint", os.Getenv("R2PROXY_ENDPOINT"), "bootstrap: upstream R2 endpoint URL")
	accessKey := fs.String("access-key", envOr("R2PROXY_ACCESS_KEY", os.Getenv("AWS_ACCESS_KEY_ID")), "bootstrap: upstream access key id")
	secretKey := fs.String("secret-key", envOr("R2PROXY_SECRET_KEY", os.Getenv("AWS_SECRET_ACCESS_KEY")), "bootstrap: upstream secret access key")
	region := fs.String("region", envOr("R2PROXY_REGION", "auto"), "bootstrap: upstream region")
	parseCLIFlags(fs, args)

	mgr := newManager(*configPath)
	if err := mgr.Load(); err != nil {
		fatal("load config: %v", err)
	}
	if *adminToken != "" {
		mgr.AdminToken = *adminToken
		_ = mgr.Save()
	}
	if mgr.EnsureAdminToken() {
		log.Printf("generated global admin token (store it): %s", mgr.AdminToken)
	}

	// Bootstrap a default tenant if requested and none exist.
	if *endpoint != "" && *accessKey != "" && *secretKey != "" && len(mgr.List()) == 0 {
		t, err := mgr.CreateTenant(TenantSpec{
			Name: "default", Endpoint: *endpoint, UpstreamKeyID: *accessKey,
			UpstreamSecret: *secretKey, Region: *region,
		})
		if err != nil {
			fatal("bootstrap tenant: %v", err)
		}
		log.Printf("bootstrapped tenant %q", t.Name)
		printTenantCreds(t)
	}

	proxy := newProxyServer(mgr)
	admin := newAdminServer(mgr, *listen, version)

	proxySrv := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: 15 * time.Second}
	adminSrv := &http.Server{Addr: *adminListen, Handler: admin.routes(), ReadHeaderTimeout: 15 * time.Second}

	go func() {
		log.Printf("proxy (data-plane) listening on %s", *listen)
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("proxy server: %v", err)
		}
	}()
	go func() {
		log.Printf("admin API + UI listening on http://%s", *adminListen)
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("admin server: %v", err)
		}
	}()

	log.Printf("console: http://%s   (login with the admin token above)", displayAddr(*adminListen))
	log.Printf("config/state: %s   tenants: %d", *configPath, len(mgr.List()))

	// Wait for shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxySrv.Shutdown(ctx)
	adminSrv.Shutdown(ctx)
}

func printTenantCreds(t *Tenant) {
	fmt.Println("┌─ tenant credentials (shown once) ───────────────────────")
	fmt.Printf("│ tenant id        %s\n", t.ID)
	fmt.Printf("│ proxy access key %s\n", t.ProxyAccessKeyID)
	fmt.Printf("│ proxy secret key %s\n", t.ProxySecretKey)
	fmt.Printf("│ tenant token     %s\n", t.Token)
	fmt.Printf("│ upstream         %s\n", t.Endpoint)
	fmt.Println("└─────────────────────────────────────────────────────────")
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "localhost:" + portOf(listen)
	}
	return listen
}

// ---- small flag/error helpers shared with cli.go ----

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs
}

func parseCLIFlags(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "r2proxy: "+format+"\n", a...)
	os.Exit(1)
}

func usage() {
	fmt.Print(`r2proxy — S3/R2 mitm proxy with stats, error injection, and multi-tenant isolation

USAGE
  r2proxy serve   [flags]     run the proxy + admin API + web console
  r2proxy stats   [--json]    show statistics for a tenant
  r2proxy tail                stream recent requests
  r2proxy rules   <cmd>       manage error-injection rules (list|add|rm|toggle|clear)
  r2proxy tenant  <cmd>       manage tenants (list|add|rm)  [superuser token]
  r2proxy version

SERVE FLAGS
  --listen         proxy data-plane addr           (default 0.0.0.0:8080)
  --admin-listen   admin API + UI addr             (default 0.0.0.0:8081)
  --config         config/state file               (default r2proxy.json)
  --admin-token    global admin token              (generated if empty)
  --endpoint --access-key --secret-key --region   bootstrap a default tenant

CLIENT ENV (for stats/tail/rules/tenant)
  R2PROXY_ADMIN    admin base URL   (default http://127.0.0.1:8081)
  R2PROXY_TOKEN    admin or tenant token
  R2PROXY_TENANT   tenant id (when using the global admin token)
`)
}
