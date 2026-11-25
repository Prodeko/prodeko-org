# Proleko?

The guild magazine's pages on the new site, and how its editor-in-chief adds
an issue without being a website editor.

## Pages

| Address | What | Template |
|---|---|---|
| `/fi/palvelut/proleko/` | front page: the newest issue, the eight before it, the years | `layouts/proleko-home.html` |
| `/fi/palvelut/proleko/arkisto/` | every issue since 1999, by year | `layouts/proleko-archive.html` |
| `/fi/palvelut/proleko/verkkojulkaisut/` | the 16 web articles of spring 2020 | `layouts/proleko-posts.html`, `proleko-post.html` |
| `/en/services/proleko/`, `/en/services/proleko/archive/` | the same in English; the articles are Finnish only | as above |
| `/fi/palvelut/proleko/lehdet.json` | the archive as JSON, for the upload page | `layouts/proleko-home.prolekoissues.json` |
| `/admin/proleko/` | the upload page | `static/admin/proleko/index.html` |

The design follows the editors' sketch (`nettisivuvisiota.pptx`): a navy
masthead with the name in serif capitals, the newest cover large, "Tuoretta
tuotantoa" as a grid, and the years down the side. Every cover opens the PDF.

The old addresses `/fi/palvelut/proleko/archives/`, `/en/services/proleko/archives/`
and `/fi/palvelut/proleko/posts/1/`…`16/` are Hugo aliases. The archive is
public; on the old site it was behind a member login. Moving it behind the
login later means moving the three section folders under `content-members/`.

## Where things are kept

Each issue is three things:

- `site/data/proleko/<year>-<number>.yaml`: year, number, title, the PDF's
  path below `/media/proleko/`, and the cover's asset path.
- `site/assets/images/proleko/kannet/<year>-<number>.(webp|jpg|png)`: the
  cover. Hugo resizes it.
- `<year>/proleko-<year>-<number>.pdf` in the server's PDF directory, served at
  `https://prodeko.org/media/proleko/`. **Not in git**: the 102 old issues are
  1.3 GB, 70 of them are over the build's 5 MB limit and one is over GitHub's
  100 MB limit.

`site.Params.prolekoMedia` in `hugo.toml` is the PDF base address.

## Adding an issue

The editor-in-chief opens **prodeko.org/admin/proleko/**, signs in with their
Prodeko account, and fills in year, number, title and the PDF. The page renders
the PDF's first page as the cover (or takes an image they choose) and sends
everything to the proxy's `POST /proleko/issues` (`proxy/internal/proleko`),
which:

1. writes the PDF to `PROLEKO_DIR/<year>/proleko-<year>-<number>.pdf`;
2. commits the data file and the cover on a `proleko/…` branch, authored by
   the editor-in-chief, and opens a pull request;
3. merges it. If `main` requires a review the pull request stays open and the
   page says so, with the link.

The site rebuilds on the merge, so the issue appears a few minutes later. The
same form with an existing year and number updates the title or cover; the
PDF is then optional.

Who may upload: anyone with the website editor roles (`EDITOR_ROLES`), and
anyone with every role in `PROLEKO_ROLES`. The second group cannot use
`/admin` or `/github/*` at all.

## Deployment checklist

The site and proxy code are in this repository; the rest is infra-prodeko,
Keycloak and the server.

1. **PDF directory on prodeko-vm2**, outside the site's web root: the publish
   step replaces the site tree, so anything inside it would be deleted on every
   deploy. For example `/srv/proleko`, owned by the proxy container's user
   (uid 65532 in the distroless image), and in the server backups.
2. **Copy the old PDFs** into it. On a machine with the disk space, or on the
   server itself:

   ```
   python3 tools/import-proleko.py --pdf-dir /srv/proleko
   ```

   This downloads the 102 PDFs from static.prodeko.org (1.3 GB) under the new
   names. Existing files are skipped, so it can be rerun.
3. **Caddy**: serve `/media/proleko/*` from that directory on prodeko.org,
   with `Content-Type: application/pdf`. Add a redirect from
   `/en/services/proleko/posts/*` to `/fi/palvelut/proleko/posts/*`. The
   articles are Finnish only, and the Finnish aliases point on to the new
   addresses.
4. **Proxy container**: mount the directory and set `PROLEKO_DIR=/srv/proleko`.
   The proxy refuses to start if the directory is missing or not writable.
5. **Keycloak**: create a realm role such as `proleko-editor`, grant it to the
   editor-in-chief, and set `PROLEKO_ROLES=membership,proleko-editor` on the
   proxy. With `membership` included, the right lapses with the membership,
   as it does for editors.
6. Keep static.prodeko.org's `media/proleko/` in place: the old PDF links
   are in emails and chats.

Until steps 1–4 are done, the covers and pages work but the PDF links answer
404, so deploy the infra before merging this to `main`.

## Re-running the migration

`tools/proleko/old-archive.txt` is the old archive's list as it stood on
2026-11-25, saved from the logged-in page.
`python3 tools/import-proleko.py --post-images` rewrites the data files and
fetches any missing cover or article picture. It does not touch the
hand-converted articles in `content/fi/palvelut/proleko/verkkojulkaisut/`.
