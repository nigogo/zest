package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type App struct {
	db              *sql.DB
	base, addr, env string
	amounts         []int
	undoWindow      time.Duration
	tmpl            *template.Template
	secure          bool
}
type Ctx struct {
	UserID, OrgID, Role, Status, CSRF string
	Authed, AdminArea                 bool
	Lang, LangPref, TimeFormat        string
}
type Page struct {
	Title string
	Ctx   Ctx
	Data  any
	Error string
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func id() string { b := make([]byte, 18); rand.Read(b); return base64.RawURLEncoding.EncodeToString(b) }
func hashpw(p string) string {
	s := sha256.Sum256([]byte("zest-dev-salt:" + p))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	a := &App{base: env("APP_BASE_URL", "http://localhost:8765"), addr: env("APP_ADDR", ":8765"), env: env("APP_ENV", "development"), undoWindow: 20 * time.Second}
	a.secure = a.env == "production"
	for _, s := range strings.Split(env("COMMON_AMOUNTS", "10,20,40,60"), ",") {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		if n > 0 {
			a.amounts = append(a.amounts, n)
		}
	}
	os.MkdirAll(filepath.Dir(env("DATABASE_PATH", "./data/app.db")), 0755)
	db, err := sql.Open("sqlite", env("DATABASE_PATH", "./data/app.db")+"?_pragma=foreign_keys(1)")
	must(err)
	a.db = db
	a.migrate()
	a.seed()
	a.templates()
	mux := http.NewServeMux()
	a.routes(mux)
	log.Println("listening", a.addr)
	must(http.ListenAndServe(a.addr, mux))
}

func (a *App) migrate() {
	b, err := os.ReadFile("migrations/001_init.sql")
	must(err)
	_, err = a.db.Exec(string(b))
	must(err)
	a.ensureColumn("users", "language", "TEXT NOT NULL DEFAULT ''")
	a.ensureColumn("users", "time_format", "TEXT NOT NULL DEFAULT 'local'")
	a.ensureColumn("products", "color", "TEXT NOT NULL DEFAULT ''")
	a.ensurePrintQRCodesTable()
}

func (a *App) ensurePrintQRCodesTable() {
	_, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS print_qr_codes (id TEXT PRIMARY KEY,organization_id TEXT NOT NULL,label TEXT NOT NULL,kind TEXT NOT NULL,place_id TEXT,product_id TEXT,qr_command_id TEXT,position INTEGER NOT NULL DEFAULT 0,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,FOREIGN KEY (organization_id) REFERENCES organizations(id),FOREIGN KEY (place_id) REFERENCES places(id),FOREIGN KEY (product_id) REFERENCES products(id),FOREIGN KEY (qr_command_id) REFERENCES qr_commands(id));CREATE INDEX IF NOT EXISTS idx_print_qr_codes_org ON print_qr_codes(organization_id,position,created_at);`)
	must(err)
}

func (a *App) ensureColumn(table, column, definition string) {
	rows, err := a.db.Query("pragma table_info(" + table + ")")
	if err != nil {
		must(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			must(err)
		}
		if name == column {
			return
		}
	}
	_, err = a.db.Exec("alter table " + table + " add column " + column + " " + definition)
	must(err)
}

func (a *App) seed() {
	var c int
	a.db.QueryRow("select count(*) from organizations").Scan(&c)
	org := "org_dev"
	if c > 0 {
		a.backfillProductColors(org)
		a.ensureSeedApprovedUser(org)
		a.ensureSeedQRCommands(org)
		return
	}
	a.db.Exec("insert into organizations(id,name)values(?,?)", org, "Example Company")
	h := hashpw(env("SEED_ADMIN_PASSWORD", "admin123-change-me"))
	u := "user_admin"
	a.db.Exec("insert into users(id,email,name,password_hash)values(?,?,?,?)", u, env("SEED_ADMIN_EMAIL", "admin@example.com"), "Admin", h)
	a.db.Exec("insert into memberships(id,organization_id,user_id,role,status,approved_at)values(?,?,?,?,?,CURRENT_TIMESTAMP)", "mem_admin", org, u, "admin", "approved")
	a.ensureSeedApprovedUser(org)
	a.db.Exec("insert into organization_invites(id,organization_id,token,active)values(?,?,?,1)", "invite_dev", org, "dev-invite-token")
	places := []string{"Freezer A", "Freezer B", "Freezer C"}
	prods := []string{"Lemon", "Lime", "Orange", "Grapefruit"}
	for i, p := range places {
		a.db.Exec("insert into places(id,organization_id,name,token,active)values(?,?,?,?,1)", fmt.Sprintf("place_%d", i), org, p, "place-"+id())
	}
	for i, p := range prods {
		a.db.Exec("insert into products(id,organization_id,name,code,unit,color,active)values(?,?,?,?,?,?,1)", fmt.Sprintf("prod_%d", i), org, p, fmt.Sprintf("P%d", i+1), "units", defaultProductColor(p))
	}
	a.backfillProductColors(org)
	a.ensureSeedQRCommands(org)
}

func (a *App) backfillProductColors(org string) {
	rows, err := a.db.Query("select id,name from products where organization_id=? and coalesce(color,'')=''", org)
	if err != nil {
		log.Printf("product color query failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var productID, name string
		if err := rows.Scan(&productID, &name); err != nil {
			log.Printf("product color scan failed: %v", err)
			continue
		}
		a.db.Exec("update products set color=? where id=? and organization_id=?", defaultProductColor(name), productID, org)
	}
}

func (a *App) ensureSeedApprovedUser(org string) {
	uid := "user_dev"
	email := env("SEED_USER_EMAIL", "user@example.com")
	h := hashpw(env("SEED_USER_PASSWORD", "password"))
	if _, err := a.db.Exec("insert into users(id,email,name,password_hash)values(?,?,?,?) on conflict(id) do update set email=excluded.email,name=excluded.name,password_hash=excluded.password_hash", uid, email, "Development User", h); err != nil {
		log.Printf("seed approved user failed: %v", err)
		return
	}
	if _, err := a.db.Exec("insert into memberships(id,organization_id,user_id,role,status,approved_at)values(?,?,?,?,?,CURRENT_TIMESTAMP) on conflict(organization_id,user_id) do update set status=excluded.status,approved_at=CURRENT_TIMESTAMP", "mem_dev", org, uid, "operator", "approved"); err != nil {
		log.Printf("seed approved user membership failed: %v", err)
	}
}

func (a *App) ensureSeedQRCommands(org string) {
	if len(a.amounts) == 0 {
		a.amounts = []int{10, 20, 40, 60}
	}
	placeIDs := a.ids("select id from places where organization_id=? order by id", org)
	productIDs := a.ids("select id from products where organization_id=? order by id", org)
	for _, pl := range placeIDs {
		for _, pr := range productIDs {
			for _, act := range []string{"add", "subtract"} {
				for _, amt := range a.amounts {
					if _, err := a.db.Exec("insert or ignore into qr_commands(id,organization_id,place_id,product_id,action,amount,token,active)values(?,?,?,?,?,?,?,1)", id(), org, pl, pr, act, amt, "cmd-"+id()); err != nil {
						log.Printf("seed qr command failed: %v", err)
					}
				}
			}
		}
	}
}

func (a *App) ensureProductQRCommands(org, productID string) {
	if len(a.amounts) == 0 {
		a.amounts = []int{10, 20, 40, 60}
	}
	placeIDs := a.ids("select id from places where organization_id=? and active=1 order by id", org)
	for _, pl := range placeIDs {
		for _, act := range []string{"add", "subtract"} {
			for _, amt := range a.amounts {
				if _, err := a.db.Exec("insert or ignore into qr_commands(id,organization_id,place_id,product_id,action,amount,token,active)values(?,?,?,?,?,?,?,1)", id(), org, pl, productID, act, amt, "cmd-"+id()); err != nil {
					log.Printf("product qr command failed: %v", err)
				}
			}
		}
	}
}

func (a *App) ensurePlaceQRCommands(org, placeID string) {
	if len(a.amounts) == 0 {
		a.amounts = []int{10, 20, 40, 60}
	}
	productIDs := a.ids("select id from products where organization_id=? and active=1 order by id", org)
	for _, pr := range productIDs {
		for _, act := range []string{"add", "subtract"} {
			for _, amt := range a.amounts {
				if _, err := a.db.Exec("insert or ignore into qr_commands(id,organization_id,place_id,product_id,action,amount,token,active)values(?,?,?,?,?,?,?,1)", id(), org, placeID, pr, act, amt, "cmd-"+id()); err != nil {
					log.Printf("place qr command failed: %v", err)
				}
			}
		}
	}
}

func normalizeProductText(v string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
}

func normalizeProductCode(v string) string {
	v = strings.ToUpper(strings.TrimSpace(v))
	v = regexp.MustCompile(`[^A-Z0-9_-]+`).ReplaceAllString(v, "-")
	v = strings.Trim(v, "-")
	return v
}

func (a *App) ids(query string, arg string) []string {
	rows, err := a.db.Query(query, arg)
	if err != nil {
		log.Printf("seed id query failed: %v", err)
		return nil
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Printf("seed id scan failed: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (a *App) templates() {
	a.tmpl = template.Must(template.New("base").Funcs(template.FuncMap{"productClass": productClass, "productStyle": productStyle, "eventSign": eventSign, "eventText": eventText, "eventAmountText": eventAmountText, "unit": unitLabel, "t": tr, "abs": func(n int) int {
		if n < 0 {
			return -n
		}
		return n
	}, "csrf": func(c Ctx) template.HTML {
		return template.HTML(`<input type="hidden" name="csrf" value="` + template.HTMLEscapeString(c.CSRF) + `">`)
	}, "formatTime": formatTime, "timeExample": timeExample}).Parse(tpl))
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, d any) {
	c := a.ctx(r)
	c.AdminArea = strings.HasPrefix(r.URL.Path, "/admin")
	c.Lang = c.LangPref
	if c.Lang == "" {
		c.Lang = preferredLang(r)
	}
	a.tmpl.ExecuteTemplate(w, name, Page{Title: name, Ctx: c, Data: d})
}

func (a *App) ctx(r *http.Request) Ctx {
	ck, err := r.Cookie("sid")
	if err != nil {
		return Ctx{Lang: preferredLang(r)}
	}
	var c Ctx
	c.Authed = true
	err = a.db.QueryRow("select s.user_id,s.csrf,coalesce(m.organization_id,''),coalesce(m.role,''),coalesce(m.status,''),coalesce(u.language,''),coalesce(u.time_format,'local') from sessions s join users u on u.id=s.user_id left join memberships m on m.user_id=s.user_id where s.token=? and s.expires_at>datetime('now') order by m.created_at limit 1", ck.Value).Scan(&c.UserID, &c.CSRF, &c.OrgID, &c.Role, &c.Status, &c.LangPref, &c.TimeFormat)
	if err != nil {
		return Ctx{Lang: preferredLang(r)}
	}
	c.Lang = c.LangPref
	if c.Lang == "" {
		c.Lang = preferredLang(r)
	}
	return c
}

func (a *App) need(w http.ResponseWriter, r *http.Request) (Ctx, bool) {
	c := a.ctx(r)
	if !c.Authed {
		http.Redirect(w, r, "/login?return="+r.URL.RequestURI(), 302)
		return c, false
	}
	return c, true
}

func (a *App) approved(w http.ResponseWriter, r *http.Request) (Ctx, bool) {
	c, ok := a.need(w, r)
	if !ok {
		return c, false
	}
	if c.Status != "approved" {
		a.render(w, r, "error", map[string]string{"Message": tr(c, "error.waiting")})
		return c, false
	}
	return c, true
}

func (a *App) admin(w http.ResponseWriter, r *http.Request) (Ctx, bool) {
	c, ok := a.approved(w, r)
	if !ok {
		return c, false
	}
	if c.Role != "admin" {
		http.Error(w, "forbidden", 403)
		return c, false
	}
	return c, true
}

func (a *App) checkPost(w http.ResponseWriter, r *http.Request, c Ctx) bool {
	if r.Method == "POST" && r.FormValue("csrf") != c.CSRF {
		http.Error(w, "bad csrf", 403)
		return false
	}
	return true
}

func langSupported(lang string) bool {
	return lang == "en" || lang == "de"
}

func preferredLang(r *http.Request) string {
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		code := strings.ToLower(strings.TrimSpace(strings.Split(part, ";")[0]))
		if len(code) >= 2 && langSupported(code[:2]) {
			return code[:2]
		}
	}
	return "en"
}

var messages = map[string]map[string]string{
	"de": germanMessages,
}

var germanMessages = map[string]string{
	"nav.places":              "Orte",
	"nav.events":              "Ereignisse",
	"nav.admin":               "Admin",
	"nav.settings":            "Einstellungen",
	"nav.logout":              "Abmelden",
	"login.tag":               "Scannen. Buchen. Fertig.",
	"login.title":             "Anmelden",
	"login.copy":              "Schnelle Bestandsführung für die Praxis.",
	"login.email":             "E-Mail",
	"login.password":          "Passwort",
	"login.submit":            "Anmelden",
	"login.register":          "Konto erstellen",
	"register.tag":            "Team beitreten",
	"register.title":          "Konto erstellen",
	"register.name":           "Name",
	"register.submit":         "Konto erstellen",
	"waiting.eyebrow":         "Warten auf Freigabe",
	"waiting.title":           "Warten auf Freigabe",
	"waiting.added":           "Dein Konto wurde zu Example Company hinzugefügt.",
	"waiting.copy":            "Ein Admin muss dich freigeben, bevor du Bestand ändern kannst.",
	"waiting.switch":          "Anderes Konto verwenden",
	"error.eyebrow":           "Zest kann nicht fortfahren",
	"error.home":              "Zur Startseite",
	"error.login":             "Anmelden",
	"error.waiting":           "Dein Konto wartet auf Freigabe oder der Zugriff wurde abgelehnt.",
	"error.invalid_invite":    "Ungültige Einladung.",
	"error.invalid_qr":        "Dieser QR-Code ist ungültig oder nicht mehr aktiv.",
	"error.inactive_qr":       "Dieser QR-Code verweist auf einen inaktiven Ort oder ein inaktives Produkt.",
	"action.add":              "Hinzufügen",
	"action.subtract":         "Entnehmen",
	"nav.scan":                "Scannen",
	"nav.manual":              "Manuell",
	"manual.eyebrow":          "Manuelle Bewegung",
	"manual.title":            "Bestand buchen",
	"manual.copy":             "Wähle Ort, Produkt und Menge. Dann hinzufügen oder entnehmen.",
	"manual.place":            "Ort",
	"manual.product":          "Produkt",
	"manual.amount":           "Menge",
	"manual.add":              "Hinzufügen",
	"manual.subtract":         "Entnehmen",
	"manual.invalid":          "Bitte wähle einen aktiven Ort, ein aktives Produkt und eine positive Menge.",
	"action.add_section":      "HINZUFÜGEN",
	"action.subtract_section": "ENTNEHMEN",
	"settings.title":          "Einstellungen",
	"settings.language":       "Sprache",
	"settings.time_format":    "Zeitformat",
	"settings.time_local":     "Lokal (Gerätestandard)",
	"settings.time_iso":       "ISO 8601",
	"settings.time_us":        "USA (Monat/Tag/Jahr)",
	"settings.time_eu":        "Europa (Tag.Monat.Jahr)",
	"settings.time_24h":       "24-Stunden kurz",
	"settings.device":         "Gerätesprache",
	"settings.english":        "Englisch",
	"settings.german":         "Deutsch",
	"settings.save":           "Einstellungen speichern",
	"settings.saved":          "Einstellungen gespeichert.",
	"settings.copy":           "Wähle Sprache und Zeitformat oder verwende die Standardsprache deines Geräts.",
	"places.eyebrow":          "Operator-Orte",
	"places.title":            "Orte",
	"places.copy":             "Öffne einen Ort, scanne einen Befehls-QR-Code und arbeite weiter.",
	"places.devqr":            "Dev-QRs",
	"places.active":           "Aktiv",
	"places.cardcopy":         "Aktuellen Bestand und letzte Bewegungen ansehen.",
	"places.empty":            "Noch keine aktiven Orte.",
	"places.admin_copy":       "Orte hinzufügen, umbenennen und archivieren. Archivierte Orte bleiben im Audit erhalten, verschwinden aber aus aktiven Workflows.",
	"places.add":              "Ort hinzufügen",
	"places.name":             "Name",
	"places.token":            "Token",
	"places.status":           "Status",
	"places.archived":         "Archiviert",
	"places.save":             "Speichern",
	"places.archive":          "Archivieren",
	"places.restore":          "Wiederherstellen",
	"places.archive_confirm":  "Dieser Ort enthält aktuellen Bestand. Bitte verteile Produkte zuerst auf andere Orte. Trotzdem archivieren?",
	"place.eyebrow":           "Aktiver Ort",
	"place.stock":             "Aktueller Bestand je Produkt.",
	"place.approved":          "Freigegeben",
	"place.here":              "hier",
	"place.zero":              "Nullbestand.",
	"place.negative":          "Negativer Bestand. Prüfe die letzte Bewegung.",
	"place.recent":            "Letzte Aktivität",
	"place.viewall":           "Alle anzeigen",
	"place.recentempty":       "Letzte Bewegungen erscheinen nach Scans.",
	"place.scannext":          "Weiter scannen",
	"events.eyebrow":          "Audit-Zeitleiste",
	"events.title":            "Letzte Ereignisse",
	"events.empty":            "Noch keine Ereignisse.",
	"events.added":            "Hinzugefügt",
	"events.subtracted":       "Entnommen",
	"events.reversal":         "Storno von",
	"admin.backoffice":        "Backoffice",
	"admin.title":             "Admin-Dashboard",
	"admin.approvals":         "Ausstehende Freigaben",
	"admin.products":          "Produkte",
	"admin.reports":           "Berichte",
	"admin.settings":          "Einstellungen",
	"admin.total":             "Gesamtbestand",
	"admin.today":             "Ereignisse heute",
	"admin.negative":          "Negativer Bestand",
	"admin.activeplaces":      "Aktive Orte",
	"admin.activeproducts":    "Aktive Produkte",
	"admin.reports_settings":  "Berichte und Einstellungen",
	"admin.coming_soon":       "Erweiterte Diagramme, Warnungen und Konfiguration folgen demnächst.",
	"members.user":            "Benutzer",
	"members.status":          "Status",
	"members.actions":         "Aktionen",
	"members.approve":         "Freigeben",
	"members.reject":          "Ablehnen",
	"members.none":            "Keine ausstehenden Freigaben.",
	"products.empty":          "Noch keine Produkte konfiguriert.",
	"products.copy":           "Produkte hinzufügen, umbenennen und archivieren. Archivierte Produkte bleiben im Audit erhalten, verschwinden aber aus aktiven Workflows.",
	"products.add":            "Produkt hinzufügen",
	"products.name":           "Name",
	"products.code":           "Code",
	"products.unit":           "Einheit",
	"products.color":          "Farbe",
	"products.status":         "Status",
	"products.active":         "Aktiv",
	"products.archived":       "Archiviert",
	"products.save":           "Speichern",
	"products.archive":        "Archivieren",
	"products.restore":        "Wiederherstellen",
	"printqr.title":           "Druck-QR-Codes",
	"printqr.copy":            "Stelle eine kleine Auswahl von QR-Codes für Ausdrucke zusammen.",
	"printqr.add":             "QR-Code hinzufügen",
	"printqr.kind":            "Ziel",
	"printqr.open_home":       "Hauptseite öffnen",
	"printqr.command":         "Bestand buchen",
	"printqr.label":           "Label",
	"printqr.delete":          "Löschen",
	"printqr.print":           "Druckmatrix",
	"printqr.empty":           "Noch keine Druck-QR-Codes vorbereitet.",
	"qr.matrices":             "QR-Matrizen",
	"qr.matrix":               "QR-Befehlsmatrix",
	"qr.print_matrix":         "Matrix drucken",
	"qr.generated":            "Für den laminierten Betriebseinsatz generiert.",
	"qr.print":                "Drucken",
	"qr.backup":               "Backup-Ortslink",
	"qr.backup_copy":          "Öffne die Ortsseite, wenn ein Befehlslabel beschädigt oder unklar ist.",
	"qr.open_place":           "Ortsseite öffnen",
	"devqr.matrix":            "Entwicklungs-QR-Matrix",
	"devqr.title":             "Entwicklungs-QR-Codes",
	"devqr.copy":              "Klicke auf eine QR-Karte, um einen Scan zu simulieren. Drucke diese Seite, um echte Smartphone-Kamera-Scans zu testen.",
	"reports.title":           "Bewegungsberichte",
	"reports.export":          "CSV exportieren",
	"reports.date_range":      "Datumsbereich",
	"reports.last_7_days":     "Letzte 7 Tage",
	"reports.product":         "Produkt",
	"reports.place":           "Ort",
	"reports.all_products":    "Alle Produkte",
	"reports.all_places":      "Alle Orte",
	"reports.chart":           "Diagramm-Platzhalter · Demnächst",
	"reports.preview":         "Exportvorschau",
	"reports.empty":           "Noch keine Berichtsdaten.",
	"unit.units":              "Einheiten",
	"result.current":          "Aktueller Bestand hier",
	"result.undoq":            "Diesen Scan korrigieren?",
	"result.undocopy":         "Rückgängig erstellt ein Stornoereignis. Der Audit-Trail bleibt erhalten.",
	"result.viewplaces":       "Orte anzeigen",
	"result.created":          "Erstellt",
	"result.warning":          "Warnung:",
	"result.negative":         "Bestand ist jetzt negativ:",
	"result.to":               "zu",
	"result.from":             "von",
	"result.added":            "HINZUGEFÜGT",
	"result.subtracted":       "ENTNOMMEN",
	"result.event":            "EREIGNIS",
	"result.undo":             "Rückgängig",
	"result.undo_expired":     "Rückgängig-Zeitfenster abgelaufen",
	"result.undone":           "Rückgängig gemacht.",
	"result.undone_copy":      "Ein Stornoereignis wurde erstellt.",
	"result.scan_copy":        "Scanne den nächsten QR-Code mit deiner Handykamera.",
	"result.scan_later":       "In-App-Scannen kann später aktiviert werden.",
}

var englishMessages = map[string]string{
	"nav.places":              "Places",
	"nav.events":              "Events",
	"nav.admin":               "Admin",
	"nav.settings":            "Settings",
	"nav.logout":              "Logout",
	"login.tag":               "Scan. Move. Done.",
	"login.title":             "Log in",
	"login.copy":              "Fast inventory for real-world work.",
	"login.email":             "Email",
	"login.password":          "Password",
	"login.submit":            "Log in",
	"login.register":          "Create an account",
	"register.tag":            "Join your team",
	"register.title":          "Create account",
	"register.name":           "Name",
	"register.submit":         "Create account",
	"waiting.eyebrow":         "Waiting for approval",
	"waiting.title":           "Waiting for approval",
	"waiting.added":           "Your account has been added to Example Company.",
	"waiting.copy":            "An admin must approve you before you can change inventory.",
	"waiting.switch":          "Use another account",
	"error.eyebrow":           "Zest cannot continue",
	"error.home":              "Go home",
	"error.login":             "Log in",
	"error.waiting":           "Your account is waiting for approval or access was rejected.",
	"error.invalid_invite":    "Invalid invite.",
	"error.invalid_qr":        "This QR code is invalid or no longer active.",
	"error.inactive_qr":       "This QR code refers to an inactive place or product.",
	"action.add":              "Add",
	"action.subtract":         "Subtract",
	"nav.scan":                "Scan",
	"nav.manual":              "Manual",
	"manual.eyebrow":          "Manual movement",
	"manual.title":            "Move stock",
	"manual.copy":             "Choose a place, product, and quantity. Then add or subtract.",
	"manual.place":            "Place",
	"manual.product":          "Product",
	"manual.amount":           "Amount",
	"manual.add":              "Add",
	"manual.subtract":         "Subtract",
	"manual.invalid":          "Choose an active place, active product, and positive amount.",
	"action.add_section":      "ADD",
	"action.subtract_section": "SUBTRACT",
	"settings.title":          "Settings",
	"settings.language":       "Language",
	"settings.time_format":    "Time format",
	"settings.time_local":     "Local (device default)",
	"settings.time_iso":       "ISO 8601",
	"settings.time_us":        "US (month/day/year)",
	"settings.time_eu":        "Europe (day.month.year)",
	"settings.time_24h":       "Short 24-hour",
	"settings.device":         "Device default",
	"settings.english":        "English",
	"settings.german":         "German",
	"settings.save":           "Save settings",
	"settings.saved":          "Settings saved.",
	"settings.copy":           "Choose a language and time format, or use your device defaults.",
	"places.eyebrow":          "Operator places",
	"places.title":            "Places",
	"places.copy":             "Open a place, scan a command QR, and keep moving.",
	"places.devqr":            "Dev QRs",
	"places.active":           "Active",
	"places.cardcopy":         "View current stock and recent movement.",
	"places.empty":            "No active places yet.",
	"places.admin_copy":       "Add, rename, and archive places. Archived places stay in the audit trail but disappear from active scanning workflows.",
	"places.add":              "Add place",
	"places.name":             "Name",
	"places.token":            "Token",
	"places.status":           "Status",
	"places.archived":         "Archived",
	"places.save":             "Save",
	"places.archive":          "Archive",
	"places.restore":          "Restore",
	"places.archive_confirm":  "This place contains current stock. Please distribute products to other places first. Archive anyway?",
	"place.eyebrow":           "Active place",
	"place.stock":             "Current stock by product.",
	"place.approved":          "Approved",
	"place.here":              "here",
	"place.zero":              "Zero stock.",
	"place.negative":          "Negative stock. Check the last movement.",
	"place.recent":            "Recent activity",
	"place.viewall":           "View all",
	"place.recentempty":       "Recent movement appears after scans.",
	"place.scannext":          "Scan next",
	"events.eyebrow":          "Audit timeline",
	"events.title":            "Recent events",
	"events.empty":            "No events yet.",
	"events.added":            "Added",
	"events.subtracted":       "Subtracted",
	"events.reversal":         "Reversal of",
	"admin.backoffice":        "Backoffice",
	"admin.title":             "Admin dashboard",
	"admin.approvals":         "Pending approvals",
	"admin.products":          "Products",
	"admin.reports":           "Reports",
	"admin.settings":          "Settings",
	"admin.total":             "Total stock",
	"admin.today":             "Events today",
	"admin.negative":          "Negative stock",
	"admin.activeplaces":      "Active places",
	"admin.activeproducts":    "Active products",
	"admin.reports_settings":  "Reports and settings",
	"admin.coming_soon":       "Advanced charts, alerts, and configuration are coming soon.",
	"members.user":            "User",
	"members.status":          "Status",
	"members.actions":         "Actions",
	"members.approve":         "Approve",
	"members.reject":          "Reject",
	"members.none":            "No pending approvals.",
	"products.empty":          "No products configured yet.",
	"products.copy":           "Add, rename, and archive products. Archived products stay in the audit trail but disappear from active scanning workflows.",
	"products.add":            "Add product",
	"products.name":           "Name",
	"products.code":           "Code",
	"products.unit":           "Unit",
	"products.color":          "Color",
	"products.status":         "Status",
	"products.active":         "Active",
	"products.archived":       "Archived",
	"products.save":           "Save",
	"products.archive":        "Archive",
	"products.restore":        "Restore",
	"printqr.title":           "Print QR codes",
	"printqr.copy":            "Prepare a small curated set of QR codes intended for printing.",
	"printqr.add":             "Add QR code",
	"printqr.kind":            "Target",
	"printqr.open_home":       "Open main page",
	"printqr.command":         "Book stock",
	"printqr.label":           "Label",
	"printqr.delete":          "Delete",
	"printqr.print":           "Print matrix",
	"printqr.empty":           "No print QR codes prepared yet.",
	"qr.matrices":             "QR matrices",
	"qr.matrix":               "QR command matrix",
	"qr.print_matrix":         "Print matrix",
	"qr.generated":            "Generated for laminated operational use.",
	"qr.print":                "Print",
	"qr.backup":               "Backup place link",
	"qr.backup_copy":          "Open the place page if a command label is damaged or unclear.",
	"qr.open_place":           "Open place page",
	"devqr.matrix":            "Development QR matrix",
	"devqr.title":             "Development QR Codes",
	"devqr.copy":              "Click any QR card to simulate scanning. Print this page to test real phone-camera QR scans.",
	"reports.title":           "Movement reports",
	"reports.export":          "Export CSV",
	"reports.date_range":      "Date range",
	"reports.last_7_days":     "Last 7 days",
	"reports.product":         "Product",
	"reports.place":           "Place",
	"reports.all_products":    "All products",
	"reports.all_places":      "All places",
	"reports.chart":           "Chart placeholder · Coming soon",
	"reports.preview":         "Event export preview",
	"reports.empty":           "No report data yet.",
	"unit.units":              "units",
	"result.current":          "Current stock here",
	"result.undoq":            "Need to correct this scan?",
	"result.undocopy":         "Undo creates a reversal event. The audit trail stays intact.",
	"result.viewplaces":       "View places",
	"result.created":          "Created",
	"result.warning":          "Warning:",
	"result.negative":         "stock is now negative:",
	"result.to":               "to",
	"result.from":             "from",
	"result.added":            "ADDED",
	"result.subtracted":       "SUBTRACTED",
	"result.event":            "EVENT",
	"result.undo":             "Undo",
	"result.undo_expired":     "Undo window expired",
	"result.undone":           "Undone.",
	"result.undone_copy":      "A reversal event was created.",
	"result.scan_copy":        "Use your phone camera to scan the next QR code.",
	"result.scan_later":       "In-app scanning can be enabled later.",
}

func tr(c Ctx, key string) string {
	if m, ok := messages[c.Lang]; ok {
		if v := m[key]; v != "" {
			return v
		}
	}
	return englishMessages[key]
}

func actionLabel(c Ctx, action string) string {
	if action == "add" {
		return tr(c, "action.add")
	}
	if action == "subtract" {
		return tr(c, "action.subtract")
	}
	return action
}

func timeFormatSupported(format string) bool {
	switch format {
	case "local", "iso", "us", "eu", "24h":
		return true
	default:
		return false
	}
}

func formatTime(c Ctx, value any) string {
	var parsed time.Time
	switch v := value.(type) {
	case time.Time:
		parsed = v.Local()
	default:
		raw := strings.TrimSpace(fmt.Sprint(value))
		if raw == "" || raw == "<nil>" {
			return ""
		}
		layouts := []string{"2006-01-02 15:04:05", time.RFC3339Nano, "2006-01-02T15:04:05"}
		for _, layout := range layouts {
			if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
				parsed = t.Local()
				break
			}
		}
		if parsed.IsZero() {
			return raw
		}
	}
	if parsed.IsZero() {
		return ""
	}
	switch c.TimeFormat {
	case "iso":
		return parsed.Format("2006-01-02 15:04:05")
	case "us":
		return parsed.Format("Jan 2, 2006 3:04 PM")
	case "eu":
		return parsed.Format("02.01.2006 15:04")
	case "24h":
		return parsed.Format("2006-01-02 15:04")
	default:
		if c.Lang == "de" {
			return parsed.Format("02.01.2006 15:04")
		}
		return parsed.Format("Jan 2, 2006 3:04 PM")
	}
}

func timeExample(c Ctx, format string) string {
	return formatTime(Ctx{Lang: c.Lang, TimeFormat: format}, time.Now())
}

func unitLabel(c Ctx, u string) string {
	if u == "units" {
		return tr(c, "unit.units")
	}
	return u
}

func eventText(c Ctx, typ string, qty int, product, unit string) string {
	amount := fmt.Sprintf("%d %s %s", abs(qty), unitLabel(c, unit), product)
	if c.Lang == "de" {
		switch {
		case typ == "reversal":
			return "Storno von " + amount
		case qty > 0:
			return amount + " hinzugefügt"
		default:
			return amount + " entnommen"
		}
	}
	switch {
	case typ == "reversal":
		return "Reversal of " + amount
	case qty > 0:
		return "Added " + amount
	default:
		return "Subtracted " + amount
	}
}

func eventAmountText(c Ctx, typ string, qty int, unit string) string {
	amount := fmt.Sprintf("%d %s", abs(qty), unitLabel(c, unit))
	if c.Lang == "de" {
		if typ == "reversal" {
			return "Storno von " + amount
		}
		if qty > 0 {
			return amount + " hinzugefügt"
		}
		return amount + " entnommen"
	}
	if typ == "reversal" {
		return "Reversal of " + amount
	}
	if qty > 0 {
		return "Added " + amount
	}
	return "Subtracted " + amount
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	c, ok := a.need(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		if !a.checkPost(w, r, c) {
			return
		}
		lang := r.FormValue("language")
		if lang != "" && !langSupported(lang) {
			http.Error(w, "unsupported language", 400)
			return
		}
		tf := r.FormValue("time_format")
		if tf == "" {
			tf = "local"
		}
		if !timeFormatSupported(tf) {
			http.Error(w, "unsupported time format", 400)
			return
		}
		a.db.Exec("update users set language=?,time_format=? where id=?", lang, tf, c.UserID)
		http.Redirect(w, r, "/settings?saved=1", 303)
		return
	}
	a.render(w, r, "settings", map[string]any{"Saved": r.URL.Query().Get("saved") == "1"})
}

func (a *App) routes(m *http.ServeMux) {
	m.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/places", 302) })
	m.HandleFunc("/login", a.login)
	m.HandleFunc("/register", a.register)
	m.HandleFunc("/logout", a.logout)
	m.HandleFunc("/org/join/", a.join)
	m.HandleFunc("/scan/", a.scan)
	m.HandleFunc("/scan", a.scanHome)
	m.HandleFunc("/manual", a.manual)
	m.HandleFunc("/dev/qr-codes", a.devQRCodes)
	m.HandleFunc("/events/", a.eventRoutes)
	m.HandleFunc("/events", a.events)
	m.HandleFunc("/places/", a.placeToken)
	m.HandleFunc("/places", a.places)
	m.HandleFunc("/reports/export.csv", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/reports/export.csv", 301) })
	m.HandleFunc("/reports", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/reports", 301) })
	m.HandleFunc("/settings", a.settings)
	m.HandleFunc("/admin/", a.adminRoutes)
	m.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.admin(w, r); ok {
			a.render(w, r, "admin", nil)
		}
	})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.render(w, r, "login", r.URL.Query().Get("return"))
		return
	}
	email, pw := r.FormValue("email"), r.FormValue("password")
	var uid, hash string
	if a.db.QueryRow("select id,password_hash from users where email=?", email).Scan(&uid, &hash) != nil || hashpw(pw) != hash {
		a.render(w, r, "login", "bad login")
		return
	}
	tok, csrf := id(), id()
	a.db.Exec("insert into sessions(token,user_id,csrf,expires_at)values(?,?,?,datetime('now','+7 days'))", tok, uid, csrf)
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: tok, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
	ret := r.FormValue("return")
	if ret == "" {
		ret = "/places"
	}
	http.Redirect(w, r, ret, 302)
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.render(w, r, "register", nil)
		return
	}
	h := hashpw(r.FormValue("password"))
	uid := id()
	_, err := a.db.Exec("insert into users(id,email,name,password_hash)values(?,?,?,?)", uid, r.FormValue("email"), r.FormValue("name"), h)
	if err != nil {
		a.render(w, r, "register", err.Error())
		return
	}
	tok, csrf := id(), id()
	a.db.Exec("insert into sessions(token,user_id,csrf,expires_at)values(?,?,?,datetime('now','+7 days'))", tok, uid, csrf)
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: tok, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/places", 302)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := a.need(w, r)
	if !a.checkPost(w, r, c) {
		return
	}
	if ck, err := r.Cookie("sid"); err == nil {
		a.db.Exec("delete from sessions where token=?", ck.Value)
	}
	http.Redirect(w, r, "/login", 302)
}

func (a *App) join(w http.ResponseWriter, r *http.Request) {
	c, ok := a.need(w, r)
	if !ok {
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/org/join/")
	var org string
	if a.db.QueryRow("select organization_id from organization_invites where token=? and active=1", token).Scan(&org) != nil {
		a.render(w, r, "error", map[string]string{"Message": tr(c, "error.invalid_invite")})
		return
	}
	a.db.Exec("insert or ignore into memberships(id,organization_id,user_id,role,status)values(?,?,?,?,?)", id(), org, c.UserID, "operator", "pending")
	a.render(w, r, "waiting", nil)
}

func (a *App) scan(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/scan/")
	var qid, org, pl, pr, act, pn, place string
	var amt, pactive, practive, qactive int
	err := a.db.QueryRow(`select q.id,q.organization_id,q.place_id,q.product_id,q.action,q.amount,q.active,p.active,pr.active,p.name,pr.name from qr_commands q join places p on p.id=q.place_id join products pr on pr.id=q.product_id where q.token=?`, token).Scan(&qid, &org, &pl, &pr, &act, &amt, &qactive, &pactive, &practive, &place, &pn)
	if err != nil || qactive == 0 {
		a.render(w, r, "error", map[string]string{"Message": tr(c, "error.invalid_qr")})
		return
	}
	if org != c.OrgID {
		http.Error(w, "forbidden", 403)
		return
	}
	if pactive == 0 || practive == 0 {
		a.render(w, r, "error", map[string]string{"Message": tr(c, "error.inactive_qr")})
		return
	}
	delta := amt
	if act == "subtract" {
		delta = -amt
	}
	eid := id()
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type)values(?,?,?,?,?,?,?)", eid, org, pl, pr, c.UserID, delta, act)
	http.Redirect(w, r, "/events/"+eid+"/result", 303)
}

func (a *App) scanHome(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.approved(w, r); !ok {
		return
	}
	http.Redirect(w, r, "/dev/qr-codes", 302)
}

type manualOption struct {
	ID, Token, Name, Unit, Color string
}

type manualData struct {
	Places        []manualOption
	Products      []manualOption
	SelectedPlace string
	Amounts       []int
}

func (a *App) manual(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" {
		if !a.checkPost(w, r, c) {
			return
		}
		placeID := r.FormValue("place_id")
		productID := r.FormValue("product_id")
		action := r.FormValue("action")
		amount, _ := strconv.Atoi(r.FormValue("amount"))
		if amount <= 0 || (action != "add" && action != "subtract") || !a.activePlaceProduct(c.OrgID, placeID, productID) {
			a.render(w, r, "error", map[string]string{"Message": tr(c, "manual.invalid")})
			return
		}
		delta := amount
		if action == "subtract" {
			delta = -amount
		}
		eid := id()
		if _, err := a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type,note)values(?,?,?,?,?,?,?,?)", eid, c.OrgID, placeID, productID, c.UserID, delta, action, "Manual entry"); err != nil {
			http.Error(w, "could not save movement", 500)
			return
		}
		http.Redirect(w, r, "/events/"+eid+"/result", 303)
		return
	}
	a.render(w, r, "manual", a.manualData(c, r.URL.Query().Get("place"), r.URL.Query().Get("place_token")))
}

func (a *App) activePlace(org, placeID string) bool {
	var n int
	a.db.QueryRow("select count(*) from places where id=? and organization_id=? and active=1", placeID, org).Scan(&n)
	return n == 1
}

func (a *App) activePlaceProduct(org, placeID, productID string) bool {
	var n int
	a.db.QueryRow(`select count(*) from places pl, products pr where pl.id=? and pr.id=? and pl.organization_id=? and pr.organization_id=? and pl.active=1 and pr.active=1`, placeID, productID, org, org).Scan(&n)
	return n == 1
}

func (a *App) manualData(c Ctx, selectedPlace, placeToken string) manualData {
	if placeToken != "" {
		a.db.QueryRow("select id from places where token=? and organization_id=? and active=1", placeToken, c.OrgID).Scan(&selectedPlace)
	}
	d := manualData{SelectedPlace: selectedPlace, Amounts: a.amounts}
	rows, err := a.db.Query("select id,token,name from places where organization_id=? and active=1 order by name", c.OrgID)
	if err != nil {
		return d
	}
	defer rows.Close()
	for rows.Next() {
		var o manualOption
		rows.Scan(&o.ID, &o.Token, &o.Name)
		d.Places = append(d.Places, o)
	}
	prows, err := a.db.Query("select id,name,unit,coalesce(color,'') from products where organization_id=? and active=1 order by name", c.OrgID)
	if err != nil {
		return d
	}
	defer prows.Close()
	for prows.Next() {
		var o manualOption
		prows.Scan(&o.ID, &o.Name, &o.Unit, &o.Color)
		d.Products = append(d.Products, o)
	}
	return d
}

func (a *App) eventRoutes(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/undo") {
		a.undo(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/result") {
		a.result(w, r)
		return
	}
	http.NotFound(w, r)
}

func (a *App) eventID(path, suf string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/events/"), suf)
}

func (a *App) result(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	eid := a.eventID(r.URL.Path, "/result")
	row := a.eventData(eid, c.OrgID)
	if row == nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "result", row)
}

func (a *App) eventData(eid, org string) map[string]any {
	var place, prod, unit, color, etype, uid, created string
	var delta, stock, rev int
	err := a.db.QueryRow(`select pl.name,pr.name,pr.unit,coalesce(pr.color,''),e.event_type,e.user_id,e.created_at,e.quantity_delta,(select coalesce(sum(quantity_delta),0) from inventory_events where place_id=e.place_id and product_id=e.product_id),(select count(*) from inventory_events where reversed_event_id=e.id) from inventory_events e join places pl on pl.id=e.place_id join products pr on pr.id=e.product_id where e.id=? and e.organization_id=?`, eid, org).Scan(&place, &prod, &unit, &color, &etype, &uid, &created, &delta, &stock, &rev)
	if err != nil {
		return nil
	}
	return map[string]any{"ID": eid, "Place": place, "Product": prod, "Unit": unit, "Color": color, "Type": etype, "Delta": delta, "Amount": abs(delta), "Stock": stock, "Reversed": rev > 0, "UndoSeconds": int(a.undoWindow.Seconds()), "Created": created, "Negative": stock < 0}
}

func productClass(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "lemon"):
		return "product-lemon"
	case strings.Contains(n, "lime"):
		return "product-lime"
	case strings.Contains(n, "orange"):
		return "product-orange"
	case strings.Contains(n, "grapefruit"):
		return "product-grapefruit"
	default:
		return "product-default"
	}
}

var productColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func defaultProductColor(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "lemon"):
		return "#f4c430"
	case strings.Contains(n, "lime"):
		return "#75b843"
	case strings.Contains(n, "orange"):
		return "#f28c28"
	case strings.Contains(n, "grapefruit"):
		return "#e85d75"
	default:
		return "#f5b700"
	}
}

func normalizeProductColor(color string) string {
	color = strings.TrimSpace(color)
	if color == "" {
		return ""
	}
	if productColorPattern.MatchString(color) {
		return strings.ToUpper(color)
	}
	return ""
}

func productStyle(color string) template.CSS {
	color = normalizeProductColor(color)
	if color == "" {
		return ""
	}
	return template.CSS("--product:" + color + ";--product-soft:color-mix(in srgb," + color + ",white 84%)")
}

func eventSign(q int, typ string) string {
	if typ == "reversal" {
		return "↺"
	}
	if q > 0 {
		return fmt.Sprintf("+%d", q)
	}
	return fmt.Sprintf("%d", q)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (a *App) undo(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	if !a.checkPost(w, r, c) {
		return
	}
	eid := a.eventID(r.URL.Path, "/undo")
	var org, pl, pr, uid string
	var delta int
	var created time.Time
	err := a.db.QueryRow("select organization_id,place_id,product_id,user_id,quantity_delta,created_at from inventory_events where id=? and organization_id=?", eid, c.OrgID).Scan(&org, &pl, &pr, &uid, &delta, &created)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	if c.Role != "admin" && uid != c.UserID {
		http.Error(w, "forbidden", 403)
		return
	}
	if time.Since(created) > a.undoWindow {
		http.Error(w, "undo window expired", 400)
		return
	}
	var cnt int
	a.db.QueryRow("select count(*) from inventory_events where reversed_event_id=?", eid).Scan(&cnt)
	if cnt > 0 {
		http.Error(w, "already undone", 400)
		return
	}
	a.db.Exec("insert into inventory_events(id,organization_id,place_id,product_id,user_id,quantity_delta,event_type,reversed_event_id,note)values(?,?,?,?,?,?,?,?,?)", id(), org, pl, pr, c.UserID, -delta, "reversal", eid, "Undo")
	fmt.Fprint(w, "<div class='success'><strong>Undone</strong><p>A reversal event was created.</p></div>")
}

func (a *App) places(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	rows, _ := a.db.Query("select id,name,token from places where organization_id=? and active=1 order by name", c.OrgID)
	defer rows.Close()
	var v []map[string]any
	for rows.Next() {
		var pid, n, t string
		rows.Scan(&pid, &n, &t)
		v = append(v, map[string]any{"Name": n, "Token": t, "Overview": a.placeStockOverview(c, pid)})
	}
	a.render(w, r, "places", v)
}

func (a *App) placeStockOverview(c Ctx, placeID string) []map[string]any {
	rows, err := a.db.Query(`select p.name,p.unit,coalesce(p.color,''),coalesce(sum(e.quantity_delta),0) as stock from products p left join inventory_events e on e.product_id=p.id and e.place_id=? where p.organization_id=? and p.active=1 group by p.id order by stock desc,p.name limit 3`, placeID, c.OrgID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var overview []map[string]any
	for rows.Next() {
		var name, unit, color string
		var qty int
		rows.Scan(&name, &unit, &color, &qty)
		overview = append(overview, map[string]any{"Name": name, "Unit": unitLabel(c, unit), "Qty": qty, "Color": color})
	}
	return overview
}

func (a *App) placeToken(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/places/")
	var pid, name string
	if a.db.QueryRow("select id,name from places where token=? and organization_id=?", token, c.OrgID).Scan(&pid, &name) != nil {
		http.NotFound(w, r)
		return
	}
	rows, _ := a.db.Query(`select p.name,p.unit,coalesce(p.color,''),coalesce(sum(e.quantity_delta),0) from products p left join inventory_events e on e.product_id=p.id and e.place_id=? where p.organization_id=? and p.active=1 group by p.id order by p.name`, pid, c.OrgID)
	var stocks []map[string]any
	for rows.Next() {
		var n, u, color string
		var q int
		rows.Scan(&n, &u, &color, &q)
		stocks = append(stocks, map[string]any{"Name": n, "Unit": u, "Qty": q, "Color": color})
	}
	rows.Close()
	a.render(w, r, "place", map[string]any{"Name": name, "Token": token, "PlaceID": pid, "Stocks": stocks})
}

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	a.render(w, r, "events", a.listEvents(c.OrgID))
}

func (a *App) listEvents(org string) []map[string]any {
	rows, _ := a.db.Query(`select e.created_at,u.name,e.quantity_delta,pr.name,pr.unit,coalesce(pr.color,''),pl.name,e.event_type from inventory_events e join users u on u.id=e.user_id join products pr on pr.id=e.product_id join places pl on pl.id=e.place_id where e.organization_id=? order by e.created_at desc limit 100`, org)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var t, u, p, unit, color, pl, et string
		var q int
		rows.Scan(&t, &u, &q, &p, &unit, &color, &pl, &et)
		out = append(out, map[string]any{"Time": t, "User": u, "Qty": q, "Product": p, "Unit": unit, "Color": color, "Place": pl, "Type": et})
	}
	return out
}

func (a *App) adminRoutes(w http.ResponseWriter, r *http.Request) {
	c, ok := a.admin(w, r)
	if !ok {
		return
	}
	if !a.checkPost(w, r, c) {
		return
	}
	p := r.URL.Path
	if p == "/admin/memberships" {
		rows, _ := a.db.Query(`select m.id,u.email,u.name,m.role,m.status from memberships m join users u on u.id=m.user_id where m.organization_id=? order by m.created_at desc`, c.OrgID)
		var out []map[string]string
		for rows.Next() {
			var id, e, n, ro, st string
			rows.Scan(&id, &e, &n, &ro, &st)
			out = append(out, map[string]string{"ID": id, "Email": e, "Name": n, "Role": ro, "Status": st})
		}
		a.render(w, r, "members", out)
		return
	}
	if strings.Contains(p, "/approve") {
		a.db.Exec("update memberships set status='approved',approved_at=CURRENT_TIMESTAMP where id=? and organization_id=?", strings.TrimSuffix(strings.TrimPrefix(p, "/admin/memberships/"), "/approve"), c.OrgID)
		http.Redirect(w, r, "/admin/memberships", 303)
		return
	}
	if strings.Contains(p, "/reject") {
		a.db.Exec("update memberships set status='rejected' where id=? and organization_id=?", strings.TrimSuffix(strings.TrimPrefix(p, "/admin/memberships/"), "/reject"), c.OrgID)
		http.Redirect(w, r, "/admin/memberships", 303)
		return
	}
	if p == "/admin/places" {
		if r.Method == "POST" {
			a.adminPlacePost(w, r, c)
			return
		}
		a.adminPlaces(w, r, c)
		return
	}
	if p == "/admin/qr-codes" {
		a.devQRCodes(w, r)
		return
	}
	if p == "/admin/print-qr-codes" {
		if r.Method == "POST" {
			a.adminPrintQRPost(w, r, c)
			return
		}
		a.adminPrintQRCodes(w, r, c)
		return
	}
	if p == "/admin/reports" {
		a.reports(w, r)
		return
	}
	if p == "/admin/reports/export.csv" {
		a.csv(w, r)
		return
	}
	if p == "/admin/products" {
		if r.Method == "POST" {
			a.adminProductPost(w, r, c)
			return
		}
		rows, _ := a.db.Query("select id,name,code,unit,coalesce(color,''),active from products where organization_id=? order by active desc,name", c.OrgID)
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var id, name, code, unit, color string
			var active int
			rows.Scan(&id, &name, &code, &unit, &color, &active)
			if color == "" {
				color = defaultProductColor(name)
			}
			out = append(out, map[string]any{"ID": id, "Name": name, "Code": code, "Unit": unit, "Color": color, "Active": active == 1})
		}
		a.render(w, r, "products", out)
		return
	}
	if strings.HasSuffix(p, "/qr-matrix") {
		a.qrMatrix(w, r, c)
		return
	}
	if p == "/admin/events" {
		a.events(w, r)
		return
	}
	http.NotFound(w, r)
}

func (a *App) adminPlaces(w http.ResponseWriter, r *http.Request, c Ctx) {
	rows, _ := a.db.Query(`select p.id,p.name,p.token,p.active,coalesce(sum(e.quantity_delta),0) from places p left join inventory_events e on e.place_id=p.id and e.organization_id=p.organization_id where p.organization_id=? group by p.id order by p.active desc,p.name`, c.OrgID)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, name, token string
		var active int
		var stock int
		rows.Scan(&id, &name, &token, &active, &stock)
		out = append(out, map[string]any{"ID": id, "Name": name, "Token": token, "Active": active == 1, "HasStock": stock != 0})
	}
	a.render(w, r, "places_admin", out)
}

func (a *App) adminPlacePost(w http.ResponseWriter, r *http.Request, c Ctx) {
	action := r.FormValue("action")
	placeID := r.FormValue("place_id")
	if action == "archive" || action == "restore" {
		active := 0
		if action == "restore" {
			active = 1
		}
		if _, err := a.db.Exec("update places set active=? where id=? and organization_id=?", active, placeID, c.OrgID); err != nil {
			http.Error(w, "could not update place status", 500)
			return
		}
		if action == "archive" {
			a.db.Exec("update qr_commands set active=0 where organization_id=? and place_id=?", c.OrgID, placeID)
		} else {
			a.ensurePlaceQRCommands(c.OrgID, placeID)
			a.db.Exec("update qr_commands set active=1 where organization_id=? and place_id=? and product_id in (select id from products where organization_id=? and active=1)", c.OrgID, placeID, c.OrgID)
		}
		http.Redirect(w, r, "/admin/places", 303)
		return
	}
	name := normalizeProductText(r.FormValue("name"))
	if name == "" {
		http.Error(w, "place name is required", 400)
		return
	}
	if action == "create" || placeID == "" {
		placeID = id()
		if _, err := a.db.Exec("insert into places(id,organization_id,name,token,active)values(?,?,?,?,1)", placeID, c.OrgID, name, "place-"+id()); err != nil {
			http.Error(w, "could not create place", 500)
			return
		}
		a.ensurePlaceQRCommands(c.OrgID, placeID)
		http.Redirect(w, r, "/admin/places", 303)
		return
	}
	if _, err := a.db.Exec("update places set name=? where id=? and organization_id=?", name, placeID, c.OrgID); err != nil {
		http.Error(w, "could not update place", 500)
		return
	}
	http.Redirect(w, r, "/admin/places", 303)
}

func (a *App) adminProductPost(w http.ResponseWriter, r *http.Request, c Ctx) {
	action := r.FormValue("action")
	productID := r.FormValue("product_id")
	if action == "" && productID != "" && r.FormValue("name") == "" && r.FormValue("code") == "" {
		color := normalizeProductColor(r.FormValue("color"))
		if color == "" {
			http.Error(w, "color must be a hex color like #F4C430", 400)
			return
		}
		a.db.Exec("update products set color=? where id=? and organization_id=?", color, productID, c.OrgID)
		http.Redirect(w, r, "/admin/products", 303)
		return
	}
	if action == "archive" || action == "restore" {
		active := 0
		if action == "restore" {
			active = 1
		}
		if _, err := a.db.Exec("update products set active=? where id=? and organization_id=?", active, productID, c.OrgID); err != nil {
			http.Error(w, "could not update product status", 500)
			return
		}
		if action == "archive" {
			a.db.Exec("update qr_commands set active=0 where organization_id=? and product_id=?", c.OrgID, productID)
		} else {
			a.ensureProductQRCommands(c.OrgID, productID)
			a.db.Exec("update qr_commands set active=1 where organization_id=? and product_id=?", c.OrgID, productID)
		}
		http.Redirect(w, r, "/admin/products", 303)
		return
	}
	name := normalizeProductText(r.FormValue("name"))
	code := normalizeProductCode(r.FormValue("code"))
	unit := normalizeProductText(r.FormValue("unit"))
	color := normalizeProductColor(r.FormValue("color"))
	if name == "" || code == "" {
		http.Error(w, "product name and code are required", 400)
		return
	}
	if unit == "" {
		unit = "units"
	}
	if color == "" {
		color = defaultProductColor(name)
	}
	if action == "create" || productID == "" {
		productID = id()
		if _, err := a.db.Exec("insert into products(id,organization_id,name,code,unit,color,active)values(?,?,?,?,?,?,1)", productID, c.OrgID, name, code, unit, color); err != nil {
			http.Error(w, "product code must be unique", 400)
			return
		}
		a.ensureProductQRCommands(c.OrgID, productID)
		http.Redirect(w, r, "/admin/products", 303)
		return
	}
	if _, err := a.db.Exec("update products set name=?,code=?,unit=?,color=? where id=? and organization_id=?", name, code, unit, color, productID, c.OrgID); err != nil {
		http.Error(w, "product code must be unique", 400)
		return
	}
	http.Redirect(w, r, "/admin/products", 303)
}

type printQRData struct {
	Codes    []map[string]any
	Places   []manualOption
	Products []manualOption
	Amounts  []int
}

func (a *App) adminPrintQRCodes(w http.ResponseWriter, r *http.Request, c Ctx) {
	rows, err := a.db.Query(`select pq.id,pq.label,pq.kind,coalesce(pl.name,''),coalesce(pr.name,''),coalesce(pr.color,''),coalesce(q.action,''),coalesce(q.amount,0),coalesce(pl.token,''),coalesce(q.token,'') from print_qr_codes pq left join places pl on pl.id=pq.place_id left join products pr on pr.id=pq.product_id left join qr_commands q on q.id=pq.qr_command_id where pq.organization_id=? order by pq.position,pq.created_at`, c.OrgID)
	if err != nil {
		http.Error(w, "could not load print QR codes", 500)
		return
	}
	defer rows.Close()
	var codes []map[string]any
	for rows.Next() {
		var id, label, kind, place, product, color, action, placeToken, commandToken string
		var amount int
		rows.Scan(&id, &label, &kind, &place, &product, &color, &action, &amount, &placeToken, &commandToken)
		target := strings.TrimRight(a.base, "/") + "/places"
		if kind == "place" && placeToken != "" {
			target = strings.TrimRight(a.base, "/") + "/places/" + placeToken
		} else if kind == "command" && commandToken != "" {
			target = a.scanURL(commandToken)
		}
		if color == "" && kind == "command" {
			color = defaultProductColor(product)
		}
		codes = append(codes, map[string]any{"ID": id, "Label": label, "Kind": kind, "Place": place, "Product": product, "Color": color, "Action": action, "ActionLabel": actionLabel(c, action), "Amount": amount, "Target": target, "Img": a.qrImageURL(target)})
	}
	md := a.manualData(c, "", "")
	a.render(w, r, "printqr", printQRData{Codes: codes, Places: md.Places, Products: md.Products, Amounts: a.amounts})
}

func (a *App) adminPrintQRPost(w http.ResponseWriter, r *http.Request, c Ctx) {
	if !a.checkPost(w, r, c) {
		return
	}
	action := r.FormValue("action")
	if action == "delete" {
		a.db.Exec("delete from print_qr_codes where id=? and organization_id=?", r.FormValue("id"), c.OrgID)
		http.Redirect(w, r, "/admin/print-qr-codes", 303)
		return
	}
	kind := r.FormValue("kind")
	label := normalizeProductText(r.FormValue("label"))
	placeID, productID, commandID := "", "", ""
	if kind == "place" || kind == "command" {
		placeID = r.FormValue("place_id")
		if !a.activePlace(c.OrgID, placeID) {
			http.Error(w, "invalid place", 400)
			return
		}
	}
	if kind == "command" {
		productID = r.FormValue("product_id")
		amount, _ := strconv.Atoi(r.FormValue("amount"))
		act := r.FormValue("command_action")
		if amount <= 0 || (act != "add" && act != "subtract") || !a.activePlaceProduct(c.OrgID, placeID, productID) {
			http.Error(w, "invalid command QR", 400)
			return
		}
		var token string
		err := a.db.QueryRow("select id,token from qr_commands where organization_id=? and place_id=? and product_id=? and action=? and amount=?", c.OrgID, placeID, productID, act, amount).Scan(&commandID, &token)
		if err != nil {
			commandID = id()
			_, err = a.db.Exec("insert into qr_commands(id,organization_id,place_id,product_id,action,amount,token,active)values(?,?,?,?,?,?,?,1)", commandID, c.OrgID, placeID, productID, act, amount, "cmd-"+id())
			if err != nil {
				http.Error(w, "could not create command QR", 500)
				return
			}
		}
	}
	if kind != "home" && kind != "place" && kind != "command" {
		http.Error(w, "invalid QR kind", 400)
		return
	}
	if label == "" {
		label = defaultPrintQRLabel(a.db, c, kind, placeID, productID, commandID)
	}
	_, err := a.db.Exec("insert into print_qr_codes(id,organization_id,label,kind,place_id,product_id,qr_command_id,position)values(?,?,?,?,?,?,?,(select coalesce(max(position),0)+1 from print_qr_codes where organization_id=?))", id(), c.OrgID, label, kind, nullEmpty(placeID), nullEmpty(productID), nullEmpty(commandID), c.OrgID)
	if err != nil {
		http.Error(w, "could not save print QR", 500)
		return
	}
	http.Redirect(w, r, "/admin/print-qr-codes", 303)
}

func nullEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func defaultPrintQRLabel(db *sql.DB, c Ctx, kind, placeID, productID, commandID string) string {
	if kind == "home" {
		return tr(c, "printqr.open_home")
	}
	var place, product, act string
	var amount int
	if kind == "place" {
		db.QueryRow("select name from places where id=?", placeID).Scan(&place)
		return tr(c, "qr.open_place") + ": " + place
	}
	db.QueryRow("select pl.name,pr.name,q.action,q.amount from qr_commands q join places pl on pl.id=q.place_id join products pr on pr.id=q.product_id where q.id=?", commandID).Scan(&place, &product, &act, &amount)
	return fmt.Sprintf("%s %d %s · %s", actionLabel(c, act), amount, product, place)
}

func (a *App) qrImageURL(target string) string {
	return "https://api.qrserver.com/v1/create-qr-code/?size=220x220&margin=12&data=" + url.QueryEscape(target)
}

func (a *App) scanURL(token string) string {
	return strings.TrimRight(a.base, "/") + "/scan/" + token
}

func (a *App) qrMatrix(w http.ResponseWriter, r *http.Request, c Ctx) {
	pid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/places/"), "/qr-matrix")
	var placeToken string
	a.db.QueryRow("select token from places where id=? and organization_id=?", pid, c.OrgID).Scan(&placeToken)
	placeURL := strings.TrimRight(a.base, "/") + "/places/" + placeToken
	rows, _ := a.db.Query(`select q.action,q.amount,pr.name,coalesce(pr.color,''),q.token from qr_commands q join products pr on pr.id=q.product_id where q.organization_id=? and q.place_id=? and q.active=1 and pr.active=1 order by q.action,pr.name,q.amount`, c.OrgID, pid)
	var out []map[string]any
	for rows.Next() {
		var act, prod, color, tok string
		var amt int
		rows.Scan(&act, &amt, &prod, &color, &tok)
		scanURL := a.scanURL(tok)
		out = append(out, map[string]any{"Action": act, "Label": fmt.Sprintf("%s %d %s", actionLabel(c, act), amt, prod), "Href": scanURL, "Img": a.qrImageURL(scanURL), "Color": color, "PlaceURL": placeURL, "PlaceImg": a.qrImageURL(placeURL)})
	}
	if len(out) == 0 && placeToken != "" {
		out = append(out, map[string]any{"PlaceURL": placeURL, "PlaceImg": a.qrImageURL(placeURL)})
	}
	a.render(w, r, "qr", out)
}

func (a *App) devQRCodes(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/dev/") && a.env == "production" {
		http.NotFound(w, r)
		return
	}
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`select pl.name,pr.name,coalesce(pr.color,''),q.action,q.amount,q.token from qr_commands q join places pl on pl.id=q.place_id join products pr on pr.id=q.product_id where q.organization_id=? and q.active=1 and pl.active=1 and pr.active=1 order by pl.name,pr.name,q.action,q.amount`, c.OrgID)
	if err != nil {
		http.Error(w, "could not load QR commands", 500)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var place, prod, color, act, tok string
		var amt int
		rows.Scan(&place, &prod, &color, &act, &amt, &tok)
		href := "/scan/" + tok
		scanURL := a.scanURL(tok)
		out = append(out, map[string]any{"Place": place, "Product": prod, "Color": color, "Action": act, "ActionLabel": actionLabel(c, act), "Amount": amt, "Href": href, "Img": a.qrImageURL(scanURL)})
	}
	a.render(w, r, "devqr", out)
}

func (a *App) reports(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	a.render(w, r, "reports", a.listEvents(c.OrgID))
}

func (a *App) csv(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	wr := csv.NewWriter(w)
	wr.Write([]string{"time", "user", "quantity", "product", "place", "type"})
	for _, e := range a.listEvents(c.OrgID) {
		wr.Write([]string{fmt.Sprint(e["Time"]), fmt.Sprint(e["User"]), fmt.Sprint(e["Qty"]), fmt.Sprint(e["Product"]), fmt.Sprint(e["Place"]), fmt.Sprint(e["Type"])})
	}
	wr.Flush()
}

var _ = context.Background

const tpl = `{{define "layout"}}<!doctype html><html lang="{{.Ctx.Lang}}"><head><title>Zest · {{.Title}}</title><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/static/app.css"><script src="https://unpkg.com/htmx.org@1.9.12"></script><script src="https://unpkg.com/hyperscript.org@0.9.12"></script><script src="https://unpkg.com/lucide@0.468.0/dist/umd/lucide.min.js"></script></head><body><header class="mobile-header"><div><a class="wordmark" href="/places"><img src="/static/zest-logo.png" alt="" width="32" height="32">Zest</a></div><nav class="header-nav" aria-label="Primary"><a class="button ghost icon-button" href="/places" title="{{t .Ctx "nav.places"}}"><i data-lucide="package" aria-hidden="true"></i></a><a class="button ghost icon-button" href="/events" title="{{t .Ctx "nav.events"}}"><i data-lucide="clipboard-list" aria-hidden="true"></i></a><a class="button ghost icon-button" href="/settings" title="{{t .Ctx "nav.settings"}}"><i data-lucide="settings" aria-hidden="true"></i></a>{{if eq .Ctx.Role "admin"}}<a class="button ghost icon-button" href="/admin" title="{{t .Ctx "nav.admin"}}"><i data-lucide="shield" aria-hidden="true"></i></a>{{end}}{{if .Ctx.Authed}}<form method="post" action="/logout">{{csrf .Ctx}}<button class="ghost icon-button" title="{{t .Ctx "nav.logout"}}"><i data-lucide="log-out" aria-hidden="true"></i></button></form>{{end}}</nav></header><main class="app-main {{if .Ctx.AdminArea}}admin-area{{end}}">{{if .Ctx.AdminArea}}<div class="admin-shell"><nav class="admin-nav">{{template "adminNav" .}}</nav><section class="admin-content">{{template "body" .}}</section></div>{{else}}{{template "body" .}}{{end}}</main>{{if and .Ctx.Authed (not .Ctx.AdminArea)}}<nav class="bottom-action-bar" aria-label="{{t .Ctx "nav.scan"}}"><a class="button primary" href="/scan"><i data-lucide="scan-line" aria-hidden="true"></i>{{t .Ctx "nav.scan"}}</a><a class="button secondary" href="/manual"><i data-lucide="pencil-line" aria-hidden="true"></i>{{t .Ctx "nav.manual"}}</a></nav>{{end}}<script>window.lucide&&lucide.createIcons()</script></body></html>{{end}}
{{define "adminNav"}}<a href="/admin">{{t .Ctx "admin.title"}}</a><a href="/admin/memberships">{{t .Ctx "admin.approvals"}}</a><a href="/admin/places">{{t .Ctx "places.title"}}</a><a href="/admin/products">{{t .Ctx "admin.products"}}</a><a href="/admin/qr-codes">{{t .Ctx "qr.matrices"}}</a><a href="/admin/print-qr-codes">{{t .Ctx "printqr.title"}}</a><a href="/admin/events">{{t .Ctx "nav.events"}}</a><a href="/admin/reports">{{t .Ctx "admin.reports"}}</a>{{end}}
{{define "login"}}{{template "layout" .}}{{end}}{{define "body"}}{{if eq .Title "login"}}<section class="auth-wrap"><div class="auth-card"><a class="wordmark" href="/places"><img src="/static/zest-logo.png" alt="" width="32" height="32">Zest</a><p class="eyebrow mt">{{t .Ctx "login.tag"}}</p><h1>{{t .Ctx "login.title"}}</h1><p class="muted">{{t .Ctx "login.copy"}}</p><form method="post"><input type="hidden" name="return" value="{{.Data}}"><label>{{t .Ctx "login.email"}}<input name="email" type="email" autocomplete="email" required></label><label>{{t .Ctx "login.password"}}<input name="password" type="password" autocomplete="current-password" required></label><button class="primary full">{{t .Ctx "login.submit"}}</button></form><p><a href="/register">{{t .Ctx "login.register"}}</a></p></div></section>{{else}}{{template "body2" .}}{{end}}{{end}}
{{define "body2"}}{{if eq .Title "register"}}<section class="auth-wrap"><div class="auth-card"><a class="wordmark" href="/places"><img src="/static/zest-logo.png" alt="" width="32" height="32">Zest</a><p class="eyebrow mt">{{t .Ctx "register.tag"}}</p><h1>{{t .Ctx "register.title"}}</h1><form method="post"><label>{{t .Ctx "register.name"}}<input name="name" autocomplete="name" required></label><label>{{t .Ctx "login.email"}}<input name="email" type="email" autocomplete="email" required></label><label>{{t .Ctx "login.password"}}<input name="password" type="password" autocomplete="new-password" required></label><button class="primary full">{{t .Ctx "register.submit"}}</button></form></div></section>{{else if eq .Title "waiting"}}<section class="card status-info"><p class="eyebrow">{{t .Ctx "waiting.eyebrow"}}</p><h1>{{t .Ctx "waiting.title"}}</h1><p>{{t .Ctx "waiting.added"}}</p><p>{{t .Ctx "waiting.copy"}}</p><a class="button secondary" href="/logout">{{t .Ctx "waiting.switch"}}</a></section>{{else if eq .Title "error"}}<section class="card status-danger"><p class="eyebrow">{{t .Ctx "error.eyebrow"}}</p><h1>{{index .Data "Message"}}</h1><div class="row mt"><a class="button primary" href="/places">{{t .Ctx "error.home"}}</a><a class="button secondary" href="/login">{{t .Ctx "error.login"}}</a></div></section>{{else if eq .Title "places"}}<div class="page-header"><div><p class="eyebrow">{{t .Ctx "places.eyebrow"}}</p><h1>{{t .Ctx "places.title"}}</h1><p class="muted">{{t .Ctx "places.copy"}}</p></div><a class="button secondary" href="/dev/qr-codes">{{t .Ctx "places.devqr"}}</a></div><div class="grid">{{range .Data}}<a class="place-card" href="/places/{{.Token}}"><div class="place-card-header"><span class="place-icon" aria-hidden="true"><i data-lucide="package"></i></span><h2>{{.Name}}</h2></div><dl class="place-overview">{{range .Overview}}<div class="place-overview-item {{productClass .Name}}" style="{{productStyle .Color}}"><dt>{{.Name}}</dt><dd><strong>{{.Qty}}</strong> {{.Unit}}</dd></div>{{else}}<div class="place-overview-empty">{{t $.Ctx "places.cardcopy"}}</div>{{end}}</dl></a>{{else}}<div class="empty-state">{{t .Ctx "places.empty"}}</div>{{end}}</div>{{else if eq .Title "place"}}<div class="page-header"><div><p class="eyebrow">{{t .Ctx "place.eyebrow"}}</p><h1>{{index .Data "Name"}}</h1><p class="muted">{{t .Ctx "place.stock"}}</p></div>{{if eq .Ctx.Role "admin"}}<span class="pill">{{t .Ctx "place.approved"}}</span>{{end}}</div><div class="grid two">{{range index .Data "Stocks"}}<article class="product-card {{productClass .Name}} {{if lt .Qty 0}}negative{{end}}" style="{{productStyle .Color}}"><span class="product-badge {{productClass .Name}}" style="{{productStyle .Color}}">{{.Name}}</span><div class="stock-qty">{{.Qty}}</div><p class="muted">{{unit $.Ctx .Unit}} {{t $.Ctx "place.here"}}</p>{{if lt .Qty 0}}<p class="status-banner status-warning">{{t $.Ctx "place.negative"}}</p>{{else if eq .Qty 0}}<p class="muted">{{t $.Ctx "place.zero"}}</p>{{end}}</article>{{else}}<div class="empty-state">{{t .Ctx "products.empty"}}</div>{{end}}</div><section class="section-header"><h2>{{t .Ctx "place.recent"}}</h2><a href="/events">{{t .Ctx "place.viewall"}}</a></section><div class="empty-state">{{t .Ctx "place.recentempty"}}</div><section class="card mt"><h2>{{t .Ctx "nav.manual"}}</h2><p class="muted">{{t .Ctx "manual.copy"}}</p><a class="button secondary full" href="/manual?place_token={{index .Data "Token"}}">{{t .Ctx "nav.manual"}}</a></section>{{else if eq .Title "events"}}<div class="page-header"><div><p class="eyebrow">{{t .Ctx "events.eyebrow"}}</p><h1>{{t .Ctx "events.title"}}</h1></div></div><div class="stack">{{range .Data}}<article class="event-card"><div class="event-sign {{if lt .Qty 0}}minus{{end}} {{if eq .Type "reversal"}}reversal{{end}}">{{eventSign .Qty .Type}}</div><div><strong class="event-product-line"><span class="product-badge {{productClass .Product}}" style="{{productStyle .Color}}">{{.Product}}</span></strong><p class="event-amount-line">{{eventAmountText $.Ctx .Type .Qty .Unit}}</p><p class="muted event-meta"><span>{{.Place}} · {{.User}}</span><span class="event-time">{{formatTime $.Ctx .Time}}</span></p><span class="pill">{{.Type}}</span></div></article>{{else}}<div class="empty-state">{{t .Ctx "events.empty"}}</div>{{end}}</div>{{else if eq .Title "manual"}}<section class="card manual-flow"><p class="eyebrow">{{t .Ctx "manual.eyebrow"}}</p><h1>{{t .Ctx "manual.title"}}</h1><p class="muted">{{t .Ctx "manual.copy"}}</p><form method="post" class="manual-form mt">{{csrf .Ctx}}<label>{{t .Ctx "manual.place"}}<select name="place_id" required>{{range .Data.Places}}<option value="{{.ID}}" {{if eq $.Data.SelectedPlace .ID}}selected{{end}}>{{.Name}}</option>{{end}}</select></label><label>{{t .Ctx "manual.product"}}<select name="product_id" required>{{range .Data.Products}}<option value="{{.ID}}">{{.Name}} · {{unit $.Ctx .Unit}}</option>{{end}}</select></label><label>{{t .Ctx "manual.amount"}}<input name="amount" type="number" inputmode="numeric" min="1" value="{{with index .Data.Amounts 0}}{{.}}{{else}}1{{end}}" required></label><div class="amount-chips">{{range .Data.Amounts}}<button class="secondary" type="button" onclick="this.form.amount.value='{{.}}'">{{.}}</button>{{end}}</div><div class="manual-actions"><button class="primary" name="action" value="add"><i data-lucide="plus" aria-hidden="true"></i>{{t .Ctx "manual.add"}}</button><button class="secondary" name="action" value="subtract"><i data-lucide="minus" aria-hidden="true"></i>{{t .Ctx "manual.subtract"}}</button></div></form></section>{{else if eq .Title "admin"}}<p class="eyebrow">{{t .Ctx "admin.backoffice"}}</p><h1>{{t .Ctx "admin.title"}}</h1><div class="admin-grid"><div class="metric-card"><span>{{t .Ctx "admin.total"}}</span><strong>—</strong></div><div class="metric-card"><span>{{t .Ctx "admin.today"}}</span><strong>—</strong></div><div class="metric-card"><span>{{t .Ctx "admin.approvals"}}</span><strong>—</strong></div><div class="metric-card"><span>{{t .Ctx "admin.negative"}}</span><strong>—</strong></div><div class="metric-card"><span>{{t .Ctx "admin.activeplaces"}}</span><strong>—</strong></div><div class="metric-card"><span>{{t .Ctx "admin.activeproducts"}}</span><strong>—</strong></div></div><div class="card mt"><h2>{{t .Ctx "admin.reports_settings"}}</h2><p class="muted">{{t .Ctx "admin.coming_soon"}}</p></div>{{else if eq .Title "members"}}<h1>{{t .Ctx "admin.approvals"}}</h1><div class="table-card"><table><thead><tr><th>{{t .Ctx "members.user"}}</th><th>{{t .Ctx "members.status"}}</th><th>{{t .Ctx "members.actions"}}</th></tr></thead><tbody>{{range .Data}}<tr><td><strong>{{.Name}}</strong><br><span class="muted">{{.Email}}</span></td><td><span class="pill">{{.Status}}</span></td><td><form method="post" action="/admin/memberships/{{.ID}}/approve">{{csrf $.Ctx}}<button>{{t $.Ctx "members.approve"}}</button></form><form method="post" action="/admin/memberships/{{.ID}}/reject">{{csrf $.Ctx}}<button class="secondary">{{t $.Ctx "members.reject"}}</button></form></td></tr>{{else}}<tr><td colspan="3">{{t .Ctx "members.none"}}</td></tr>{{end}}</tbody></table></div>{{else if eq .Title "products"}}<div class="page-header"><div><p class="eyebrow">{{t .Ctx "admin.products"}}</p><h1>{{t .Ctx "admin.products"}}</h1><p class="muted">{{t .Ctx "products.copy"}}</p></div><details class="new-product-menu"><summary class="button primary">{{t .Ctx "products.add"}}</summary><form class="product-management-form card" method="post" action="/admin/products">{{csrf .Ctx}}<input type="hidden" name="action" value="create"><h2>{{t .Ctx "products.add"}}</h2><div class="product-form-grid"><label>{{t .Ctx "products.name"}}<input name="name" required placeholder="Lemon"></label><label>{{t .Ctx "products.code"}}<input name="code" required placeholder="LEMON"></label><label>{{t .Ctx "products.unit"}}<input name="unit" value="units" required></label><label>{{t .Ctx "products.color"}}<input type="color" name="color" value="#F5B700"></label><button class="primary">{{t .Ctx "products.add"}}</button></div></form></details></div><div class="product-list" role="list">{{range .Data}}<details class="product-list-item {{if not .Active}}is-archived{{end}}" style="{{productStyle .Color}}" role="listitem"><summary><span class="product-badge {{productClass .Name}}" style="{{productStyle .Color}}">{{.Name}}</span><span class="muted product-list-meta">{{.Code}} · {{.Unit}}</span><span class="pill">{{if .Active}}{{t $.Ctx "products.active"}}{{else}}{{t $.Ctx "products.archived"}}{{end}}</span></summary><div class="product-editor"><form id="save-{{.ID}}" class="product-management-form" method="post" action="/admin/products">{{csrf $.Ctx}}<input type="hidden" name="action" value="save"><input type="hidden" name="product_id" value="{{.ID}}"><div class="product-form-grid"><label>{{t $.Ctx "products.name"}}<input name="name" value="{{.Name}}" required></label><label>{{t $.Ctx "products.code"}}<input name="code" value="{{.Code}}" required></label><label>{{t $.Ctx "products.unit"}}<input name="unit" value="{{.Unit}}" required></label><label>{{t $.Ctx "products.color"}}<input type="color" name="color" value="{{.Color}}"></label><button class="secondary">{{t $.Ctx "products.save"}}</button></div></form><form method="post" action="/admin/products">{{csrf $.Ctx}}<input type="hidden" name="product_id" value="{{.ID}}"><input type="hidden" name="action" value="{{if .Active}}archive{{else}}restore{{end}}"><button class="{{if .Active}}danger{{else}}secondary{{end}}">{{if .Active}}{{t $.Ctx "products.archive"}}{{else}}{{t $.Ctx "products.restore"}}{{end}}</button></form></div></details>{{else}}<div class="empty-state">{{t .Ctx "products.empty"}}</div>{{end}}</div>{{else if eq .Title "places_admin"}}<div class="page-header"><div><p class="eyebrow">{{t .Ctx "places.title"}}</p><h1>{{t .Ctx "places.title"}}</h1><p class="muted">{{t .Ctx "places.admin_copy"}}</p></div><details class="new-product-menu"><summary class="button primary">{{t .Ctx "places.add"}}</summary><form class="product-management-form card" method="post" action="/admin/places">{{csrf .Ctx}}<input type="hidden" name="action" value="create"><h2>{{t .Ctx "places.add"}}</h2><div class="product-form-grid"><label>{{t .Ctx "places.name"}}<input name="name" required placeholder="Warehouse"></label><button class="primary">{{t .Ctx "places.add"}}</button></div></form></details></div><div class="product-list" role="list">{{range .Data}}<details class="product-list-item {{if not .Active}}is-archived{{end}}" role="listitem"><summary><span class="admin-place-list-name"><span class="place-icon small" aria-hidden="true"><i data-lucide="package"></i></span><span>{{.Name}}</span></span><span class="pill">{{if .Active}}{{t $.Ctx "places.active"}}{{else}}{{t $.Ctx "places.archived"}}{{end}}</span></summary><div class="product-editor"><form id="save-{{.ID}}" class="product-management-form" method="post" action="/admin/places">{{csrf $.Ctx}}<input type="hidden" name="action" value="save"><input type="hidden" name="place_id" value="{{.ID}}"><div class="product-form-grid"><label>{{t $.Ctx "places.name"}}<input name="name" value="{{.Name}}" required></label><button class="secondary">{{t $.Ctx "places.save"}}</button></div></form><form method="post" action="/admin/places" {{if and .Active .HasStock}}onsubmit="return confirm('{{t $.Ctx "places.archive_confirm"}}')"{{end}}>{{csrf $.Ctx}}<input type="hidden" name="place_id" value="{{.ID}}"><input type="hidden" name="action" value="{{if .Active}}archive{{else}}restore{{end}}"><button class="{{if .Active}}danger{{else}}secondary{{end}}">{{if .Active}}{{t $.Ctx "places.archive"}}{{else}}{{t $.Ctx "places.restore"}}{{end}}</button></form></div></details>{{else}}<div class="empty-state">{{t .Ctx "places.empty"}}</div>{{end}}</div>{{else if eq .Title "printqr"}}<div class="page-header no-print"><div><p class="eyebrow">{{t .Ctx "qr.matrices"}}</p><h1>{{t .Ctx "printqr.title"}}</h1><p class="muted">{{t .Ctx "printqr.copy"}}</p></div><button class="secondary" onclick="window.print()">{{t .Ctx "printqr.print"}}</button></div><section class="card no-print"><h2>{{t .Ctx "printqr.add"}}</h2><form method="post" class="printqr-form" data-printqr-form>{{csrf .Ctx}}<input type="hidden" name="action" value="create"><label data-printqr-field="kind">{{t .Ctx "printqr.kind"}}<select name="kind" data-printqr-kind><option value="home">{{t .Ctx "printqr.open_home"}}</option><option value="place">{{t .Ctx "qr.open_place"}}</option><option value="command">{{t .Ctx "printqr.command"}}</option></select></label><label data-printqr-field="place">{{t .Ctx "manual.place"}}<select name="place_id">{{range .Data.Places}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select></label><label data-printqr-field="product">{{t .Ctx "manual.product"}}<select name="product_id">{{range .Data.Products}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select></label><label data-printqr-field="amount">{{t .Ctx "manual.amount"}}<input name="amount" type="number" min="1" value="{{with index .Data.Amounts 0}}{{.}}{{else}}1{{end}}"></label><label data-printqr-field="action">{{t .Ctx "nav.events"}}<select name="command_action"><option value="add">{{t .Ctx "manual.add"}}</option><option value="subtract">{{t .Ctx "manual.subtract"}}</option></select></label><label data-printqr-field="label">{{t .Ctx "printqr.label"}}<input name="label" placeholder="Optional custom label"></label><div class="printqr-submit-row"><button class="primary">{{t .Ctx "printqr.add"}}</button></div></form><script>document.querySelectorAll('[data-printqr-form]').forEach(function(form){var kind=form.querySelector('[data-printqr-kind]');var fields={place:form.querySelector('[data-printqr-field="place"]'),product:form.querySelector('[data-printqr-field="product"]'),amount:form.querySelector('[data-printqr-field="amount"]'),action:form.querySelector('[data-printqr-field="action"]')};function setField(name,show){var field=fields[name];if(!field)return;field.classList.toggle('is-hidden',!show);field.querySelectorAll('input,select').forEach(function(control){control.disabled=!show;});}function update(){var value=kind.value;setField('place',value==='place'||value==='command');setField('product',value==='command');setField('amount',value==='command');setField('action',value==='command');}kind.addEventListener('change',update);update();});</script></section><section class="qr-page printqr-page"><div class="row print-only"><div><span class="wordmark"><img src="/static/zest-logo.png" alt="" width="32" height="32">Zest</span><p class="eyebrow">{{t .Ctx "printqr.title"}}</p></div></div><div class="qrgrid printable-matrix printqr-matrix">{{range .Data.Codes}}<div class="qr {{productClass .Product}}" style="{{productStyle .Color}}"><img alt="QR code for {{.Label}}" src="{{.Img}}"><strong>{{.Label}}</strong>{{if eq .Kind "command"}}<small>{{.Place}}<br>{{.ActionLabel}} {{.Amount}} {{.Product}}</small>{{else if eq .Kind "place"}}<small>{{.Place}}</small>{{else}}<small>{{.Target}}</small>{{end}}<form class="no-print" method="post">{{csrf $.Ctx}}<input type="hidden" name="action" value="delete"><input type="hidden" name="id" value="{{.ID}}"><button class="danger">{{t $.Ctx "printqr.delete"}}</button></form></div>{{else}}<div class="empty-state no-print">{{t .Ctx "printqr.empty"}}</div>{{end}}</div></section>{{else if eq .Title "qr"}}<section class="qr-page"><div class="row"><div><span class="wordmark"><img src="/static/zest-logo.png" alt="" width="32" height="32">Zest</span><p class="eyebrow">{{t .Ctx "qr.matrix"}} · Example Company</p><h1>{{t .Ctx "qr.print_matrix"}}</h1><p class="muted">{{t .Ctx "qr.generated"}}</p></div><button class="secondary no-print" onclick="window.print()">{{t .Ctx "qr.print"}}</button></div><div class="qr-section"><h2>{{t .Ctx "action.add_section"}}</h2><div class="qrgrid">{{range .Data}}{{if eq .Action "add"}}<div class="qr {{productClass .Label}}" style="{{productStyle .Color}}"><img alt="QR code" src="{{.Img}}"><small>{{.Label}}</small></div>{{end}}{{end}}</div></div><div class="qr-section"><h2>{{t .Ctx "action.subtract_section"}}</h2><div class="qrgrid">{{range .Data}}{{if eq .Action "subtract"}}<div class="qr {{productClass .Label}}" style="{{productStyle .Color}}"><img alt="QR code" src="{{.Img}}"><small>{{.Label}}</small></div>{{end}}{{end}}</div></div><div class="qr-section"><h2>{{t .Ctx "qr.backup"}}</h2><p class="muted">{{t .Ctx "qr.backup_copy"}}</p>{{with index .Data 0}}<a class="qr" href="{{.PlaceURL}}"><img alt="{{t $.Ctx "qr.open_place"}}" src="{{.PlaceImg}}"><small>{{t $.Ctx "qr.open_place"}}</small></a>{{end}}</div></section>{{else if eq .Title "devqr"}}<section class="qr-page"><div class="row"><div><p class="eyebrow">{{t .Ctx "devqr.matrix"}}</p><h1>{{t .Ctx "devqr.title"}}</h1><p class="muted">{{t .Ctx "devqr.copy"}}</p></div><button class="secondary no-print" onclick="window.print()">{{t .Ctx "qr.print_matrix"}}</button></div><div class="qrgrid printable-matrix">{{range .Data}}<a class="qr {{productClass .Product}}" style="{{productStyle .Color}}" href="{{.Href}}"><img alt="QR code for {{.ActionLabel}} {{.Amount}} {{.Product}} at {{.Place}}" src="{{.Img}}"><small>{{.Place}}<br>{{.ActionLabel}} {{.Amount}} {{.Product}}</small></a>{{end}}</div></section>{{else if eq .Title "settings"}}<section class="card"><p class="eyebrow">{{t .Ctx "nav.settings"}}</p><h1>{{t .Ctx "settings.title"}}</h1><p class="muted">{{t .Ctx "settings.copy"}}</p>{{if index .Data "Saved"}}<p class="status-banner status-success">{{t .Ctx "settings.saved"}}</p>{{end}}<form method="post" class="mt">{{csrf .Ctx}}<label>{{t .Ctx "settings.language"}}<select name="language"><option value="" {{if eq .Ctx.LangPref ""}}selected{{end}}>{{t .Ctx "settings.device"}}</option><option value="en" {{if eq .Ctx.LangPref "en"}}selected{{end}}>{{t .Ctx "settings.english"}}</option><option value="de" {{if eq .Ctx.LangPref "de"}}selected{{end}}>{{t .Ctx "settings.german"}}</option></select></label><label>{{t .Ctx "settings.time_format"}}<select name="time_format"><option value="local" {{if eq .Ctx.TimeFormat "local"}}selected{{end}}>{{timeExample .Ctx "local"}}</option><option value="iso" {{if eq .Ctx.TimeFormat "iso"}}selected{{end}}>{{timeExample .Ctx "iso"}}</option><option value="us" {{if eq .Ctx.TimeFormat "us"}}selected{{end}}>{{timeExample .Ctx "us"}}</option><option value="eu" {{if eq .Ctx.TimeFormat "eu"}}selected{{end}}>{{timeExample .Ctx "eu"}}</option><option value="24h" {{if eq .Ctx.TimeFormat "24h"}}selected{{end}}>{{timeExample .Ctx "24h"}}</option></select></label><button class="primary">{{t .Ctx "settings.save"}}</button></form></section>{{else if eq .Title "reports"}}<div class="page-header"><div><p class="eyebrow">{{t .Ctx "admin.reports"}}</p><h1>{{t .Ctx "reports.title"}}</h1></div><a class="button primary" href="/admin/reports/export.csv">{{t .Ctx "reports.export"}}</a></div><section class="card"><div class="report-filters"><label>{{t .Ctx "reports.date_range"}}<input value="{{t .Ctx "reports.last_7_days"}}" disabled></label><label>{{t .Ctx "reports.product"}}<select disabled><option>{{t .Ctx "reports.all_products"}}</option></select></label><label>{{t .Ctx "reports.place"}}<select disabled><option>{{t .Ctx "reports.all_places"}}</option></select></label></div><div class="chart-placeholder mt">{{t .Ctx "reports.chart"}}</div></section><section class="section-header"><h2>{{t .Ctx "reports.preview"}}</h2></section><div class="stack">{{range .Data}}<div class="event-card"><div class="event-sign {{if lt .Qty 0}}minus{{end}}">{{eventSign .Qty .Type}}</div><div><strong class="event-product-line"><span class="product-badge {{productClass .Product}}" style="{{productStyle .Color}}">{{.Product}}</span></strong><p class="event-amount-line">{{eventAmountText $.Ctx .Type .Qty .Unit}}</p><p class="muted event-meta"><span>{{.Place}}</span><span class="event-time">{{formatTime $.Ctx .Time}}</span></p></div></div>{{else}}<div class="empty-state">{{t .Ctx "reports.empty"}}</div>{{end}}</div>{{else if eq .Title "result"}}<section class="hero-result {{productClass (index .Data "Product")}}" style="{{productStyle (index .Data "Color")}}"><span class="result-action">{{if gt (index .Data "Delta") 0}}{{t .Ctx "result.added"}}{{else if lt (index .Data "Delta") 0}}{{t .Ctx "result.subtracted"}}{{else}}{{t .Ctx "result.event"}}{{end}}</span><div class="result-amount">{{index .Data "Amount"}} ×</div><h1>{{index .Data "Product"}}</h1><p class="result-place">{{if gt (index .Data "Delta") 0}}{{t .Ctx "result.to"}}{{else}}{{t .Ctx "result.from"}}{{end}} {{index .Data "Place"}}</p><div class="current-stock"><p class="eyebrow">{{t .Ctx "result.current"}}</p><strong class="stock-qty">{{index .Data "Stock"}} {{unit .Ctx (index .Data "Unit")}}</strong></div><p class="muted mt">{{t .Ctx "result.created"}} {{formatTime .Ctx (index .Data "Created")}}</p></section>{{if index .Data "Negative"}}<p class="status-banner status-warning mt"><strong>{{t .Ctx "result.warning"}}</strong> {{t .Ctx "result.negative"}} {{index .Data "Stock"}} {{unit .Ctx (index .Data "Unit")}}.</p>{{end}}<div id="undo" class="undo-panel mt">{{if not (index .Data "Reversed")}}<p><strong>{{t .Ctx "result.undoq"}}</strong><br><span class="muted">{{t .Ctx "result.undocopy"}}</span></p><form hx-post="/events/{{index .Data "ID"}}/undo" hx-target="#undo" method="post">{{csrf .Ctx}}<button class="danger full" _="on load set n to {{index .Data "UndoSeconds"}} then repeat while n > 0 set my.innerText to '{{t .Ctx "result.undo"}} · ' + n + 's' wait 1s decrement n end then set my.disabled to true then set my.innerText to '{{t .Ctx "result.undo_expired"}}'">{{t .Ctx "result.undo"}} · {{index .Data "UndoSeconds"}}s</button></form>{{else}}<div class="success"><strong>{{t .Ctx "result.undone"}}</strong><p>{{t .Ctx "result.undone_copy"}}</p></div>{{end}}</div><section class="card mt"><h2>{{t .Ctx "place.scannext"}}</h2><p>{{t .Ctx "result.scan_copy"}}</p><p class="muted">{{t .Ctx "result.scan_later"}}</p></section>{{end}}{{end}}
{{define "register"}}{{template "layout" .}}{{end}}
{{define "waiting"}}{{template "layout" .}}{{end}}
{{define "error"}}{{template "layout" .}}{{end}}
{{define "places"}}{{template "layout" .}}{{end}}
{{define "place"}}{{template "layout" .}}{{end}}
{{define "events"}}{{template "layout" .}}{{end}}
{{define "manual"}}{{template "layout" .}}{{end}}
{{define "admin"}}{{template "layout" .}}{{end}}
{{define "members"}}{{template "layout" .}}{{end}}
{{define "products"}}{{template "layout" .}}{{end}}
{{define "places_admin"}}{{template "layout" .}}{{end}}
{{define "printqr"}}{{template "layout" .}}{{end}}
{{define "qr"}}{{template "layout" .}}{{end}}
{{define "devqr"}}{{template "layout" .}}{{end}}
{{define "settings"}}{{template "layout" .}}{{end}}
{{define "reports"}}{{template "layout" .}}{{end}}
{{define "result"}}{{template "layout" .}}{{end}}
`
