package archiv

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the service needs to know about its surroundings.
// Values come from defaults, then the config file (key = value lines), then
// command line flags.
type Config struct {
	Home            string // the installation: data/, html/, bin/ and eingang/ lie below it
	Listen          string
	SiteURL         string   // public URL of the site, without trailing slash
	SiteTitle       string   // name of the archive in the page header; default the host of SiteURL
	SiteTagline     string   // line beside the name in the page header
	HomeTitle       string   // heading of the start page; default SiteTitle
	EventsHint      string   // start page: the line under "Treffen"
	CollectionsHint string   // start page: the line under "CD-ROMs und Sammlungen"
	ArchiveName     string   // what the AI prompts call the archive ("Archiv der Familie Müller")
	SiteAliases     []string // further host names the site answers to (while moving)
	BaseURL         string   // public URL of einreichen; default SiteURL + BasePath
	BasePath        string   // URL path the service is mounted at, e.g. /einreichen
	StagingDir      string   // local disk: uploads, unpacked files, previews
	DBPath          string
	ArchiveRoot     string // the files of the archive
	CollectionDB    string // index of the archive, rebuilt nightly
	CatalogDB       string // the catalog: items, tags, history
	PagesDB         string // text of the documents, page by page
	TaxonomyConf    string // fixed tag vocabulary
	LegacyMap       string // rules from old folders to tags
	MirrorsConf     string // additions to the list of mirrors
	BinDir          string // the Python tools of the archive
	ReindexLock     string
	BackupDir       string // originals of unpacked archives
	ModeratorGrp    string
	EditGroup       string // edits the catalog directly; everyone else proposes
	TagCreateGrp    string // creates new tags
	Editing         bool   // the catalog can be edited (after the old site is switched off)
	ThumbsByHash    bool   // thumbnails and previews lie at <dir>/<sha[0:2]>/<sha>.jpg (thumbs = hash)
	APIKeyFile      string
	Model           string
	Auth            string // header: a proxy in front passes user, name and groups (OIDC); single: one user, no login
	UserHeader      string // auth = header: the user's id
	NameHeader      string // auth = header: the user's display name (optional)
	GroupsHeader    string // auth = header: the user's groups, comma-separated
	SingleUser      string // auth = single: the name the one user goes by
	DirectIntake    bool   // a moderator's own submission goes into the archive without a review step
	MaxFile         int64
	MaxSubmission   int64
	MaxPending      int64
	DevUser         string // "id:name:group,group" - bypasses the login headers, for tests only

	ClaimReminder   time.Duration
	ClaimRelease    time.Duration
	UnclaimedRemind time.Duration
	QuestionRemind  time.Duration
}

const GB = int64(1) << 30

func defaultConfig() *Config {
	return &Config{
		Listen:          "127.0.0.1:8095",
		SiteURL:         "http://localhost:8095",
		BasePath:        "/einreichen",
		ModeratorGrp:    "Moderatoren",
		EditGroup:       "Moderatoren",
		TagCreateGrp:    "Moderatoren",
		Model:           "claude-opus-5-5",
		ArchiveName:     "Archiv",
		EventsHint:      "Fotos von Treffen und Ausstellungen",
		CollectionsHint: "Sammlungen auf CD und DVD",
		Auth:            "single",
		ThumbsByHash:    true,
		UserHeader:      "OIDC_CLAIM_sub",
		NameHeader:      "OIDC_CLAIM_preferred_username",
		GroupsHeader:    "OIDC_CLAIM_groups",
		SingleUser:      "Archivar",
		MaxFile:         20 * GB,
		MaxSubmission:   50 * GB,
		MaxPending:      100 * GB,

		ClaimReminder:   48 * time.Hour,
		ClaimRelease:    7 * 24 * time.Hour,
		UnclaimedRemind: 72 * time.Hour,
		QuestionRemind:  14 * 24 * time.Hour,
	}
}

// NewConfig is the configuration with its defaults; Resolve completes it.
func NewConfig() *Config { return defaultConfig() }

// Resolve fills what is left empty from Home and the other settings.
func (c *Config) Resolve() { c.resolve() }

// resolve fills the paths left empty from Home: the databases and settings
// in data/, the archive in html/, the tools in bin/, uploads in eingang/.
func (c *Config) resolve() {
	if c.Home == "" {
		c.Home = os.Getenv("ARCHIV_HOME")
	}
	if c.Home == "" {
		c.Home, _ = os.Getwd()
	}
	at := func(p *string, rel ...string) {
		if *p == "" {
			*p = filepath.Join(append([]string{c.Home}, rel...)...)
		}
	}
	at(&c.StagingDir, "eingang")
	at(&c.DBPath, "data", "einreichen.sqlite")
	at(&c.ArchiveRoot, "html")
	at(&c.CollectionDB, "data", "collection.sqlite")
	at(&c.CatalogDB, "data", "catalog.sqlite")
	at(&c.PagesDB, "data", "pages.sqlite")
	at(&c.TaxonomyConf, "data", "taxonomy.conf")
	at(&c.LegacyMap, "data", "legacy-map.conf")
	at(&c.MirrorsConf, "data", "mirrors.conf")
	at(&c.BinDir, "bin")
	at(&c.ReindexLock, "data", "reindex.lock")
	at(&c.BackupDir, "backup", "einreichungen")
	at(&c.APIKeyFile, "data", "anthropic.key")
	c.SiteURL = strings.TrimRight(c.SiteURL, "/")
	c.BasePath = "/" + strings.Trim(c.BasePath, "/")
	if c.BaseURL == "" {
		c.BaseURL = c.SiteURL + c.BasePath
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.SiteTitle == "" {
		if u, err := url.Parse(c.SiteURL); err == nil && u.Host != "" {
			c.SiteTitle = u.Host
		} else {
			c.SiteTitle = c.SiteURL
		}
	}
	if c.HomeTitle == "" {
		c.HomeTitle = c.SiteTitle
	}
	siteURL = c.SiteURL
	archiveName = c.ArchiveName
}

// loadConfig parses the common flags from args into fs; a command adds its
// own flags to fs before.
func loadConfig(fs *flag.FlagSet, args []string) (*Config, error) {
	c := defaultConfig()
	file := fs.String("config", "", "config file (default $ARCHIV_HOME/data/archiv.conf)")
	listen := fs.String("listen", "", "listen address")
	dev := fs.String("dev-user", "", "fake login id:name:groups (testing only)")
	dbPath := fs.String("db", "", "database")
	staging := fs.String("staging", "", "staging directory")
	fs.Parse(args)

	if *file == "" {
		home := os.Getenv("ARCHIV_HOME")
		if home == "" {
			home, _ = os.Getwd()
		}
		*file = filepath.Join(home, "data", "archiv.conf")
	}
	if err := c.readFile(*file); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if *listen != "" {
		c.Listen = *listen
	}
	if *dev != "" {
		c.DevUser = *dev
	}
	if *dbPath != "" {
		c.DBPath = *dbPath
	}
	if *staging != "" {
		c.StagingDir = *staging
	}
	c.resolve()
	if c.Auth != "header" && c.Auth != "single" {
		return nil, fmt.Errorf("auth: header or single, not %q", c.Auth)
	}
	return c, nil
}

func (c *Config) readFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if err := c.set(k, v); err != nil {
			return fmt.Errorf("%s:%d: %v", path, n, err)
		}
	}
	return sc.Err()
}

func (c *Config) set(k, v string) error {
	str := map[string]*string{
		"home": &c.Home, "listen": &c.Listen, "site_url": &c.SiteURL, "site_title": &c.SiteTitle, "site_tagline": &c.SiteTagline, "home_title": &c.HomeTitle, "events_hint": &c.EventsHint, "collections_hint": &c.CollectionsHint, "archive_name": &c.ArchiveName, "base_url": &c.BaseURL, "base_path": &c.BasePath,
		"staging": &c.StagingDir, "db": &c.DBPath, "archive": &c.ArchiveRoot,
		"collection": &c.CollectionDB, "catalog": &c.CatalogDB, "pages": &c.PagesDB, "taxonomy": &c.TaxonomyConf,
		"legacy_map": &c.LegacyMap, "mirrors_conf": &c.MirrorsConf, "bin": &c.BinDir, "reindex_lock": &c.ReindexLock,
		"backup": &c.BackupDir, "moderator_group": &c.ModeratorGrp,
		"edit_group": &c.EditGroup, "tag_create_group": &c.TagCreateGrp,
		"api_key_file": &c.APIKeyFile, "model": &c.Model, "auth": &c.Auth, "user_header": &c.UserHeader,
		"name_header": &c.NameHeader, "groups_header": &c.GroupsHeader, "single_user": &c.SingleUser,
	}
	if p, ok := str[k]; ok {
		*p = v
		return nil
	}
	if k == "thumbs" {
		switch v {
		case "hash":
			c.ThumbsByHash = true
		case "path":
			c.ThumbsByHash = false
		default:
			return fmt.Errorf("thumbs: hash or path")
		}
		return nil
	}
	if k == "editing" {
		c.Editing = v == "on" || v == "yes" || v == "ja" || v == "true" || v == "1"
		return nil
	}
	if k == "intake" {
		switch v {
		case "direct":
			c.DirectIntake = true
		case "moderated":
			c.DirectIntake = false
		default:
			return fmt.Errorf("intake: direct or moderated")
		}
		return nil
	}
	if k == "site_aliases" {
		c.SiteAliases = strings.Fields(strings.ReplaceAll(v, ",", " "))
		return nil
	}
	sizes := map[string]*int64{"max_file_gb": &c.MaxFile, "max_submission_gb": &c.MaxSubmission,
		"max_pending_gb": &c.MaxPending}
	if p, ok := sizes[k]; ok {
		n, err := strconv.ParseFloat(v, 64)
		*p = int64(n * float64(GB))
		return err
	}
	durs := map[string]*time.Duration{"claim_reminder": &c.ClaimReminder, "claim_release": &c.ClaimRelease,
		"unclaimed_reminder": &c.UnclaimedRemind, "question_reminder": &c.QuestionRemind}
	if p, ok := durs[k]; ok {
		d, err := parseDays(v)
		*p = d
		return err
	}
	for _, e := range extensions {
		if cf, ok := e.(Configurer); ok {
			if known, err := cf.Configure(k, v); known {
				return err
			}
		}
	}
	return fmt.Errorf("unknown setting %q", k)
}

// parseDays accepts "2d", "36h" or a plain number of days.
func parseDays(v string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(v, "d"); ok {
		v = d
	} else if _, err := strconv.ParseFloat(v, 64); err != nil {
		return time.ParseDuration(v)
	}
	n, err := strconv.ParseFloat(v, 64)
	return time.Duration(n * float64(24*time.Hour)), err
}

// siteURL is the public address of the site, for the links into the
// archive; set from the configuration.
var siteURL = defaultConfig().SiteURL

// archiveName is what the AI prompts call the archive; set from the
// configuration.
var archiveName = defaultConfig().ArchiveName

// promptText fills the archive's name and address into an AI prompt.
func promptText(prompt string) string {
	return strings.NewReplacer("{archive}", archiveName, "{site}", siteName()).Replace(prompt)
}

// siteName is the host name of the site, as the AI prompts name it.
func siteName() string {
	if u, err := url.Parse(siteURL); err == nil && u.Host != "" {
		return u.Host
	}
	return siteURL
}

// publicHost is the host name a request came in under, if the site answers
// to it, else the host of site_url. Apache passes it in X-Forwarded-Host,
// possibly with values of its own appended.
func (c *Config) publicHost(r *http.Request) string {
	site, _ := url.Parse(c.SiteURL)
	h, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	if h = strings.ToLower(strings.TrimSpace(h)); h != "" && c.knownHost(h) {
		return h
	}
	return site.Host
}

// knownHost tells whether the site answers to host.
func (c *Config) knownHost(host string) bool {
	site, _ := url.Parse(c.SiteURL)
	if strings.EqualFold(host, site.Host) {
		return true
	}
	for _, a := range c.SiteAliases {
		if strings.EqualFold(host, a) {
			return true
		}
	}
	return false
}
