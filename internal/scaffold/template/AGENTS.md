# [[.Title]]

A [tinypaas](https://github.com/parthiv-krishna/tinypaas) app: a single static Go
binary served at `https://<apps-domain>/[[.Name]]/` behind an OIDC login.
Pushing to `main` builds it with Nix and deploys it (`.forgejo/workflows/deploy.yaml`).

## Contract

- **Listen on `127.0.0.1:$PORT`.** Fall back to 8080 when `PORT` is unset.
- **State lives in the working directory.** It is the app's persistent data
  directory: use `./app.db` (SQLite via pure-Go `modernc.org/sqlite`) and plain
  files. Never write next to the binary; it is replaced on deploy.
- **Prefix is stripped.** Requests to `/[[.Name]]/foo` arrive as `/foo`, so register
  routes as if the app lives at `/`.
- **HTML must use relative URLs**: `href="static/app.css"`, `action="notes"`,
  `fetch("api/items")`, `href="../"` (back to the app index). Never `href="/..."`.
  Root-relative *redirects* (`http.Redirect(w, r, "/", ...)`) and cookie paths are
  rewritten for you; links in HTML/JS are not.
- **Identity is in headers** (already verified; spoofed copies are stripped):
  `X-Forwarded-User`, `X-Forwarded-Email`, `X-Forwarded-Name`,
  `X-Forwarded-Groups` (comma-separated). `X-Forwarded-Prefix` is `/[[.Name]]`.
  Every visitor is signed in, so there is no login to build.
- **Everything ships in the binary**: templates and static assets via `go:embed`.
- **Static build**: `CGO_ENABLED=0`. No cgo dependencies (so no `mattn/go-sqlite3`).
- **Handle schema changes in code**: run `CREATE TABLE IF NOT EXISTS` /
  additive migrations at startup; data persists across deploys.
- `app.json` holds `name` (URL slug, never change it after first deploy), `title` and
  `description` shown on the apps index.

## Developing

```sh
nix develop          # go, gopls, sqlite
go run .             # http://127.0.0.1:8080/ (identity headers absent: user "dev")
nix build            # must succeed before pushing; produces result/bin/app
```

After changing dependencies in `go.mod`, update `vendorHash` in `flake.nix`: set it
to `pkgs.lib.fakeHash`, run `nix build`, and paste the `got:` hash.

## Operating (on the tinypaas host)

`tinypaas ls`, `tinypaas logs [[.Name]]`, `tinypaas restart [[.Name]]`,
`tinypaas rollback [[.Name]]`. A deploy that does not start listening within 30s
is rejected and the previous release keeps running.
