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

const version = "0.2.0"

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
	case "version", "-v", "--version":
		fmt.Println("r2proxy", version)
	case "help", "-h", "--help":
		usage()
	default:
		fatal("unknown command %q (try: serve stats tail rules)", cmd)
	}
}

func cmdServe(args []string) {
	fs := newFlagSet("serve")
	listen := fs.String("listen", envOr("R2PROXY_LISTEN", "0.0.0.0:8080"), "proxy (data-plane) listen address")
	adminListen := fs.String("admin-listen", envOr("R2PROXY_ADMIN_LISTEN", "0.0.0.0:8081"), "admin API + console listen address")
	configPath := fs.String("config", envOr("R2PROXY_CONFIG", "r2proxy.json"), "path to config/state file")
	adminToken := fs.String("admin-token", os.Getenv("R2PROXY_ADMIN_TOKEN"), "admin token (generated if empty)")

	endpoint := fs.String("endpoint", os.Getenv("R2PROXY_ENDPOINT"), "upstream R2 endpoint URL")
	accessKey := fs.String("access-key", envOr("R2PROXY_ACCESS_KEY", os.Getenv("AWS_ACCESS_KEY_ID")), "upstream access key id")
	secretKey := fs.String("secret-key", envOr("R2PROXY_SECRET_KEY", os.Getenv("AWS_SECRET_ACCESS_KEY")), "upstream secret access key")
	region := fs.String("region", envOr("R2PROXY_REGION", "auto"), "upstream region")
	parseCLIFlags(fs, args)

	app := newApp(*configPath)
	if err := app.Load(); err != nil {
		fatal("load config: %v", err)
	}
	if *adminToken != "" {
		app.AdminToken = *adminToken
	}
	if _, err := app.Configure(*endpoint, *accessKey, *secretKey, *region); err != nil {
		fatal("%v", err)
	}

	proxy := newProxyServer(app)
	admin := newAdminServer(app, *listen, version)

	proxySrv := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: 15 * time.Second}
	adminSrv := &http.Server{Addr: *adminListen, Handler: admin.routes(), ReadHeaderTimeout: 15 * time.Second}

	go func() {
		if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("proxy server: %v", err)
		}
	}()
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("admin server: %v", err)
		}
	}()

	printCreds(app, *listen, *adminListen)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxySrv.Shutdown(ctx)
	adminSrv.Shutdown(ctx)
}

func printCreds(app *App, listen, adminListen string) {
	fmt.Println("┌─ r2proxy ready ─────────────────────────────────────────")
	fmt.Printf("│ proxy endpoint    http://%s\n", displayAddr(listen))
	fmt.Printf("│ proxy access key  %s\n", app.ProxyAccessKeyID)
	fmt.Printf("│ proxy secret key  %s\n", app.ProxySecretKey)
	fmt.Printf("│ upstream          %s (%s)\n", app.Endpoint, regionOr(app.Region))
	fmt.Println("│")
	fmt.Printf("│ console           http://%s\n", displayAddr(adminListen))
	fmt.Printf("│ admin token       %s\n", app.AdminToken)
	fmt.Println("└─────────────────────────────────────────────────────────")
	fmt.Printf("try: AWS_ACCESS_KEY_ID=%s AWS_SECRET_ACCESS_KEY=%s AWS_DEFAULT_REGION=%s \\\n",
		app.ProxyAccessKeyID, app.ProxySecretKey, regionOr(app.Region))
	fmt.Printf("     aws s3 ls s3://<bucket>/ --endpoint-url http://%s\n", displayAddr(listen))
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "localhost:" + portOf(listen)
	}
	return listen
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
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
	fmt.Print(`r2proxy — S3/R2 mitm proxy with stats and error injection

USAGE
  r2proxy serve   [flags]     run the proxy + admin API + web console
  r2proxy stats   [--json]    show statistics
  r2proxy tail                stream recent requests
  r2proxy rules   <cmd>       manage error-injection rules (list|add|rm|toggle|clear)
  r2proxy version

SERVE FLAGS
  --listen         proxy data-plane addr           (default 0.0.0.0:8080)
  --admin-listen   admin API + console addr        (default 0.0.0.0:8081)
  --config         config/state file               (default r2proxy.json)
  --admin-token    admin token                     (generated if empty)
  --endpoint --access-key --secret-key --region    upstream R2 target

CLIENT ENV (for stats/tail/rules)
  R2PROXY_ADMIN    admin base URL   (default http://127.0.0.1:8081)
  R2PROXY_TOKEN    admin token
`)
}
