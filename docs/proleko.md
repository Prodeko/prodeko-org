# Proleko?

The guild magazine's pages on the new site, and how its editor-in-chief adds
an issue in the editing screen.

## Pages

| Address | What | Template |
|---|---|---|
| `/fi/palvelut/proleko/` | front page: the newest issue, the eight before it, the years | `layouts/proleko-home.html` |
| `/fi/palvelut/proleko/arkisto/` | every issue since 1999, by year | `layouts/proleko-archive.html` |
| `/fi/palvelut/proleko/verkkojulkaisut/` | the 16 web articles of spring 2020 | `layouts/proleko-posts.html`, `proleko-post.html` |
| `/en/services/proleko/`, `/en/services/proleko/archive/` | the same in English; the articles are Finnish only | as above |

The design follows the editors' sketch (`nettisivuvisiota.pptx`): a navy
masthead with the name in serif capitals, the newest cover large, "Tuoretta
tuotantoa" as a grid, and the years down the side. Every cover opens the PDF.

The old addresses `/fi/palvelut/proleko/archives/`, `/en/services/proleko/archives/`
and `/fi/palvelut/proleko/posts/1/`…`16/` are Hugo aliases. The archive is
public; on the old site it was behind a member login. Moving it behind the
login later means moving the three section folders under `content-members/`.

## Where things are kept

Each issue is a data file and a PDF:

- `site/data/proleko/<year>-<number>.yaml`: year, number, title, the PDF's
  path below `/media/proleko/`, and optionally `cover`, an image in assets.
- The PDF in the server's PDF directory, served at
  `https://prodeko.org/media/proleko/`. **Not in git**: the 102 old issues are
  1.3 GB, 70 of them are over the build's 5 MB limit and one is over GitHub's
  100 MB limit.
- The cover: for the old issues an image in
  `site/assets/images/proleko/kannet/`, which Hugo resizes. For a new issue,
  unless the editor chooses one, the JPEG of the PDF's first page that is
  stored beside the PDF with the same name.

`site.Params.prolekoMedia` in `hugo.toml` is the PDF base address.

## Adding an issue

In **prodeko.org/admin**, collection **Proleko?**, "New Proleko?-lehti":
year, number, title, then the PDF. The PDF field (`static/admin/proleko-pdf.js`)
renders the first page as the cover with pdf.js and sends both to the
proxy's `POST /proleko/pdf` (`proxy/internal/proleko`), which writes them to
`PROLEKO_DIR` as `<year>/proleko-<year>-<number>-<random>.pdf` and `.jpg`.
The field stores that path. Saving and publishing then work like any other
change: a pull request, a preview, and the site rebuilds on publish.

Each upload gets a new file name, so a draft never replaces the PDF that the
published page links to. A replaced PDF, or the PDF of a draft that is thrown
away, stays on the disk with nothing linking to it.

The editor-in-chief needs ordinary website editor rights (`EDITOR_ROLES`).
The upload is behind the same session and role check as the rest of the
editing screen.

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
5. **Keycloak**: give the editor-in-chief the website editor roles.
6. Keep static.prodeko.org's `media/proleko/` in place: the old PDF links
   are in emails and chats.

Until steps 1–4 are done, the pages and old covers work but the PDF links
answer 404 and the PDF field cannot upload, so deploy the infra before merging
this to `main`.

## Re-running the migration

`tools/proleko/old-archive.txt` is the old archive's list as it stood on
2026-11-25, saved from the logged-in page.
`python3 tools/import-proleko.py --post-images` rewrites the data files and
fetches any missing cover or article picture. It does not touch the
hand-converted articles in `content/fi/palvelut/proleko/verkkojulkaisut/`.
