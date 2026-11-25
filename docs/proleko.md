# Proleko?

The guild magazine's pages on the new site, and how its editor-in-chief adds
an issue in the editing screen.

## Pages

Members only: the pages are in the member tree (`content-members/`) and Caddy
serves them, and the PDFs, only to someone signed in with the `membership`
realm role, as on the old site.

| Address | What | Template |
|---|---|---|
| `/fi/jasenille/proleko/` | front page: the newest issue, the eight before it, the years | `layouts/proleko-home.html` |
| `/fi/jasenille/proleko/arkisto/` | every issue since 1999, by year | `layouts/proleko-archive.html` |
| `/fi/jasenille/proleko/verkkojulkaisut/` | the 16 web articles of spring 2020 | `layouts/proleko-posts.html`, `proleko-post.html` |
| `/en/members/proleko/`, `/en/members/proleko/archive/` | the same in English; the articles are Finnish only | as above |

The design follows the editors' sketch (`nettisivuvisiota.pptx`): a navy
masthead with the name in serif capitals, the newest cover large, "Tuoretta
tuotantoa" as a grid, and the years down the side. Every cover opens the PDF.

The menus name the pages with `members: true`, so the public build shows
them as plain text and never prints a member address.

Every image is under the gate as well. Hugo publishes processed images at
`/images/…`, which the gate does not cover, so `partials/proleko-gated.html`
copies each cover and banner to `/fi/jasenille/proleko/kuvat/`. The
articles' own pictures are files in the section,
`verkkojulkaisut/kuvat/`.

The old addresses are redirected by Caddy: `/fi/palvelut/proleko/…` to
`/fi/jasenille/proleko/…` and `/en/services/proleko/…` to
`/en/members/proleko/…`. Inside the member tree, `archives/` and
`posts/1/`…`16/` are Hugo aliases of the new pages, so every old link
lands on its page after sign-in.

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

The host side is in Prodeko/infrastructure#39: the `cms_auth_proxy` role
creates `/srv/proleko` (owned by the container's uid 65532), mounts it into
the proxy and sets `PROLEKO_DIR`, and the Caddyfile serves it at
`/media/proleko/` behind the member gate, redirects the old addresses into
`/fi/jasenille/proleko/` and `/en/members/proleko/`, and points
proleko.prodeko.org at the new pages.

1. Merge that pull request and run
   `ansible-playbook playbooks/prodeko_vm.yml --tags caddy,cms_auth_proxy`.
2. Copy the old PDFs in once, on the server or anywhere with 1.3 GB free:

   ```
   python3 tools/import-proleko.py --pdf-dir /srv/proleko
   sudo chown -R 65532:65532 /srv/proleko
   ```

   This downloads the 102 PDFs from static.prodeko.org under the new names.
   Existing files are skipped, so it can be rerun.
3. **Keycloak**: give the editor-in-chief the website editor roles.
4. Merge this. The proxy image deployed from `main` then serves the upload.
5. Keep static.prodeko.org's `media/proleko/` in place: the old PDF links
   are in emails and chats, and it is the only other copy of the old issues.

`/srv/proleko` is not backed up: the VM has no backup. An issue uploaded after
the move exists only there, so the editor-in-chief should keep their own copy
of each PDF until there is one.

Until steps 1–2 are done the PDF links answer 404, so do them before merging
this to `main`.

## Re-running the migration

`tools/proleko/old-archive.txt` is the old archive's list as it stood on
2026-11-25, saved from the logged-in page.
`python3 tools/import-proleko.py --post-images` rewrites the data files and
fetches any missing cover or article picture. It does not touch the
hand-converted articles in `content-members/fi/jasenille/proleko/verkkojulkaisut/`.
