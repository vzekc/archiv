package archiv

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// auth = single: every request is the one user, who moderates; with
// intake = direct their own submission stays with them for taking in.
func TestSingleUserDirectIntake(t *testing.T) {
	a := testApp(t)
	a.cfg.DevUser = ""
	a.cfg.Auth = "single"
	a.cfg.SingleUser = "Familie Müller"
	a.cfg.DirectIntake = true
	h := a.routes()

	if body := get(t, h, "/einreichen/moderation").Body.String(); !strings.Contains(body, "Familie Müller") {
		t.Errorf("the single user is not named on the page")
	}
	res, err := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated) VALUES ('1', 'Fotos', ?, 1, 1)", StDraft)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := res.LastInsertId()
	a.db.Exec(`INSERT INTO files (submission, name, size, path, sha256, kind, status, created)
	    VALUES (?, 'a.jpg', 1, 'files/x/a.jpg', 'aa', 'image', ?, 1)`, sub, FReady)
	if w := postForm(t, h, fmt.Sprintf("/einreichen/e/%d/einreichen", sub), url.Values{"title": {"Fotos"}}, ""); w.Code != 303 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	s, _ := a.db.submission(sub)
	if s.ClaimedBy.String != "1" {
		t.Errorf("own submission not kept for taking in: claimed by %q", s.ClaimedBy.String)
	}
}

// auth = header: id, name and groups come from the proxy's headers.
func TestHeaderAuth(t *testing.T) {
	a := testApp(t)
	a.cfg.DevUser = ""
	a.cfg.Auth = "header"
	h := a.routes()
	if w := get(t, h, "/einreichen/"); w.Code != 401 {
		t.Errorf("without login: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/einreichen/moderation", nil)
	r.Header.Set("OIDC_CLAIM_sub", "42")
	r.Header.Set("OIDC_CLAIM_preferred_username", "Erika")
	r.Header.Set("OIDC_CLAIM_groups", "Mitglieder, Moderatoren")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Erika") {
		t.Errorf("with login: %d, name shown: %v", w.Code, strings.Contains(w.Body.String(), "Erika"))
	}
}
