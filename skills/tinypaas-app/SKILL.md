---
name: tinypaas-app
description: Build, adapt or debug a small personal web app (Go + SQLite) for tinypaas, the homelab app host at apps.sub0.net. Use when the user wants a quick utility/tool/"little app" hosted on their apps domain, or when working in a repo that has a tinypaas app.json.
---

# Building a tinypaas app

tinypaas runs each app as **one static binary** under `https://apps.sub0.net/<name>/`,
behind a forced OIDC login, with a persistent working directory for state. Apps are
**Go + SQLite**, built with Nix, and deployed by Forgejo Actions on push to `main`.
There is no per-app server config: everything below is the whole contract.

## 1. Scaffold

```sh
nix run github:parthiv-krishna/tinypaas -- new -description "One line for the index" my-app
cd my-app && git init -b main
```

`my-app` must be a lowercase slug (`a-z`, `0-9`, `-`). It becomes the URL path and must
not change after the first deploy (it also names the data directory). The scaffold is a
working notes app: `main.go`, `templates/`, `static/`, `app.json`, `flake.nix`,
`.forgejo/workflows/deploy.yaml`, and `AGENTS.md` (the contract, for future agents).
Replace the notes logic with the requested app; keep the structure.

## 2. The contract

1. **Listen on `127.0.0.1:$PORT`** (fall back to `8080` locally). Never `:$PORT` or `0.0.0.0`.
2. **Working directory = persistent data dir.** Store state in `./app.db` (SQLite) or
   files under `.`; it survives deploys and is backed up. Nothing else persists.
3. **The `/<name>` prefix is stripped.** Register routes as if the app is at `/`
   (`GET /{$}`, `POST /items`, `GET /api/items`).
4. **Use relative URLs in HTML, CSS and JS.** This is the one rule that breaks apps:
   - `href="static/app.css"`, `src="static/app.js"`, `action="items"`,
     `fetch("api/items")`, `href="items/42"`, back to index: `href="./"`;
     to the apps index: `href="../"`.
   - Never `href="/..."`, `fetch("/api/...")`, or `<base href="/">`.
   - Relative URLs resolve against the current path, so prefer a flat URL space or
     compute `../` correctly from nested pages. In JS, `new URL("api/x", document.baseURI)`.
   - Server-side redirects may be root-relative (`http.Redirect(w, r, "/", 303)`);
     tinypaas rewrites `Location` and cookie `Path`s. It does **not** rewrite bodies.
5. **Identity comes from trusted headers** (inbound copies are stripped):
   `X-Forwarded-User`, `X-Forwarded-Email`, `X-Forwarded-Name`,
   `X-Forwarded-Groups` (comma-separated). Every request is already authenticated, so
   never build login/sessions. Absent headers mean local dev: treat as user `dev`.
   Use the user for per-user data or attribution only if the app needs it.
6. **One self-contained static binary.** `CGO_ENABLED=0`; embed templates and assets
   with `go:embed`; SQLite via `modernc.org/sqlite` (never `mattn/go-sqlite3`). No
   runtime files next to the binary, no external services, no secrets.
7. **Migrate at startup** with idempotent SQL (`CREATE TABLE IF NOT EXISTS`, additive
   `ALTER TABLE`). Keep `db.SetMaxOpenConns(1)` and the WAL/busy_timeout pragmas.
8. **Start fast.** A release that is not listening within 30s is rejected and the
   previous release keeps running. Log to stdout/stderr.
9. **`app.json`** is `{ "name", "title", "description" }`, shown on the apps index.

## 3. Style guidance

- Prefer the standard library: `net/http` (Go 1.22+ method/path patterns),
  `html/template`, `database/sql`. Add dependencies only when they clearly pay off.
- Server-rendered HTML with plain forms (POST → 303 redirect) is the default. For
  interactivity use small vanilla JS in `static/` (or vendor htmx into `static/`).
  No npm, no bundlers, no CDNs.
- Make pages work on mobile (`<meta name="viewport">`, simple responsive CSS) and
  support dark mode via `color-scheme: light dark`.
- Keep it small: one `main.go` is fine; split files only when it gets unwieldy.

## 4. Verify before pushing

```sh
nix develop -c go vet ./...
nix develop -c go run .        # check http://127.0.0.1:8080/ in a browser or with curl
nix build -L                    # must pass; this is exactly what CI deploys
```

Also check, e.g. with `grep -rnE '(href|src|action)="/' templates static` and a look at
any `fetch(` calls, that no root-relative URLs slipped in.

When `go.mod` dependencies change, `nix build` fails with a `vendorHash` mismatch:
set `vendorHash = pkgs.lib.fakeHash;` in `flake.nix`, run `nix build`, and paste
the `got: sha256-...` value. Commit `go.sum`.

## 5. Deploy

Create a repo on Forgejo (`https://git.sub0.net`, ask the user if you cannot) and push
`main`. The included workflow runs on the `tinypaas` runner:
`nix build` → `tinypaas deploy result/bin/app`. The app then appears on
`https://apps.sub0.net/`.

On the host, members of the `tinypaas` group can operate apps:
`tinypaas ls`, `tinypaas logs <name>`, `tinypaas restart <name>`,
`tinypaas rollback <name>` (swaps current/previous), `tinypaas rm [-purge] <name>`.

## Debugging checklist

- Blank styling / 404s on assets or API calls → a root-relative URL (rule 4).
- 502 "not responding" → crashed or not on `127.0.0.1:$PORT`; see `tinypaas logs <name>`.
- Deploy rejected "did not listen" → slow startup, wrong address, or a crash on boot.
- Data "lost" after deploy → written somewhere other than the working directory.
- Build fails on `cgo`/`gcc` → a cgo dependency crept in; replace it.
