// Package archiv is the web service of an archive of documents, software
// and pictures: the catalog with its tags, search over the full text,
// item pages, the reader, ZIP downloads, submissions with moderation and
// AI suggestions, and the editing history.
//
// The service is built as a command by calling Main, optionally with
// extensions that connect it to a particular installation (see Extension).
//
// Commands: [command] [-config file] [flags]
//
//	serve    run the web service (default)
//	import   build the catalog from the archive (the first sync)
//	sync     bring the catalog in line with the archive
//	export-descriptions <dir>   write the catalog as descriptions files
//	classify  propose tags for the untagged items (AI), or relations and merges
package archiv

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

type App struct {
	cfg     *Config
	db      *DB
	cat     *Catalog
	events  *eventHub
	pagesDB pagesStore
	arch    *Archive
	names   *Names
	tmpl    *template.Template
	tus     *handler.Handler
	exts    []Extension
	ai      *AI
	wake    chan struct{}
}

func logf(format string, args ...any) { log.Printf(format, args...) }

// commands are the subcommands; each gets the arguments after its name.
var commands = map[string]func(args []string){
	"serve":  serve,
	"sync":   syncCmd,
	"import": syncCmd,

	"export-descriptions": exportDescriptionsCmd,
	"classify":            classifyCmd,
	"split-item":          splitItemCmd,
	"merge-gone":          mergeGoneCmd,
}

// Main runs the command named on the command line with the given
// extensions and exits when it is done.
func Main(exts ...Extension) {
	extensions = exts
	log.SetFlags(log.Ldate | log.Ltime)
	args := os.Args[1:]
	name := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	cmd, ok := commands[name]
	if !ok {
		names := make([]string, 0, len(commands))
		for n := range commands {
			names = append(names, n)
		}
		sort.Strings(names)
		log.Fatalf("unknown command %q (known: %s)", name, strings.Join(names, ", "))
	}
	cmd(args)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	a, err := newApp(cfg)
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: a.routes(), ReadHeaderTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go a.handleCompletedUploads(ctx)
	go a.worker(ctx)
	go a.suggester(ctx)

	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	logf("archiv listening on %s (login %s, %d extensions, AI %v)", cfg.Listen, cfg.Auth, len(a.exts), a.ai.enabled())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// New opens the databases and builds an App with the given extensions, for
// programs and tests that run the archive themselves.
func New(cfg *Config, exts ...Extension) (*App, error) { return newAppWith(cfg, exts) }

// Handler answers the requests of the site.
func (a *App) Handler() http.Handler { return a.routes() }

// newApp opens the databases and builds the handlers with the extensions
// Main got; the background jobs are started by serve.
func newApp(cfg *Config) (*App, error) { return newAppWith(cfg, extensions) }

func newAppWith(cfg *Config, exts []Extension) (*App, error) {
	for _, d := range []string{filepath.Join(cfg.StagingDir, "tus"), filepath.Join(cfg.StagingDir, "files"),
		filepath.Dir(cfg.DBPath), filepath.Dir(cfg.CatalogDB)} {
		if err := os.MkdirAll(d, 0o2770); err != nil {
			return nil, err
		}
	}
	db, err := openDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	cat, err := openCatalog(cfg.CatalogDB)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, db: db, cat: cat, events: newEventHub(), arch: &Archive{cfg: cfg}, names: newNames(db),
		exts: exts, wake: make(chan struct{}, 1)}
	a.ai = newAI(a)
	for _, e := range a.exts {
		if err := e.Init(a); err != nil {
			return nil, err
		}
	}
	if a.tmpl, err = parseTemplates(a); err != nil {
		return nil, err
	}
	if a.tus, err = a.newTus(); err != nil {
		return nil, err
	}
	return a, nil
}

// poke wakes the worker up early.
func (a *App) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *App) url(format string, args ...any) string {
	return a.cfg.BasePath + fmt.Sprintf(format, args...)
}

func (a *App) absURL(format string, args ...any) string {
	return a.cfg.BaseURL + fmt.Sprintf(format, args...)
}
