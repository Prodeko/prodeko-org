#!/usr/bin/env python3
"""Move the Proleko? archive from old.prodeko.org to the new site.

The old archive is a member-only page listing 102 issues, each a PDF and a
cover picture on static.prodeko.org. This turns that list into:

  - one data file per issue in site/data/proleko/, which is what the archive
    page renders and what the editors' upload page writes for a new issue;
  - one cover per issue in site/assets/images/proleko/kannet/, shrunk to
    600 px wide so 102 of them stay small enough for git;
  - with --pdf-dir, the PDFs themselves under a directory outside the
    repository, named the same way the upload page names new ones. They are
    1.3 GB together and several are over GitHub's 100 MB limit, so they are
    served from the server's disk at /media/proleko/, never committed. Copy the
    directory to the server's media volume after running this.

The list itself is tools/proleko/old-archive.txt, saved from the logged-in
page, because the page cannot be fetched without a member login.

The 16 web articles from spring 2020 were converted by hand into
site/content/fi/palvelut/proleko/verkkojulkaisut/. --post-images downloads the
pictures those pages refer to.

Run from the repository root. Needs cwebp (libwebp) for the covers.

    python3 tools/import-proleko.py                       # data and covers
    python3 tools/import-proleko.py --pdf-dir ~/proleko   # and the PDFs
    python3 tools/import-proleko.py --post-images         # article pictures

Existing data files are overwritten; covers, PDFs and pictures that already
exist are kept, so a rerun only fetches what is missing.
"""

import argparse
import shutil
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

SOURCE = Path("tools/proleko/old-archive.txt")
STATIC = "https://static.prodeko.org/media/proleko/archives/"
DATA_DIR = Path("site/data/proleko")
COVER_DIR = Path("site/assets/images/proleko/kannet")
COVER_WIDTH = 600

# Pictures in the 2020 web articles: (where the old site had it, the file name
# the converted page uses). Two articles both had a kuva-2 and a kuva-3, so the
# later ones were renamed.
CK = "https://static.prodeko.org/media/ckeditor_uploads/2020/"
BANNERS = "https://static.prodeko.org/media/proleko/posts/2020/"
POST_IMAGE_DIR = Path("site/static/uploads/proleko/verkkojulkaisut")
BANNER_DIR = Path("site/assets/images/proleko/verkkojulkaisut")
POST_IMAGES = [
    ("04/14/koronapositiivisuus_final_I7seHcp.jpg", None),
    ("04/15/kuva1proleko.jpg", None),
    ("04/15/zoom2.jpg", None),
    ("04/15/kimmoov2.jpg", None),
    ("04/15/kimmoo2v2.jpg", None),
    ("04/15/_dkeskustelu_XmNAmjy.jpg", None),
    ("04/21/prodeko.png", None),
    ("04/21/mycourses.png", None),
    ("04/21/omistajuuskiista.png", None),
    ("04/21/arts.png", None),
    ("04/21/slush.png", None),
    ("04/21/ruotsi.png", None),
    ("04/22/kuva1.jpg", None),
    ("04/22/kuva-2.jpg", None),
    ("04/22/kuva-3.jpg", None),
    ("04/29/paavo1.jpg", None),
    ("04/29/zoom.png", None),
    ("04/29/tois.png", None),
    ("04/29/paavo2.jpg", None),
    ("04/29/kukka.jpg", None),
    ("04/29/netflix_chill.jpg", None),
    ("04/29/paavo3.jpg", None),
    ("04/29/paavo4.jpg", None),
    ("04/30/kalsarikannit.jpeg", None),
    ("04/30/tiger-king.jpg", None),
    ("04/30/koff.jpg", None),
    ("05/29/kuva-1.jpg", None),
    ("05/29/kuva-2.jpg", "kuva-2-traktori.jpg"),
    ("05/29/lukashenko3.jpg", None),
    ("05/29/kuva-3.png", "kuva-3-vuohet.png"),
]
POST_BANNERS = [
    "Paakirjoitus_22020.jpg", "banneri_koronapositiivisuus.png",
    "banneri_etakokousetiketti.png", "bannermielikuvitusystava.png",
    "bannerinetflixsuositukset.png", "banneri_kaksoispistedee.png",
    "banneritiktokinfluensseri.png", "bannerilukemiset.png",
    "banneri_runetappelu.png", "banneri_escape.png", "Paavo_Banner.jpg",
    "bannerisitsit.png", "bannerimarkets.png", "banneri_fillari.png",
    "Lukashenko.jpg", "Made_artikkeli.jpg",
]


def read_archive():
    issues = []
    for line in SOURCE.read_text(encoding="utf-8").splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        year, number, title, pdf, cover = line.split("|")
        n = int(number.split("/")[0])
        issues.append({
            "year": int(year),
            "number": n,
            # The old site filled an untitled issue's title with its number.
            "title": "" if title == number else title,
            "old_pdf": pdf,
            "old_cover": cover,
        })
    return issues


def slug(issue):
    return f"{issue['year']}-{issue['number']}"


def pdf_path(issue):
    """Where the PDF lives below /media/proleko/. The upload page uses the
    same scheme, so old and new issues sit side by side."""
    return f"{issue['year']}/proleko-{slug(issue)}.pdf"


def yaml_str(s):
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def write_data(issue):
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    lines = [
        f"year: {issue['year']}",
        f"number: {issue['number']}",
        f"title: {yaml_str(issue['title'])}",
        f"pdf: {pdf_path(issue)}",
        f"cover: images/proleko/kannet/{slug(issue)}.webp",
    ]
    (DATA_DIR / f"{slug(issue)}.yaml").write_text("\n".join(lines) + "\n", encoding="utf-8")


def fetch(url, dest):
    dest.parent.mkdir(parents=True, exist_ok=True)
    tmp = dest.with_suffix(dest.suffix + ".part")
    req = urllib.request.Request(url, headers={"User-Agent": "prodeko-org importer"})
    with urllib.request.urlopen(req, timeout=120) as resp, open(tmp, "wb") as out:
        shutil.copyfileobj(resp, out)
    tmp.rename(dest)


def image_width(path):
    """Width of a PNG or JPEG, read from its header; None if unknown."""
    data = path.read_bytes()
    if data[:8] == b"\x89PNG\r\n\x1a\n":
        return int.from_bytes(data[16:20], "big")
    if data[:2] == b"\xff\xd8":
        i = 2
        while i + 9 < len(data):
            if data[i] != 0xFF:
                i += 1
                continue
            marker = data[i + 1]
            length = int.from_bytes(data[i + 2:i + 4], "big")
            if marker in (0xC0, 0xC1, 0xC2):
                return int.from_bytes(data[i + 7:i + 9], "big")
            i += 2 + length
    return None


def fetch_webp(url, dest, max_width):
    """Download a picture and store it as WebP no wider than max_width. The
    originals are screenshots and camera files of several megabytes each,
    which do not belong in git at that size."""
    if dest.exists():
        return
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / Path(url).name
        fetch(url, src)
        dest.parent.mkdir(parents=True, exist_ok=True)
        cmd = ["cwebp", "-quiet", "-q", "80"]
        width = image_width(src)
        if width is None or width > max_width:
            cmd += ["-resize", str(max_width), "0"]
        subprocess.run(cmd + [str(src), "-o", str(dest)], check=True)


def write_cover(issue):
    fetch_webp(STATIC + issue["old_cover"], COVER_DIR / f"{slug(issue)}.webp", COVER_WIDTH)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--pdf-dir", type=Path, help="also download every PDF into this directory")
    ap.add_argument("--post-images", action="store_true", help="download the 2020 web articles' pictures")
    args = ap.parse_args()

    if not SOURCE.exists():
        sys.exit("run from the repository root")
    if not shutil.which("cwebp"):
        sys.exit("cwebp is needed for the covers (brew install webp / apt install webp)")

    issues = read_archive()
    for issue in issues:
        write_data(issue)
        write_cover(issue)
        if args.pdf_dir:
            dest = args.pdf_dir.expanduser() / pdf_path(issue)
            if not dest.exists():
                print("pdf", pdf_path(issue), flush=True)
                fetch(STATIC + issue["old_pdf"], dest)
    print(f"{len(issues)} issues: data in {DATA_DIR}, covers in {COVER_DIR}")

    if args.post_images:
        for src, name in POST_IMAGES:
            stem = Path(name or src).stem
            fetch_webp(CK + src, POST_IMAGE_DIR / f"{stem}.webp", 1200)
        for name in POST_BANNERS:
            fetch_webp(BANNERS + name, BANNER_DIR / f"{Path(name).stem}.webp", 1800)
        print(f"{len(POST_IMAGES)} pictures in {POST_IMAGE_DIR}, {len(POST_BANNERS)} banners in {BANNER_DIR}")


if __name__ == "__main__":
    main()
