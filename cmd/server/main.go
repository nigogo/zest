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
	Authed                            bool
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
}

func (a *App) seed() {
	var c int
	a.db.QueryRow("select count(*) from organizations").Scan(&c)
	org := "org_dev"
	if c > 0 {
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
		a.db.Exec("insert into products(id,organization_id,name,code,unit,active)values(?,?,?,?,?,1)", fmt.Sprintf("prod_%d", i), org, p, fmt.Sprintf("P%d", i+1), "units")
	}
	a.ensureSeedQRCommands(org)
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
	a.tmpl = template.Must(template.New("base").Funcs(template.FuncMap{"productClass": productClass, "eventSign": eventSign, "abs": func(n int) int {
		if n < 0 {
			return -n
		}
		return n
	}, "csrf": func(c Ctx) template.HTML {
		return template.HTML(`<input type="hidden" name="csrf" value="` + template.HTMLEscapeString(c.CSRF) + `">`)
	}}).Parse(tpl))
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, d any) {
	c := a.ctx(r)
	a.tmpl.ExecuteTemplate(w, name, Page{Title: name, Ctx: c, Data: d})
}

func (a *App) ctx(r *http.Request) Ctx {
	ck, err := r.Cookie("sid")
	if err != nil {
		return Ctx{}
	}
	var c Ctx
	c.Authed = true
	err = a.db.QueryRow("select s.user_id,s.csrf,coalesce(m.organization_id,''),coalesce(m.role,''),coalesce(m.status,'') from sessions s left join memberships m on m.user_id=s.user_id where s.token=? and s.expires_at>datetime('now') order by m.created_at limit 1", ck.Value).Scan(&c.UserID, &c.CSRF, &c.OrgID, &c.Role, &c.Status)
	if err != nil {
		return Ctx{}
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
		a.render(w, r, "error", map[string]string{"Message": "Your account is waiting for approval or access was rejected."})
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

func (a *App) routes(m *http.ServeMux) {
	m.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/places", 302) })
	m.HandleFunc("/login", a.login)
	m.HandleFunc("/register", a.register)
	m.HandleFunc("/logout", a.logout)
	m.HandleFunc("/org/join/", a.join)
	m.HandleFunc("/scan/", a.scan)
	m.HandleFunc("/dev/qr-codes", a.devQRCodes)
	m.HandleFunc("/events/", a.eventRoutes)
	m.HandleFunc("/events", a.events)
	m.HandleFunc("/places/", a.placeToken)
	m.HandleFunc("/places", a.places)
	m.HandleFunc("/reports/export.csv", a.csv)
	m.HandleFunc("/reports", a.reports)
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
		a.render(w, r, "error", map[string]string{"Message": "Invalid invite."})
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
		a.render(w, r, "error", map[string]string{"Message": "This QR code is invalid or no longer active."})
		return
	}
	if org != c.OrgID {
		http.Error(w, "forbidden", 403)
		return
	}
	if pactive == 0 || practive == 0 {
		a.render(w, r, "error", map[string]string{"Message": "This QR code refers to an inactive place or product."})
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
	var place, prod, unit, etype, uid, created string
	var delta, stock, rev int
	err := a.db.QueryRow(`select pl.name,pr.name,pr.unit,e.event_type,e.user_id,e.created_at,e.quantity_delta,(select coalesce(sum(quantity_delta),0) from inventory_events where place_id=e.place_id and product_id=e.product_id),(select count(*) from inventory_events where reversed_event_id=e.id) from inventory_events e join places pl on pl.id=e.place_id join products pr on pr.id=e.product_id where e.id=? and e.organization_id=?`, eid, org).Scan(&place, &prod, &unit, &etype, &uid, &created, &delta, &stock, &rev)
	if err != nil {
		return nil
	}
	return map[string]any{"ID": eid, "Place": place, "Product": prod, "Unit": unit, "Type": etype, "Delta": delta, "Amount": abs(delta), "Stock": stock, "Reversed": rev > 0, "UndoSeconds": int(a.undoWindow.Seconds()), "Created": created, "Negative": stock < 0}
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
	rows, _ := a.db.Query("select name,token from places where organization_id=? and active=1 order by name", c.OrgID)
	defer rows.Close()
	var v []map[string]string
	for rows.Next() {
		var n, t string
		rows.Scan(&n, &t)
		v = append(v, map[string]string{"Name": n, "Token": t})
	}
	a.render(w, r, "places", v)
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
	rows, _ := a.db.Query(`select p.name,p.unit,coalesce(sum(e.quantity_delta),0) from products p left join inventory_events e on e.product_id=p.id and e.place_id=? where p.organization_id=? and p.active=1 group by p.id order by p.name`, pid, c.OrgID)
	var stocks []map[string]any
	for rows.Next() {
		var n, u string
		var q int
		rows.Scan(&n, &u, &q)
		stocks = append(stocks, map[string]any{"Name": n, "Unit": u, "Qty": q})
	}
	rows.Close()
	a.render(w, r, "place", map[string]any{"Name": name, "Stocks": stocks})
}

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	a.render(w, r, "events", a.listEvents(c.OrgID))
}

func (a *App) listEvents(org string) []map[string]any {
	rows, _ := a.db.Query(`select e.created_at,u.name,e.quantity_delta,pr.name,pl.name,e.event_type from inventory_events e join users u on u.id=e.user_id join products pr on pr.id=e.product_id join places pl on pl.id=e.place_id where e.organization_id=? order by e.created_at desc limit 100`, org)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var t, u, p, pl, et string
		var q int
		rows.Scan(&t, &u, &q, &p, &pl, &et)
		out = append(out, map[string]any{"Time": t, "User": u, "Qty": q, "Product": p, "Place": pl, "Type": et})
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
		a.places(w, r)
		return
	}
	if p == "/admin/products" {
		a.render(w, r, "products", nil)
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

func (a *App) qrImageURL(target string) string {
	return "https://api.qrserver.com/v1/create-qr-code/?size=220x220&margin=12&data=" + url.QueryEscape(target)
}

func (a *App) scanURL(token string) string {
	return strings.TrimRight(a.base, "/") + "/scan/" + token
}

func (a *App) qrMatrix(w http.ResponseWriter, r *http.Request, c Ctx) {
	pid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/places/"), "/qr-matrix")
	rows, _ := a.db.Query(`select q.action,q.amount,pr.name,q.token from qr_commands q join products pr on pr.id=q.product_id where q.organization_id=? and q.place_id=? and q.active=1 and pr.active=1 order by q.action,pr.name,q.amount`, c.OrgID, pid)
	var out []map[string]any
	for rows.Next() {
		var act, prod, tok string
		var amt int
		rows.Scan(&act, &amt, &prod, &tok)
		scanURL := a.scanURL(tok)
		out = append(out, map[string]any{"Action": act, "Label": fmt.Sprintf("%s %d %s", strings.Title(act), amt, prod), "Href": scanURL, "Img": a.qrImageURL(scanURL)})
	}
	a.render(w, r, "qr", out)
}

func (a *App) devQRCodes(w http.ResponseWriter, r *http.Request) {
	if a.env == "production" {
		http.NotFound(w, r)
		return
	}
	c, ok := a.approved(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`select pl.name,pr.name,q.action,q.amount,q.token from qr_commands q join places pl on pl.id=q.place_id join products pr on pr.id=q.product_id where q.organization_id=? and q.active=1 and pl.active=1 and pr.active=1 order by pl.name,pr.name,q.action,q.amount`, c.OrgID)
	if err != nil {
		http.Error(w, "could not load QR commands", 500)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var place, prod, act, tok string
		var amt int
		rows.Scan(&place, &prod, &act, &amt, &tok)
		href := "/scan/" + tok
		scanURL := a.scanURL(tok)
		out = append(out, map[string]any{"Place": place, "Product": prod, "Action": act, "Amount": amt, "Href": href, "ScanURL": scanURL, "Img": a.qrImageURL(scanURL)})
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

const tpl = `{{define "layout"}}<!doctype html><html lang="en"><head><title>Zest · {{.Title}}</title><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/static/app.css"><script src="https://unpkg.com/htmx.org@1.9.12"></script><script src="https://unpkg.com/hyperscript.org@0.9.12"></script></head><body><header class="mobile-header"><div><a class="wordmark" href="/places"><img src="/static/zest-logo.svg" alt="" width="32" height="32">Zest</a><div class="header-meta">Example Company{{if .Ctx.Status}} · {{.Ctx.Status}}{{end}}</div></div><nav class="header-nav" aria-label="Primary"><a class="button ghost icon-button" href="/places" title="Places">⌂</a><a class="button ghost icon-button" href="/events" title="Events">↕</a>{{if eq .Ctx.Role "admin"}}<a class="button ghost icon-button" href="/admin" title="Admin">☰</a>{{end}}{{if .Ctx.Authed}}<form method="post" action="/logout">{{csrf .Ctx}}<button class="ghost icon-button" title="Logout">⎋</button></form>{{end}}</nav></header><main class="app-main">{{template "body" .}}</main></body></html>{{end}}
{{define "login"}}{{template "layout" .}}{{end}}{{define "body"}}{{if eq .Title "login"}}<section class="auth-wrap"><div class="auth-card"><a class="wordmark" href="/places"><img src="/static/zest-logo.svg" alt="" width="32" height="32">Zest</a><p class="eyebrow mt">Scan. Move. Done.</p><h1>Log in</h1><p class="muted">Fast inventory for real-world work.</p><form method="post"><input type="hidden" name="return" value="{{.Data}}"><label>Email<input name="email" type="email" autocomplete="email" required></label><label>Password<input name="password" type="password" autocomplete="current-password" required></label><button class="primary full">Log in</button></form><p><a href="/register">Create an account</a></p></div></section>{{else}}{{template "body2" .}}{{end}}{{end}}
{{define "body2"}}{{if eq .Title "register"}}<section class="auth-wrap"><div class="auth-card"><a class="wordmark" href="/places"><img src="/static/zest-logo.svg" alt="" width="32" height="32">Zest</a><p class="eyebrow mt">Join your team</p><h1>Create account</h1><form method="post"><label>Name<input name="name" autocomplete="name" required></label><label>Email<input name="email" type="email" autocomplete="email" required></label><label>Password<input name="password" type="password" autocomplete="new-password" required></label><button class="primary full">Create account</button></form></div></section>{{else if eq .Title "waiting"}}<section class="card status-info"><p class="eyebrow">Waiting for approval</p><h1>Waiting for approval</h1><p>Your account has been added to Example Company.</p><p>An admin must approve you before you can change inventory.</p><a class="button secondary" href="/logout">Use another account</a></section>{{else if eq .Title "error"}}<section class="card status-danger"><p class="eyebrow">Zest cannot continue</p><h1>{{index .Data "Message"}}</h1><div class="row mt"><a class="button primary" href="/places">Go home</a><a class="button secondary" href="/login">Log in</a></div></section>{{else if eq .Title "places"}}<div class="page-header"><div><p class="eyebrow">Operator places</p><h1>Places</h1><p class="muted">Open a place, scan a command QR, and keep moving.</p></div><a class="button secondary" href="/dev/qr-codes">Dev QRs</a></div><div class="grid">{{range .Data}}<a class="product-card product-default" href="/places/{{.Token}}"><div class="row"><h2>{{.Name}}</h2><span class="pill">Active</span></div><p class="muted">View current stock and recent movement.</p></a>{{else}}<div class="empty-state">No active places yet.</div>{{end}}</div>{{else if eq .Title "place"}}<div class="page-header"><div><p class="eyebrow">Active place</p><h1>{{index .Data "Name"}}</h1><p class="muted">Current stock by product.</p></div><span class="pill">Approved</span></div><div class="grid two">{{range index .Data "Stocks"}}<article class="product-card {{productClass .Name}} {{if lt .Qty 0}}negative{{end}}"><span class="product-badge {{productClass .Name}}">{{.Name}}</span><div class="stock-qty">{{.Qty}}</div><p class="muted">{{.Unit}} here</p>{{if lt .Qty 0}}<p class="status-banner status-warning">Negative stock. Check the last movement.</p>{{else if eq .Qty 0}}<p class="muted">Zero stock.</p>{{end}}</article>{{else}}<div class="empty-state">No products configured yet.</div>{{end}}</div><section class="section-header"><h2>Recent activity</h2><a href="/events">View all</a></section><div class="empty-state">Recent movement appears after scans.</div><div class="bottom-action-bar"><a class="button primary" href="/dev/qr-codes">Scan next</a><a class="button secondary" href="/events">Events</a></div>{{else if eq .Title "events"}}<div class="page-header"><div><p class="eyebrow">Audit timeline</p><h1>Recent events</h1></div></div><div class="stack">{{range .Data}}<article class="event-card"><div class="event-sign {{if lt .Qty 0}}minus{{end}} {{if eq .Type "reversal"}}reversal{{end}}">{{eventSign .Qty .Type}}</div><div><strong>{{if eq .Type "reversal"}}Reversal of {{else if gt .Qty 0}}Added {{else}}Subtracted {{end}}{{abs .Qty}} {{.Product}}</strong><p class="muted">{{.Place}} · {{.User}} · {{.Time}}</p><span class="pill">{{.Type}}</span></div></article>{{else}}<div class="empty-state">No events yet.</div>{{end}}</div>{{else if eq .Title "admin"}}<div class="admin-shell"><nav class="admin-nav"><a href="/admin">Dashboard</a><a href="/admin/memberships">Pending approvals</a><a href="/admin/places">Places</a><a href="/admin/products">Products</a><a href="/dev/qr-codes">QR matrices</a><a href="/admin/events">Events</a><a href="/reports">Reports</a><a aria-disabled="true">Settings · Coming soon</a></nav><section class="admin-content"><p class="eyebrow">Backoffice</p><h1>Admin dashboard</h1><div class="admin-grid"><div class="metric-card"><span>Total stock</span><strong>—</strong></div><div class="metric-card"><span>Events today</span><strong>—</strong></div><div class="metric-card"><span>Pending approvals</span><strong>—</strong></div><div class="metric-card"><span>Negative stock</span><strong>—</strong></div><div class="metric-card"><span>Active places</span><strong>—</strong></div><div class="metric-card"><span>Active products</span><strong>—</strong></div></div><div class="card mt"><h2>Reports and settings</h2><p class="muted">Advanced charts, alerts, and configuration are coming soon.</p></div></section></div>{{else if eq .Title "members"}}<h1>Pending approvals</h1><div class="table-card"><table><thead><tr><th>User</th><th>Status</th><th>Actions</th></tr></thead><tbody>{{range .Data}}<tr><td><strong>{{.Name}}</strong><br><span class="muted">{{.Email}}</span></td><td><span class="pill">{{.Status}}</span></td><td><form method="post" action="/admin/memberships/{{.ID}}/approve">{{csrf $.Ctx}}<button>Approve</button></form><form method="post" action="/admin/memberships/{{.ID}}/reject">{{csrf $.Ctx}}<button class="secondary">Reject</button></form></td></tr>{{else}}<tr><td colspan="3">No pending approvals.</td></tr>{{end}}</tbody></table></div>{{else if eq .Title "products"}}<section class="card"><p class="eyebrow">Products</p><h1>Products</h1><p>Product administration is intentionally simple in this MVP; seed products are active.</p><div class="grid two"><span class="product-badge product-lemon">Lemon</span><span class="product-badge product-lime">Lime</span><span class="product-badge product-orange">Orange</span><span class="product-badge product-grapefruit">Grapefruit</span></div></section>{{else if eq .Title "qr"}}<section class="qr-page"><div class="row"><div><span class="wordmark"><img src="/static/zest-logo.svg" alt="" width="32" height="32">Zest</span><p class="eyebrow">QR command matrix · Example Company</p><h1>Print matrix</h1><p class="muted">Generated for laminated operational use.</p></div><button class="secondary no-print" onclick="window.print()">Print</button></div><div class="qr-section"><h2>ADD</h2><div class="qrgrid">{{range .Data}}{{if eq .Action "add"}}<div class="qr"><img alt="QR code" src="{{.Img}}"><small>{{.Label}}</small><span class="qr-url">{{.Href}}</span></div>{{end}}{{end}}</div></div><div class="qr-section"><h2>SUBTRACT</h2><div class="qrgrid">{{range .Data}}{{if eq .Action "subtract"}}<div class="qr"><img alt="QR code" src="{{.Img}}"><small>{{.Label}}</small><span class="qr-url">{{.Href}}</span></div>{{end}}{{end}}</div></div><div class="qr-section"><h2>Backup place link</h2><p class="muted">Open the place page if a command label is damaged or unclear.</p><div class="qr"><img alt="Backup QR placeholder" src="data:image/svg+xml;base64,PHN2ZyB4bWxucz0naHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmcnIHdpZHRoPSc5NicgaGVpZ2h0PSc5Nic+PHJlY3Qgd2lkdGg9Jzk2JyBoZWlnaHQ9Jzk2JyBmaWxsPSd3aGl0ZScvPjxyZWN0IHg9JzgnIHk9JzgnIHdpZHRoPSc4MCcgaGVpZ2h0PSc4MCcgZmlsbD0nbm9uZScgc3Ryb2tlPSdibGFjaycvPjx0ZXh0IHg9JzQ4JyB5PSc1MicgZm9udC1zaXplPScxMCcgdGV4dC1hbmNob3I9J21pZGRsZSc+UGxhY2U8L3RleHQ+PC9zdmc+"><small>Open place page</small></div></div></section>{{else if eq .Title "devqr"}}<section class="qr-page"><div class="row"><div><p class="eyebrow">Development QR matrix</p><h1>Development QR Codes</h1><p class="muted">Click any QR card to simulate scanning. Print this page to test real phone-camera QR scans.</p></div><button class="secondary no-print" onclick="window.print()">Print matrix</button></div><div class="qrgrid printable-matrix">{{range .Data}}<a class="qr {{productClass .Product}}" href="{{.Href}}"><img alt="QR code for {{.Action}} {{.Amount}} {{.Product}} at {{.Place}}" src="{{.Img}}"><small>{{.Place}}<br>{{.Action}} {{.Amount}} {{.Product}}</small><span class="qr-url">{{.ScanURL}}</span></a>{{end}}</div></section>{{else if eq .Title "reports"}}<div class="page-header"><div><p class="eyebrow">Reports</p><h1>Movement reports</h1></div><a class="button primary" href="/reports/export.csv">Export CSV</a></div><section class="card"><div class="report-filters"><label>Date range<input value="Last 7 days" disabled></label><label>Product<select disabled><option>All products</option></select></label><label>Place<select disabled><option>All places</option></select></label></div><div class="chart-placeholder mt">Chart placeholder · Coming soon</div></section><section class="section-header"><h2>Event export preview</h2></section><div class="stack">{{range .Data}}<div class="event-card"><div class="event-sign {{if lt .Qty 0}}minus{{end}}">{{eventSign .Qty .Type}}</div><div><strong>{{.Product}}</strong><p class="muted">{{.Place}} · {{.Time}}</p></div></div>{{else}}<div class="empty-state">No report data yet.</div>{{end}}</div>{{else if eq .Title "result"}}<section class="hero-result {{productClass (index .Data "Product")}}"><span class="result-action">{{if gt (index .Data "Delta") 0}}ADDED{{else if lt (index .Data "Delta") 0}}SUBTRACTED{{else}}EVENT{{end}}</span><div class="result-amount">{{index .Data "Amount"}} ×</div><h1>{{index .Data "Product"}}</h1><p class="result-place">{{if gt (index .Data "Delta") 0}}to{{else}}from{{end}} {{index .Data "Place"}}</p><div class="current-stock"><p class="eyebrow">Current stock here</p><strong class="stock-qty">{{index .Data "Stock"}} {{index .Data "Unit"}}</strong></div><p class="muted mt">Created {{index .Data "Created"}}</p></section>{{if index .Data "Negative"}}<p class="status-banner status-warning mt"><strong>Warning:</strong> stock is now negative: {{index .Data "Stock"}} {{index .Data "Unit"}}.</p>{{end}}<div id="undo" class="undo-panel mt">{{if not (index .Data "Reversed")}}<p><strong>Need to correct this scan?</strong><br><span class="muted">Undo creates a reversal event. The audit trail stays intact.</span></p><form hx-post="/events/{{index .Data "ID"}}/undo" hx-target="#undo" method="post">{{csrf .Ctx}}<button class="danger full" _="on load set n to {{index .Data "UndoSeconds"}} then repeat while n > 0 set my.innerText to 'Undo · ' + n + 's' wait 1s decrement n end then set my.disabled to true then set my.innerText to 'Undo window expired'">Undo · {{index .Data "UndoSeconds"}}s</button></form>{{else}}<div class="success"><strong>Undone.</strong><p>A reversal event was created.</p></div>{{end}}</div><section class="card mt"><h2>Scan next</h2><p>Use your phone camera to scan the next QR code.</p><p class="muted">In-app scanning can be enabled later.</p></section><div class="bottom-action-bar"><a class="button primary" href="/dev/qr-codes">Scan next</a><a class="button secondary" href="/places">View places</a></div>{{end}}{{end}}
{{define "register"}}{{template "layout" .}}{{end}}
{{define "waiting"}}{{template "layout" .}}{{end}}
{{define "error"}}{{template "layout" .}}{{end}}
{{define "places"}}{{template "layout" .}}{{end}}
{{define "place"}}{{template "layout" .}}{{end}}
{{define "events"}}{{template "layout" .}}{{end}}
{{define "admin"}}{{template "layout" .}}{{end}}
{{define "members"}}{{template "layout" .}}{{end}}
{{define "products"}}{{template "layout" .}}{{end}}
{{define "qr"}}{{template "layout" .}}{{end}}
{{define "devqr"}}{{template "layout" .}}{{end}}
{{define "reports"}}{{template "layout" .}}{{end}}
{{define "result"}}{{template "layout" .}}{{end}}
`
