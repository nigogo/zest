# Zest QR Inventory Movement

Zest is a small, server-rendered Go application for applying inventory movements at physical places by scanning opaque QR command-token URLs. Inventory is append-only: stock is derived from `inventory_events`, and undo creates a reversal event rather than deleting or mutating history.

## Architecture

- Go `net/http` server in `cmd/server/main.go`.
- Internationalization resolves each request from the user's saved language or, if unset, the browser/device `Accept-Language` header.
- SQLite schema in `migrations/001_init.sql`.
- Server-rendered HTML with mobile-first CSS in `static/app.css`.
- `.templ` placeholders are included under `templates/` to keep the project shaped for templ adoption; the MVP renders through Go `html/template` to avoid generated-code requirements.
- HTMX powers the scan-result undo action; hyperscript powers the visible countdown.
- SQLite persistence uses the pure-Go `modernc.org/sqlite` driver, so local development does not require a system `sqlite3` executable.

## Environment

The server loads environment variables from `.env` by default before reading configuration. Values already exported in the shell take precedence, and production servers can replace `.env` or set `ENV_FILE=/path/to/envfile`.

Defaults in the committed `.env` file:

```txt
APP_ENV=development
APP_BASE_URL=http://localhost:8765
APP_ADDR=:8765
DATABASE_PATH=./data/app.db
SESSION_SECRET=dev-secret-change-me
UNDO_WINDOW_SECONDS=20
COMMON_AMOUNTS=10,20,40,60
SEED_ADMIN_EMAIL=admin@example.com
SEED_ADMIN_PASSWORD=admin123-change-me
SEED_USER_EMAIL=user@example.com
SEED_USER_PASSWORD=password
```

When `APP_ENV=production`, set `APP_BASE_URL` to the public production URL and replace `SESSION_SECRET` and `SEED_ADMIN_PASSWORD`; the server refuses to start with the local defaults for those values.

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

Users can choose English, German, or device default from `/settings`; that preference is stored in SQLite.

## Seed Credentials

Development admin:

```txt
admin@example.com
admin123-change-me
```

Development approved operator:

```txt
user@example.com
password
```

These credentials are for local development only. Override them with `SEED_ADMIN_EMAIL`, `SEED_ADMIN_PASSWORD`, `SEED_USER_EMAIL`, and `SEED_USER_PASSWORD`.

## QR Command Tokens

In development, `/dev/qr-codes` lists every active QR command as a clickable card so you can simulate scanning without a camera.

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

## UI structure and design tokens

Zest's interface is intentionally server-rendered and mobile-first. The current MVP keeps reusable UI concepts in the Go `html/template` bundle in `cmd/server/main.go`, while the `.templ` files remain as placeholders for a later templ migration. The primary reusable patterns are:

- **App shell and mobile header**: a compact Zest wordmark, organization/status context, and small navigation actions for phone use.
- **Operator cards**: scan result hero cards, product stock cards, event timeline cards, empty states, status banners, and sticky bottom action bars.
- **Admin shell**: a responsive admin navigation area with metric cards, table cards, report filters, and coming-soon placeholders for unimplemented backoffice features.
- **QR matrix layout**: print-oriented QR cards with human-readable labels, separated add/subtract sections, and print CSS.
- **Auth and error states**: branded login, registration, waiting approval, and problem pages with concise operational copy.

The visual system lives in `static/app.css` and is organized by section: reset/base, variables, typography, layout, buttons, forms, cards, product identity, operator mobile UI, admin UI, QR matrix, status/alerts, utilities, and print.

Key token groups include:

- **Brand tokens**: `--zest-50`, `--zest-100`, `--zest-300`, `--zest-500`, and `--zest-600`.
- **Neutral tokens**: `--ink-900`, `--ink-700`, `--ink-500`, `--ink-300`, `--ice-50`, `--ice-100`, and `--surface`.
- **Semantic tokens**: `--success`, `--warning`, `--danger`, `--info`, plus matching soft background tokens.
- **Product tokens**: Lemon, Lime, Orange, and Grapefruit each have a strong accent token and a soft background token. Product color is always paired with product text and never used as the only indicator.
- **Layout and interaction tokens**: spacing scale (`--space-*`), radius scale (`--radius-*`), shadows (`--shadow-*`), `--tap` for 44px minimum tap targets, and width tokens for operator/admin layouts.

The scan result undo panel uses HTMX for the server-backed undo request and hyperscript for the visible countdown/disabled state. The server remains authoritative for the undo window and writes a reversal event instead of changing or deleting the original inventory event.

The logo mark is stored as `static/zest-logo.png` and used in the mobile header, auth pages, and printable QR matrix branding.
