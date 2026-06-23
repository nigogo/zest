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
		a.ensureSeedQRCommands(org)
		return
	}
	a.db.Exec("insert into organizations(id,name)values(?,?)", org, "Example Company")
	h := hashpw(env("SEED_ADMIN_PASSWORD", "admin123-change-me"))
	u := "user_admin"
	a.db.Exec("insert into users(id,email,name,password_hash)values(?,?,?,?)", u, env("SEED_ADMIN_EMAIL", "admin@example.com"), "Admin", h)
	a.db.Exec("insert into memberships(id,organization_id,user_id,role,status,approved_at)values(?,?,?,?,?,CURRENT_TIMESTAMP)", "mem_admin", org, u, "admin", "approved")
	a.db.Exec("insert into organization_invites(id,organization_id,token,active)values(?,?,?,1)", "invite_dev", org, "dev-invite-token")
	places := []string{"Freezer A", "Freezer B", "Freezer C"}
	prods := []string{"Product A", "Product B", "Product C", "Product D"}
	for i, p := range places {
		a.db.Exec("insert into places(id,organization_id,name,token,active)values(?,?,?,?,1)", fmt.Sprintf("place_%d", i), org, p, "place-"+id())
	}
	for i, p := range prods {
		a.db.Exec("insert into products(id,organization_id,name,code,unit,active)values(?,?,?,?,?,1)", fmt.Sprintf("prod_%d", i), org, p, fmt.Sprintf("P%d", i+1), "units")
	}
	a.ensureSeedQRCommands(org)
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
	a.tmpl = template.Must(template.New("base").Funcs(template.FuncMap{"abs": func(n int) int {
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

func (a *App) qrMatrix(w http.ResponseWriter, r *http.Request, c Ctx) {
	pid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/places/"), "/qr-matrix")
	rows, _ := a.db.Query(`select q.action,q.amount,pr.name,q.token from qr_commands q join products pr on pr.id=q.product_id where q.organization_id=? and q.place_id=? and q.active=1 and pr.active=1 order by q.action,pr.name,q.amount`, c.OrgID, pid)
	var out []map[string]template.HTML
	for rows.Next() {
		var act, prod, tok string
		var amt int
		rows.Scan(&act, &amt, &prod, &tok)
		svg := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg xmlns='http://www.w3.org/2000/svg' width='96' height='96'><rect width='96' height='96' fill='white'/><rect x='8' y='8' width='80' height='80' fill='none' stroke='black'/><text x='48' y='52' font-size='10' text-anchor='middle'>QR</text></svg>"))
		out = append(out, map[string]template.HTML{"Label": template.HTML(fmt.Sprintf("%s %d %s", act, amt, prod)), "Img": template.HTML(svg)})
	}
	a.render(w, r, "qr", out)
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

const tpl = `{{define "layout"}}<!doctype html><html><head><title>{{.Title}}</title><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/static/app.css"><script src="https://unpkg.com/htmx.org@1.9.12"></script><script src="https://unpkg.com/hyperscript.org@0.9.12"></script></head><body><header><a href="/places">Places</a> <a href="/events">Events</a> <a href="/reports">Reports</a> <a href="/admin">Admin</a>{{if .Ctx.Authed}}<form method="post" action="/logout">{{csrf .Ctx}}<button>Logout</button></form>{{end}}</header><main>{{template "body" .}}</main></body></html>{{end}}
{{define "login"}}{{template "layout" .}}{{end}}{{define "body"}}{{if eq .Title "login"}}<section class="card"><h1>Login</h1><form method="post"><input type="hidden" name="return" value="{{.Data}}"><label>Email<input name="email"></label><label>Password<input name="password" type="password"></label><button>Login</button></form><a href="/register">Register</a></section>{{else}}{{template "body2" .}}{{end}}{{end}}
{{define "body2"}}{{if eq .Title "register"}}<section class="card"><h1>Register</h1><form method="post"><label>Name<input name="name"></label><label>Email<input name="email"></label><label>Password<input name="password" type="password"></label><button>Create account</button></form></section>{{else if eq .Title "waiting"}}<section class="card"><h1>Your account is waiting for approval.</h1><p>Organization: Example Company</p><p>An admin must approve you before you can change inventory.</p></section>{{else if eq .Title "error"}}<section class="card error"><h1>Problem</h1><p>{{index .Data "Message"}}</p></section>{{else if eq .Title "places"}}<h1>Places</h1>{{range .Data}}<a class="row" href="/places/{{.Token}}">{{.Name}}</a>{{end}}{{else if eq .Title "place"}}<h1>{{index .Data "Name"}}</h1>{{range index .Data "Stocks"}}<div class="stock"><b>{{.Name}}</b><span>{{.Qty}} {{.Unit}}</span></div>{{end}}<a href="/events">Recent events</a>{{else if eq .Title "events"}}<h1>Recent events</h1>{{range .Data}}<div class="row"><b>{{.Time}}</b> {{.User}} {{.Qty}} {{.Product}} {{.Place}} <em>{{.Type}}</em></div>{{end}}{{else if eq .Title "admin"}}<h1>Admin</h1><nav class="grid"><a href="/admin/memberships">Memberships</a><a href="/admin/places">Places</a><a href="/admin/products">Products</a><a href="/admin/events">Events</a></nav>{{else if eq .Title "members"}}<h1>Memberships</h1>{{range .Data}}<div class="row">{{.Email}} {{.Status}} <form method="post" action="/admin/memberships/{{.ID}}/approve">{{csrf $.Ctx}}<button>Approve</button></form><form method="post" action="/admin/memberships/{{.ID}}/reject">{{csrf $.Ctx}}<button>Reject</button></form></div>{{end}}{{else if eq .Title "products"}}<h1>Products</h1><p>Product administration is intentionally simple in this MVP; seed products are active.</p>{{else if eq .Title "qr"}}<h1>QR Matrix</h1><div class="qrgrid">{{range .Data}}<div class="qr"><img src="{{.Img}}"><small>{{.Label}}</small></div>{{end}}</div>{{else if eq .Title "reports"}}<h1>Reports</h1><a href="/reports/export.csv">Export CSV</a>{{range .Data}}<div class="row">{{.Time}} {{.Qty}} {{.Product}} {{.Place}}</div>{{end}}{{else if eq .Title "result"}}<section class="result"><h1>{{if gt (index .Data "Delta") 0}}ADDED{{else if lt (index .Data "Delta") 0}}SUBTRACTED{{else}}EVENT{{end}}</h1><div class="big">{{index .Data "Amount"}} × {{index .Data "Product"}}</div><p>{{if gt (index .Data "Delta") 0}}to{{else}}from{{end}} {{index .Data "Place"}}</p><p>Current {{index .Data "Product"}} stock here: <b>{{index .Data "Stock"}} {{index .Data "Unit"}}</b></p>{{if index .Data "Negative"}}<p class="warn">Warning: stock is now negative.</p>{{end}}<div id="undo">{{if not (index .Data "Reversed")}}<form hx-post="/events/{{index .Data "ID"}}/undo" hx-target="#undo" method="post">{{csrf .Ctx}}<button class="danger" _="on load set n to {{index .Data "UndoSeconds"}} then repeat while n > 0 set my.innerText to 'Undo ' + n + 's' wait 1s decrement n end then set my.disabled to true then set my.innerText to 'Undo window expired'">Undo</button></form>{{else}}Undone{{end}}</div><a class="button" href="/places">Scan next</a></section>{{end}}{{end}}
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
{{define "reports"}}{{template "layout" .}}{{end}}
{{define "result"}}{{template "layout" .}}{{end}}
`
