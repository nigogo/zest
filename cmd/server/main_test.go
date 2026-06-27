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

func TestAdminQRCodesPageLinksToScanRoutes(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
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

	req := httptest.NewRequest(http.MethodGet, "/admin/qr-codes", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin qr status=%d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Development QR Codes") || !strings.Contains(body, "href=\"/scan/cmd-") || !strings.Contains(body, "Click any QR card") || !strings.Contains(body, "api.qrserver.com/v1/create-qr-code") || !strings.Contains(body, "Print matrix") || !strings.Contains(body, `class="admin-nav"`) {
		t.Fatalf("admin QR page did not include admin navigation, clickable scan links, and QR images: %s", body)
	}
	if strings.Contains(body, "qr-url") || strings.Contains(body, "</span></a>") {
		t.Fatalf("admin QR page should not render URL captions: %s", body)
	}
}

func TestScanHomeRendersCameraScanner(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/scan", nil)
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status=%d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Scan QR code", `id="qr-video"`, "visualViewport", "--scan-vh", "jsQR", `id="camera-start"`, "/static/qr_scanner.js"} {
		if !strings.Contains(body, want) {
			t.Fatalf("scan page missing %q: %s", want, body)
		}
	}
	if rec.Result().Header.Get("Location") == "/dev/qr-codes" {
		t.Fatalf("scan page should render scanner instead of redirecting to dev QR codes")
	}
	for _, unwanted := range []string{"Paste scan link manually", "Show dev QRs", `data-scan-manual`, `bottom-action-bar`, `id="qr-upload"`, `capture="environment"`, "Upload QR image"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("scan page should not include %q: %s", unwanted, body)
		}
	}
}

func TestQRScannerScriptDefinesUnifiedLifecycle(t *testing.T) {
	body, err := os.ReadFile("../../static/qr_scanner.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		"function createQRCodeScanner",
		"startScanner",
		"stopScanner",
		"onScanSuccess",
		"onScanError",
		"navigator.mediaDevices.getUserMedia",
		`facingMode: { ideal: "environment" }`,
		"stream.getTracks().forEach",
		"video.playsInline = true",
		"decodeImageFile",
		"BarcodeDetector",
		"new BarcodeDetector",
		"barcodeDetector.detect(video)",
		"jsQR(ctx.getImageData",
		"if (completed) return",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("scanner script missing %q", want)
		}
	}
	if strings.Contains(script, "BarcodeScanner") {
		t.Fatalf("scanner should not use BarcodeScanner")
	}
}

func TestStaticJavaScriptServedWithJavaScriptContentType(t *testing.T) {
	a := testApp(t)
	mux := http.NewServeMux()
	a.routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/static/qr_scanner.js", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("static js status=%d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
		t.Fatalf("static js Content-Type=%q, want application/javascript", got)
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
	if got := eventAmountText(ctx, "add", 10, "units"); got != "10 Einheiten hinzugefügt" {
		t.Fatalf("eventAmountText add=%q", got)
	}
}

func TestPlacesPageShowsStockOverviewInsteadOfActiveLabel(t *testing.T) {
	a := testApp(t)
	a.templates()
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type) values('overview1','org_dev','place_0','prod_0','user_admin',40,'add')")
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type) values('overview2','org_dev','place_0','prod_1','user_admin',20,'add')")
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=user%40example.com&password=password"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)

	req := httptest.NewRequest(http.MethodGet, "/places", nil)
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("places status=%d, want %d; body=%s", rec.Code, http.StatusOK, body)
	}
	if strings.Contains(body, `class="pill">Active`) {
		t.Fatalf("places page still shows active label: %s", body)
	}
	if strings.Contains(body, `class="product-card product-default"`) || !strings.Contains(body, `class="place-card"`) {
		t.Fatalf("places page should use distinct place cards: %s", body)
	}
	if !strings.Contains(body, `class="place-overview-item product-lemon"`) || !strings.Contains(body, "<strong>40</strong> units") {
		t.Fatalf("places page missing organized stock overview: %s", body)
	}
	if !strings.Contains(body, `class="place-overview-item product-lime"`) || !strings.Contains(body, "<strong>20</strong> units") {
		t.Fatalf("places page missing second stock overview item: %s", body)
	}
}

func TestAdminCanSetProductColorAcrossProductMentions(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}

	var csrf string
	if err := a.db.QueryRow("select csrf from sessions where user_id='user_admin'").Scan(&csrf); err != nil {
		t.Fatal(err)
	}
	form := "csrf=" + csrf + "&product_id=prod_0&color=%231234AB"
	req := httptest.NewRequest(http.MethodPost, "/admin/products", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("product color status=%d, want %d; body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}

	var color string
	if err := a.db.QueryRow("select color from products where id='prod_0'").Scan(&color); err != nil {
		t.Fatal(err)
	}
	if color != "#1234AB" {
		t.Fatalf("color=%q, want #1234AB", color)
	}

	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type) values('color1','org_dev','place_0','prod_0','user_admin',10,'add')")
	for _, path := range []string{"/admin/products", "/places", "/events", "/events/color1/result"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for _, cookie := range loginRec.Result().Cookies() {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d, want %d; body=%s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "--product:#1234AB") {
			t.Fatalf("%s missing saved product color: %s", path, rec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/events", nil)
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, `class="event-card product-`) || strings.Contains(body, `<article class="event-card" style=`) {
		t.Fatalf("event card should keep the neutral card style and not carry product coloring: %s", body)
	}
	if !strings.Contains(body, `class="event-product-line"`) || !strings.Contains(body, `class="product-badge product-lemon" style="--product:#1234AB`) {
		t.Fatalf("event product name should be wrapped in a colored product badge: %s", body)
	}
	if !strings.Contains(body, `<p class="event-amount-line">Added 10 units</p>`) {
		t.Fatalf("event amount should be shown on its own second line: %s", body)
	}
}

func TestAdminProductsPageUsesCollapsedListManagement(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/products", nil)
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("products status=%d, want %d; body=%s", rec.Code, http.StatusOK, body)
	}
	if !strings.Contains(body, `class="new-product-menu"`) || !strings.Contains(body, `<summary class="button primary">Add product</summary>`) {
		t.Fatalf("products page should hide the new-product form behind an Add product button: %s", body)
	}
	if !strings.Contains(body, `class="product-list" role="list"`) || !strings.Contains(body, `class="product-list-item `) {
		t.Fatalf("products page should render products as clickable list entries: %s", body)
	}
	if strings.Contains(body, `admin-products-table`) || strings.Contains(body, `<table`) {
		t.Fatalf("products page should not render the management UI as a table: %s", body)
	}
}

func TestAdminCanCreateRenameArchiveAndRestoreProduct(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}
	cookies := loginRec.Result().Cookies()
	var csrf string
	if err := a.db.QueryRow("select csrf from sessions where user_id='user_admin'").Scan(&csrf); err != nil {
		t.Fatal(err)
	}

	form := "csrf=" + csrf + "&action=create&name=Blood+Orange&code=bo-1&unit=cases&color=%23ABCDEF"
	req := httptest.NewRequest(http.MethodPost, "/admin/products", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status=%d, want %d; body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}

	var productID, name, code, unit, color string
	var active int
	if err := a.db.QueryRow("select id,name,code,unit,color,active from products where code='BO-1'").Scan(&productID, &name, &code, &unit, &color, &active); err != nil {
		t.Fatal(err)
	}
	if name != "Blood Orange" || code != "BO-1" || unit != "cases" || color != "#ABCDEF" || active != 1 {
		t.Fatalf("created product mismatch: id=%s name=%q code=%q unit=%q color=%q active=%d", productID, name, code, unit, color, active)
	}
	var qrCount int
	if err := a.db.QueryRow("select count(*) from qr_commands where product_id=? and active=1", productID).Scan(&qrCount); err != nil {
		t.Fatal(err)
	}
	if qrCount != 24 {
		t.Fatalf("new product active qr commands=%d, want 24", qrCount)
	}

	form = "csrf=" + csrf + "&action=save&product_id=" + productID + "&name=Ruby+Orange&code=ruby&unit=boxes&color=%23112233"
	req = httptest.NewRequest(http.MethodPost, "/admin/products", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rename status=%d, want %d; body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if err := a.db.QueryRow("select name,code,unit,color from products where id=?", productID).Scan(&name, &code, &unit, &color); err != nil {
		t.Fatal(err)
	}
	if name != "Ruby Orange" || code != "RUBY" || unit != "boxes" || color != "#112233" {
		t.Fatalf("renamed product mismatch: name=%q code=%q unit=%q color=%q", name, code, unit, color)
	}

	form = "csrf=" + csrf + "&action=archive&product_id=" + productID
	req = httptest.NewRequest(http.MethodPost, "/admin/products", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("archive status=%d, want %d", rec.Code, http.StatusSeeOther)
	}
	if err := a.db.QueryRow("select active from products where id=?", productID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active after archive=%d, want 0", active)
	}
	if err := a.db.QueryRow("select count(*) from qr_commands where product_id=? and active=1", productID).Scan(&qrCount); err != nil {
		t.Fatal(err)
	}
	if qrCount != 0 {
		t.Fatalf("active qr commands after archive=%d, want 0", qrCount)
	}

	form = "csrf=" + csrf + "&action=restore&product_id=" + productID
	req = httptest.NewRequest(http.MethodPost, "/admin/products", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("restore status=%d, want %d", rec.Code, http.StatusSeeOther)
	}
	if err := a.db.QueryRow("select active from products where id=?", productID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active after restore=%d, want 1", active)
	}
	if err := a.db.QueryRow("select count(*) from qr_commands where product_id=? and active=1", productID).Scan(&qrCount); err != nil {
		t.Fatal(err)
	}
	if qrCount != 24 {
		t.Fatalf("active qr commands after restore=%d, want 24", qrCount)
	}
}

func TestAdminPlacesPageUsesCollapsedListManagement(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/places", nil)
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("places admin status=%d, want %d; body=%s", rec.Code, http.StatusOK, body)
	}
	if !strings.Contains(body, `class="new-product-menu"`) || !strings.Contains(body, `<summary class="button primary">Add place</summary>`) {
		t.Fatalf("places page should hide the new-place form behind an Add place button: %s", body)
	}
	if !strings.Contains(body, `class="product-list" role="list"`) || !strings.Contains(body, `class="product-list-item `) {
		t.Fatalf("places page should render places as product-style list entries: %s", body)
	}
	if !strings.Contains(body, `class="admin-place-list-name"`) || !strings.Contains(body, `class="place-icon small"`) {
		t.Fatalf("places page should show compact place icons before names: %s", body)
	}
	if strings.Contains(body, `<table`) || strings.Contains(body, `Token:`) || strings.Contains(body, `product-badge product-default`) {
		t.Fatalf("places page should not render a table, expose tokens, or use product badge labels: %s", body)
	}
}

func TestAdminCanCreateRenameArchiveAndRestorePlace(t *testing.T) {
	a := testApp(t)
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}
	cookies := loginRec.Result().Cookies()
	var csrf string
	if err := a.db.QueryRow("select csrf from sessions where user_id='user_admin'").Scan(&csrf); err != nil {
		t.Fatal(err)
	}

	form := "csrf=" + csrf + "&action=create&name=Back+Room"
	req := httptest.NewRequest(http.MethodPost, "/admin/places", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status=%d, want %d; body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}

	var placeID, name string
	var active int
	if err := a.db.QueryRow("select id,name,active from places where name='Back Room'").Scan(&placeID, &name, &active); err != nil {
		t.Fatal(err)
	}
	if name != "Back Room" || active != 1 {
		t.Fatalf("created place mismatch: id=%s name=%q active=%d", placeID, name, active)
	}
	var qrCount int
	if err := a.db.QueryRow("select count(*) from qr_commands where place_id=? and active=1", placeID).Scan(&qrCount); err != nil {
		t.Fatal(err)
	}
	if qrCount != 32 {
		t.Fatalf("new place active qr commands=%d, want 32", qrCount)
	}

	form = "csrf=" + csrf + "&action=save&place_id=" + placeID + "&name=Front+Room"
	req = httptest.NewRequest(http.MethodPost, "/admin/places", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rename status=%d, want %d; body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if err := a.db.QueryRow("select name from places where id=?", placeID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Front Room" {
		t.Fatalf("renamed place name=%q, want Front Room", name)
	}

	form = "csrf=" + csrf + "&action=archive&place_id=" + placeID
	req = httptest.NewRequest(http.MethodPost, "/admin/places", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("archive status=%d, want %d", rec.Code, http.StatusSeeOther)
	}
	if err := a.db.QueryRow("select active from places where id=?", placeID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active after archive=%d, want 0", active)
	}
	if err := a.db.QueryRow("select count(*) from qr_commands where place_id=? and active=1", placeID).Scan(&qrCount); err != nil {
		t.Fatal(err)
	}
	if qrCount != 0 {
		t.Fatalf("active qr commands after archive=%d, want 0", qrCount)
	}

	form = "csrf=" + csrf + "&action=restore&place_id=" + placeID
	req = httptest.NewRequest(http.MethodPost, "/admin/places", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("restore status=%d, want %d", rec.Code, http.StatusSeeOther)
	}
	if err := a.db.QueryRow("select active from places where id=?", placeID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active after restore=%d, want 1", active)
	}
	if err := a.db.QueryRow("select count(*) from qr_commands where place_id=? and active=1", placeID).Scan(&qrCount); err != nil {
		t.Fatal(err)
	}
	if qrCount != 32 {
		t.Fatalf("active qr commands after restore=%d, want 32", qrCount)
	}
}

func TestAdminPlacesWarnsButAllowsArchiveWithStock(t *testing.T) {
	a := testApp(t)
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type) values('stocked-place','org_dev','place_0','prod_0','user_admin',40,'add')")
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)

	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=admin%40example.com&password=admin123-change-me"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want %d", loginRec.Code, http.StatusFound)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/places", nil)
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "This place contains current stock") || !strings.Contains(body, "Archive anyway?") {
		t.Fatalf("places page should confirm before archiving stocked places: %s", body)
	}
	if strings.Contains(body, `status-warning`) {
		t.Fatalf("places page should only show the stock warning in the archive prompt: %s", body)
	}

	var csrf string
	if err := a.db.QueryRow("select csrf from sessions where user_id='user_admin'").Scan(&csrf); err != nil {
		t.Fatal(err)
	}
	form := "csrf=" + csrf + "&action=archive&place_id=place_0"
	req = httptest.NewRequest(http.MethodPost, "/admin/places", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range loginRec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("archive status=%d, want %d", rec.Code, http.StatusSeeOther)
	}
	var active int
	if err := a.db.QueryRow("select active from places where id='place_0'").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active after archive=%d, want 0", active)
	}
}
