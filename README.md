# Zest QR Inventory Movement

Zest is a small, server-rendered Go application for applying inventory movements at physical places by scanning opaque QR command-token URLs. Inventory is append-only: stock is derived from `inventory_events`, and undo creates a reversal event rather than deleting or mutating history.

## Architecture

- Go `net/http` server in `cmd/server/main.go`.
- SQLite schema in `migrations/001_init.sql`.
- Server-rendered HTML with mobile-first CSS in `static/app.css`.
- `.templ` placeholders are included under `templates/` to keep the project shaped for templ adoption; the MVP renders through Go `html/template` to avoid generated-code requirements.
- HTMX powers the scan-result undo action; hyperscript powers the visible countdown.
- SQLite persistence uses the pure-Go `modernc.org/sqlite` driver, so local development does not require a system `sqlite3` executable.

## Environment

Defaults:

```txt
APP_ENV=development
APP_BASE_URL=http://localhost:8080
APP_ADDR=:8080
DATABASE_PATH=./data/app.db
SESSION_SECRET=dev-secret-change-me
UNDO_WINDOW_SECONDS=20
COMMON_AMOUNTS=10,20,40,60
SEED_ADMIN_EMAIL=admin@example.com
SEED_ADMIN_PASSWORD=admin123-change-me
```

## Setup and Run

```sh
go test ./...
go run ./cmd/server
```

With air:

```sh
air
```

The database migrates and development seed data is created on first startup.

## Seed Credentials

Development admin:

```txt
admin@example.com
admin123-change-me
```

These credentials are for local development only. Override them with `SEED_ADMIN_EMAIL` and `SEED_ADMIN_PASSWORD`.

## QR Command Tokens

Inventory QR codes point to `/scan/:commandToken`. The token maps to a `qr_commands` row containing organization, place, product, action, and amount. Product and amount are never accepted from editable query parameters. Inactive commands, places, or products are rejected before an event is created.

## Undo

The scan result page shows an HTMX undo button with a 20-second countdown. The server enforces the same undo window. Undo inserts a `reversal` event with the opposite quantity and `reversed_event_id`; double undo is blocked.

## Reports

`/reports` lists recent event data and `/reports/export.csv` exports event rows as CSV. Reports are event-based.

## Known Limitations

- QR images are placeholder SVG data URIs in this environment; swap in a real QR generator before production printing.
- Admin place/product creation routes are intentionally minimal in this MVP.
- Password hashing is structured in one helper; replace with Argon2id/bcrypt when external crypto modules are available.

## Next Steps

- Replace placeholder QR rendering with inline SVG QR generation.
- Move inline templates into generated templ components.
- Add richer admin create/deactivate flows.
- Add idempotency-key support for future in-app scanners.
