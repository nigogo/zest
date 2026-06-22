package main

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
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
	a := &App{db: db, base: "http://example.test", undoWindow: 20 * time.Second, amounts: []int{10, 20, 40, 60}}
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
