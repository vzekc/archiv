package archiv

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

func parseTemplates(a *App) (*template.Template, error) {
	funcs := template.FuncMap{
		"size":      humanSize,
		"time":      fmtTime,
		"date":      fmtDate,
		"state":     stateLabel,
		"kind":      kindLabel,
		"name":      a.names.Name,
		"plural":    plural,
		"archive":   archiveURL,
		"url":       func(p string, args ...any) string { return a.cfg.BasePath + p + joinArgs(args) },
		"pct":       func(f float64) int { return int(f*100 + 0.5) },
		"first":     func(n int, s []string) []string { return s[:min(n, len(s))] },
		"lines":     func(s string) []string { return strings.Split(s, "\n") },
		"base":      path.Base,
		"site":      func() string { return a.cfg.SiteTitle },
		"tagline":   func() string { return a.cfg.SiteTagline },
		"homeTitle": func() string { return a.cfg.HomeTitle },
		"authLocal": func() bool { return a.cfg.Auth == "local" },
		"hasInbox":  func() bool { return a.mailbox != nil },
		"unread":    a.unread,
		"safeHTML":  func(s string) template.HTML { return template.HTML(s) },
		"inc":       func(i int) int { return i + 1 },
		"int":       func(n int64) int { return int(n) },
		"int64":     func(n int) int64 { return int64(n) },
		"num":       num,
		"href":      href,
		"dir":       parentDir,
		"pathesc":   func(s string) string { return strings.TrimPrefix(href(s), "/") },
		"chain":     scopeChain,
		"inChain": func(p string, chain []SearchArea) bool {
			for _, c := range chain {
				if c.Path == p {
					return true
				}
			}
			return false
		},
		"facetLabel":   func(f string) string { return facetLabels[f] },
		"markdown":     renderMarkdown,
		"itemlinks":    itemLinks,
		"kindLabel":    func(k string) string { return commentKinds[k] },
		"relLabel":     func(k string) string { return relKinds[k] },
		"relKinds":     func() map[string]string { return relKinds },
		"commentKinds": func() map[string]string { return commentKinds },
		"list":         func(xs ...string) []string { return xs },
		"roleLabel":    func(r string) string { return roleLabels[r] },
		"revState":     func(s string) string { return revStates[s] },
		"listingLightbox": func(d any) bool {
			l, ok := d.(*Listing)
			return ok && l.Lightbox
		},
		"withData": func(p *SitePage, d any) *SitePage {
			q := *p
			q.Data = d
			return &q
		},
		"listingPreviews": func(d any) bool {
			l, ok := d.(*Listing)
			return ok && l.Previews
		},
		"since": func(t int64) string {
			if t == 0 {
				return ""
			}
			return since(t)
		},
		"gb": func(n int64) string {
			return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(float64(n)/float64(GB), 'f', 1, 64), "0"), ".")
		},
		"isoDate": func(t int64) string {
			if t == 0 {
				return ""
			}
			return time.Unix(t, 0).In(berlin).Format("2006-01-02")
		},
		"thumb": func(f *File) string {
			if f.ArchivePath != "" {
				p := filepath.Join(a.cfg.ArchiveRoot, "thumbs", filepath.FromSlash(f.ArchivePath)+".jpg")
				if _, err := os.Stat(p); err == nil {
					return "/thumbs/" + strings.TrimPrefix(archiveURL(f.ArchivePath), siteURL+"/") + ".jpg"
				}
				return ""
			}
			if parseInfo(f.Info).Thumb {
				return a.cfg.BasePath + "/f/" + strconv.FormatInt(f.ID, 10) + "/bild/thumb"
			}
			return ""
		},
		"evt": eventLabel,
		"v":   func() int64 { return startTime },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
	}
	t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, e := range a.exts {
		if tp, ok := e.(TemplateProvider); ok {
			files, patterns, more := tp.Templates()
			if more != nil {
				t.Funcs(more)
			}
			if t, err = t.ParseFS(files, patterns...); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

var startTime = time.Now().Unix()

func joinArgs(args []any) string {
	var sb strings.Builder
	for _, x := range args {
		switch v := x.(type) {
		case int64:
			sb.WriteString(strconv.FormatInt(v, 10))
		case int:
			sb.WriteString(strconv.Itoa(v))
		case string:
			sb.WriteString(v)
		}
	}
	return sb.String()
}

var eventLabels = map[string]string{
	"created": "angelegt", "submitted": "eingereicht", "deleted": "gelöscht", "file removed": "Datei entfernt",
	"answered": "Rückfrage beantwortet", "claimed": "übernommen", "released": "zurückgegeben",
	"comment": "Kommentar", "question": "Rückfrage", "rejected": "abgelehnt", "accepted": "zur Aufnahme freigegeben",
	"reanalyse": "neu analysieren", "folder created": "Verzeichnis angelegt", "file taken in": "aufgenommen",
	"taken in": "Aufnahme abgeschlossen", "intake failed": "Aufnahme fehlgeschlagen", "auto release": "automatisch zurückgegeben",
}

func eventLabel(s string) string {
	if l, ok := eventLabels[s]; ok {
		return l
	}
	return s
}

// assetHandler serves the shared style sheet and scripts of the site
// (static/assets) at /assets/.
func assetHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static/assets")
	fsrv := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fsrv.ServeHTTP(w, r)
	})
}

func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	fsrv := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mjs") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fsrv.ServeHTTP(w, r)
	})
}

// itemRef matches an archive object number; two or more digits keep paper sizes such as A4 plain text.
var itemRef = regexp.MustCompile(`\bA(\d{2,})\b`)

// itemLinks escapes text and links each archive object number in it ("A9640")
// to the object's page.
func itemLinks(s string) template.HTML {
	esc := template.HTMLEscapeString(s)
	return template.HTML(itemRef.ReplaceAllString(esc, `<a href="/a/$1">A$1</a>`))
}
