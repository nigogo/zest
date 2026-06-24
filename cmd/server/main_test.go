package main

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	oldwd, _ := os.Getwd()
	os.Chdir("../..")
	t.Cleanup(func() { os.Chdir(oldwd) })
	t.Helper()
	dir := t.TempDir()
	old := os.Getenv("DATABASE_PATH")
	os.Setenv("DATABASE_PATH", filepath.Join(dir, "app.db"))
	t.Cleanup(func() { os.Setenv("DATABASE_PATH", old) })
	db, err := sql.Open("sqlite", os.Getenv("DATABASE_PATH")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{db: db, base: "http://example.test", env: "development", undoWindow: 20 * time.Second, amounts: []int{10, 20, 40, 60}}
	a.migrate()
	a.seed()
	return a
}
func TestSeedCreatesQRCommands(t *testing.T) {
	a := testApp(t)
	var c int
	if err := a.db.QueryRow("select count(*) from qr_commands").Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 96 {
		t.Fatalf("qr commands=%d, want 96", c)
	}
}

func TestSeedBackfillsMissingQRCommands(t *testing.T) {
	a := testApp(t)
	if _, err := a.db.Exec("delete from qr_commands"); err != nil {
		t.Fatal(err)
	}
	a.seed()
	var c int
	if err := a.db.QueryRow("select count(*) from qr_commands").Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 96 {
		t.Fatalf("qr commands=%d, want 96 after backfill", c)
	}
}

func TestInventoryStockAndReversal(t *testing.T) {
	a := testApp(t)
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type) values('e1','org_dev','place_0','prod_0','user_admin',40,'add')")
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type,reversed_event_id) values('e2','org_dev','place_0','prod_0','user_admin',-40,'reversal','e1')")
	var stock int
	a.db.QueryRow("select coalesce(sum(quantity_delta),0) from inventory_events where place_id='place_0' and product_id='prod_0'").Scan(&stock)
	if stock != 0 {
		t.Fatalf("stock=%d, want 0", stock)
	}
}
func TestNegativeStockAllowed(t *testing.T) {
	a := testApp(t)
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type) values('e1','org_dev','place_0','prod_0','user_admin',-20,'subtract')")
	row := a.eventData("e1", "org_dev")
	if row == nil || row["Stock"].(int) != -20 || row["Negative"].(bool) != true {
		t.Fatalf("expected negative stock row: %#v", row)
	}
}
func TestPasswordHashStable(t *testing.T) {
	if hashpw("secret") == "secret" || hashpw("secret") != hashpw("secret") {
		t.Fatal("hash should be non-plain and stable")
	}
}

func TestSeedCreatesApprovedDevelopmentUser(t *testing.T) {
	a := testApp(t)
	var role, status, hash string
	if err := a.db.QueryRow(`select m.role,m.status,u.password_hash from users u join memberships m on m.user_id=u.id where u.email=?`, "user@example.com").Scan(&role, &status, &hash); err != nil {
		t.Fatal(err)
	}
	if role != "operator" || status != "approved" || hash != hashpw("password") {
		t.Fatalf("seed user role=%q status=%q hash matches=%v, want approved operator with simple password", role, status, hash == hashpw("password"))
	}
}

func TestDevQRCodesPageLinksToScanRoutes(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=user%40example.com&password=password"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/dev/qr-codes", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev qr status=%d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Development QR Codes") || !strings.Contains(body, "href=\"/scan/cmd-") || !strings.Contains(body, "Click any QR card") || !strings.Contains(body, "api.qrserver.com/v1/create-qr-code") || !strings.Contains(body, "Print matrix") {
		t.Fatalf("dev QR page did not include clickable scan links and QR images: %s", body)
	}
	if strings.Contains(body, "qr-url") || strings.Contains(body, "</span></a>") {
		t.Fatalf("dev QR page should not render URL captions: %s", body)
	}
}

func TestPreferredLanguageUsesAcceptLanguage(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("Accept-Language", "de-DE,de;q=0.9,en;q=0.8")
	if got := preferredLang(req); got != "de" {
		t.Fatalf("preferredLang=%q, want de", got)
	}
}

func TestSettingsPersistsLanguagePreference(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=user%40example.com&password=password"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}
	cookies := loginRec.Result().Cookies()

	var csrf string
	if err := a.db.QueryRow("select csrf from sessions where user_id='user_dev'").Scan(&csrf); err != nil {
		t.Fatal(err)
	}
	form := "csrf=" + csrf + "&language=de"
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("settings status=%d, want %d", rec.Code, http.StatusSeeOther)
	}
	var lang string
	if err := a.db.QueryRow("select language from users where id='user_dev'").Scan(&lang); err != nil {
		t.Fatal(err)
	}
	if lang != "de" {
		t.Fatalf("language=%q, want de", lang)
	}
}

func TestGermanHelpersTranslateUnitsAndEventText(t *testing.T) {
	ctx := Ctx{Lang: "de"}
	if got := unitLabel(ctx, "units"); got != "Einheiten" {
		t.Fatalf("unitLabel=%q, want Einheiten", got)
	}
	if got := eventText(ctx, "add", 10, "Lemon", "units"); got != "10 Einheiten Lemon hinzugefügt" {
		t.Fatalf("eventText add=%q", got)
	}
	if got := eventText(ctx, "subtract", -5, "Lime", "units"); got != "5 Einheiten Lime entnommen" {
		t.Fatalf("eventText subtract=%q", got)
	}
}
