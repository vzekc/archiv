package archiv

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n) / 1024
	for _, u := range []string{"kB", "MB", "GB", "TB"} {
		if v < 1024 || u == "TB" {
			s := fmt.Sprintf("%.0f %s", v, u)
			if v < 10 {
				s = fmt.Sprintf("%.1f %s", v, u)
			}
			return strings.Replace(s, ".", ",", 1)
		}
		v /= 1024
	}
	return fmt.Sprintf("%d B", n)
}

var berlin = func() *time.Location {
	l, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.Local
	}
	return l
}()

func fmtTime(t int64) string {
	if t == 0 {
		return ""
	}
	return time.Unix(t, 0).In(berlin).Format("02.01.2006, 15:04")
}

func fmtDate(t int64) string {
	if t == 0 {
		return ""
	}
	return time.Unix(t, 0).In(berlin).Format("02.01.2006")
}

var stateLabels = map[string]string{
	StDraft:      "Entwurf",
	StAnalysing:  "wird eingeordnet",
	StModeration: "in Moderation",
	StQuestion:   "Rückfrage",
	StIntake:     "wird aufgenommen",
	StDone:       "im Archiv",
	StRejected:   "abgelehnt",
	StFailed:     "Aufnahme fehlgeschlagen",
}

func stateLabel(s string) string {
	if l, ok := stateLabels[s]; ok {
		return l
	}
	return s
}

var kindLabels = map[string]string{
	"pdf": "PDF", "image": "Bild", "archive": "Archiv", "iso": "CD-/DVD-Abbild",
	"disk": "Diskettenabbild", "text": "Text", "other": "Datei",
}

func kindLabel(k string) string {
	if l, ok := kindLabels[k]; ok {
		return l
	}
	return "Datei"
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// randomHex is n random bytes as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// days is a duration in whole days, in words.
func days(d time.Duration) string {
	n := int(d.Hours()/24 + 0.5)
	return plural(n, "Tag", "Tagen")
}

// since is the time since t in whole days, in words.
func since(t int64) string {
	return days(time.Since(time.Unix(t, 0)))
}

// archiveURL is the absolute URL of a path below the archive root.
func archiveURL(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = urlPathEscape(s)
	}
	return siteURL + "/" + strings.Join(parts, "/")
}
