#!/usr/bin/env python3
"""Export the plain-text pages of the live prodeko.org into site/content/.

This is a stand-in for the real converter described in
docs/superpowers/specs/2026-09-18-prodeko-git-cms-design.md (which reads the
Django database dump we don't have here). It scrapes the live HTML instead,
but produces the same shape of output: one Markdown file per page, with
front matter, placed at the exact path the live page already has, so no
redirects are needed.

Run from the repository root:

    python3 tools/export-content.py [--dry-run]

What it deliberately does NOT touch:
  - The two homepage roots (/fi/, /en/), which keep their existing
    hand-written intro text.
  - The board, officials and honours roster/archive pages: those are driven
    by site/data/*.yaml and layouts/people.html or layouts/archive.html
    (see docs/roadmap.md item E), which already hold better-structured,
    hand-verified data than a blind table scrape would produce. The full
    scrape of those pages (2500+ names) is migration work, item H, not
    seed content.

Front matter intentionally has no `reviewed` or `owner` field. This content
has not been reviewed by a human yet; claiming otherwise would defeat the
point of those fields (see the design doc's "a review date" section).
Instead each page carries `sourceURL` so a human can find and check the
original.
"""

import argparse
import re
import sys
import time
from pathlib import Path
from urllib.parse import urljoin, urlparse

import requests
import yaml
from bs4 import BeautifulSoup

REPO_ROOT = Path(__file__).resolve().parent.parent
CONTENT_ROOT = REPO_ROOT / "site" / "content"
SITEMAP_URL = "https://prodeko.org/sitemap.xml"
USER_AGENT = "prodeko-org-content-export/0.1 (+https://github.com/prodeko/prodeko-org)"

SKIP_PATHS = {
    "/fi/",
    "/en/",
    # Board, officials, honours: hand-curated from the same live pages,
    # structured as YAML data rather than scraped prose. See module docstring.
    "/fi/guild/arvot/",
    "/en/guild/values/",
    "/fi/guild/board/hallitus/",
    "/en/guild/board/board/",
    "/fi/guild/board/edelliset-hallitukset/",
    "/en/guild/board/previous-boards/",
    "/fi/guild/toimarit/toimihenkilot/",
    "/en/guild/guild-officials/guild-officials/",
    "/fi/guild/toimarit/edelliset-toimihenkilot/",
    "/en/guild/guild-officials/previous-guild-officials/",
    "/fi/guild/kunnianosoitukset/",
    "/en/guild/honour/",
}

BLOCK_TAGS = {"p", "div", "section", "article", "li", "blockquote"}
DROP_TAGS = {"script", "style", "nav"}
RAW_PASSTHROUGH_TAGS = {"iframe", "form", "svg", "video", "table"}


def fetch(session, url):
    resp = session.get(url, timeout=20, allow_redirects=True)
    resp.raise_for_status()
    return resp


def tail_parts(url_path):
    parts = [p for p in url_path.strip("/").split("/") if p]
    return parts[1:]  # drop the /fi/ or /en/ language prefix


def is_section(path, all_paths):
    return any(other != path and other.startswith(path) for other in all_paths)


def local_file_for(url_path, all_paths):
    lang = url_path.strip("/").split("/")[0]
    parts = tail_parts(url_path)
    if not parts:
        return None
    if is_section(url_path, all_paths):
        return CONTENT_ROOT / lang / Path(*parts) / "_index.md"
    *dirs, last = parts
    return CONTENT_ROOT / lang / Path(*dirs, last + ".md")


def inline_text(node):
    return re.sub(r"\s+", " ", node.get_text())


def render(node):
    """Recursively render a BeautifulSoup node to Markdown."""
    out = []
    for child in getattr(node, "children", []):
        if isinstance(child, str):
            out.append(re.sub(r"\s+", " ", child))
            continue
        name = child.name
        if name in DROP_TAGS:
            continue
        if name in RAW_PASSTHROUGH_TAGS:
            out.append("\n\n" + str(child) + "\n\n")
        elif name == "br":
            out.append("\n")
        elif name in ("h1", "h2", "h3", "h4", "h5", "h6"):
            text = inline_text(child).strip()
            if text:
                level = min(max(2, int(name[1])), 6)  # h1 is reserved for the page title
                out.append("\n\n" + "#" * level + " " + text + "\n\n")
        elif name == "p":
            text = render(child).strip()
            if text:
                out.append("\n\n" + text + "\n\n")
        elif name in ("ul", "ol"):
            items = []
            for i, li in enumerate(child.find_all("li", recursive=False), start=1):
                bullet = "-" if name == "ul" else f"{i}."
                items.append(f"{bullet} {render(li).strip()}")
            out.append("\n\n" + "\n".join(items) + "\n\n")
        elif name == "a":
            href = child.get("href", "")
            text = render(child).strip() or href
            out.append(f"[{text}]({href})")
        elif name in ("strong", "b"):
            text = render(child).strip()
            out.append(f"**{text}**" if text else "")
        elif name in ("em", "i"):
            text = render(child).strip()
            out.append(f"*{text}*" if text else "")
        elif name == "img":
            src = child.get("src", "")
            alt = child.get("alt", "")
            out.append(f"\n\n![{alt}]({src})\n\n")
        elif name == "blockquote":
            text = render(child).strip()
            quoted = "\n".join("> " + line for line in text.splitlines())
            out.append("\n\n" + quoted + "\n\n")
        else:
            out.append(render(child))
    return "".join(out)


def to_markdown(node):
    text = render(node)
    text = re.sub(r"[ \t]+\n", "\n", text)
    text = re.sub(r"\n{3,}", "\n\n", text)
    return text.strip() + "\n"


def extract(html, url):
    soup = BeautifulSoup(html, "html.parser")

    alternates = {}
    for link in soup.find_all("link", rel="alternate"):
        hreflang = link.get("hreflang")
        href = link.get("href")
        if hreflang and href:
            alternates[hreflang] = urljoin(url, href)

    title = None
    jumbotron_h1 = soup.select_one(".content-page-jumbotron h1")
    if jumbotron_h1:
        title = jumbotron_h1.get_text(strip=True) or None

    row = soup.select_one(".page-container .row")
    if row is None:
        return None
    cols = row.find_all("div", class_=re.compile(r"\bcol-md-\d+\b"), recursive=False)
    if not cols:
        return None
    body = cols[0]

    breadcrumb_items = [
        (li.find("a") or li.find("span")).get_text(strip=True)
        for li in body.select('nav[aria-label="breadcrumb"] li')
        if li.find("a") or li.find("span")
    ]
    breadcrumb_title = breadcrumb_items[-1] if breadcrumb_items else None
    breadcrumb_parent = breadcrumb_items[-2] if len(breadcrumb_items) >= 2 else None
    for nav in body.find_all("nav"):
        nav.decompose()

    # Editors sometimes leave a blank <h1></h1> above the real heading (a CMS
    # artifact), so skip empty ones rather than taking the first h1 found.
    if title is None:
        for heading in body.find_all(("h1", "h2", "h3")):
            text = heading.get_text(strip=True)
            if text:
                title = text
                heading.decompose()
                break

    # Another CMS artifact: a heading copy-pasted from the parent section
    # (e.g. a page titled "Tohtoriopinnoista" whose body <h1> still says
    # "Opinnot"). The breadcrumb's own label for this page is more reliable.
    if title and breadcrumb_parent and title == breadcrumb_parent and breadcrumb_title:
        title = breadcrumb_title

    if title is None:
        title = breadcrumb_title

    if not title:
        return None

    for heading in body.find_all(("h1", "h2", "h3")):
        if not heading.get_text(strip=True):
            heading.decompose()

    return {"title": title, "body": body, "alternates": alternates}


def front_matter(data):
    text = "---\n"
    text += yaml.safe_dump(data, allow_unicode=True, sort_keys=False, default_flow_style=False)
    text += "---\n\n"
    return text


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true", help="Fetch and parse, but don't write files")
    parser.add_argument("--delay", type=float, default=0.3, help="Seconds to sleep between requests")
    args = parser.parse_args()

    session = requests.Session()
    session.headers["User-Agent"] = USER_AGENT

    print(f"Fetching sitemap from {SITEMAP_URL}")
    sitemap_xml = fetch(session, SITEMAP_URL).text
    urls = sorted(set(re.findall(r"<loc>(.*?)</loc>", sitemap_xml)))
    all_paths = {urlparse(u).path for u in urls}

    to_process = [u for u in urls if urlparse(u).path not in SKIP_PATHS]
    print(f"{len(urls)} pages in sitemap, {len(to_process)} to export "
          f"({len(urls) - len(to_process)} skipped: homepages + hand-curated pages)")

    pages = {}
    failures = []
    for i, url in enumerate(to_process, start=1):
        path = urlparse(url).path
        print(f"[{i}/{len(to_process)}] {path}")
        try:
            resp = fetch(session, url)
        except requests.RequestException as exc:
            if "id.prodeko.org" in str(exc):
                failures.append((url, "member-only: redirects to the Keycloak login, "
                                       "roadmap item G, not something this script can fetch"))
            else:
                failures.append((url, f"fetch error: {exc}"))
            continue
        final_path = urlparse(resp.url).path
        if final_path != path and final_path in all_paths:
            print(f"    redirects to {final_path}, already covered separately, skipping")
            continue
        data = extract(resp.text, resp.url)
        if data is None:
            if "page-container" not in resp.text:
                reason = "different template (no .page-container): a form, hub or landing page, not plain text"
            else:
                reason = "could not find a title in the expected markup"
            failures.append((url, reason))
            continue
        pages[url] = data
        time.sleep(args.delay)

    # translationKey: derived from the Finnish path, shared with its English
    # alternate when one exists and was itself exported.
    translation_keys = {}
    for url, data in pages.items():
        path = urlparse(url).path
        if not path.startswith("/fi/"):
            continue
        key = "-".join(tail_parts(path)) or "home"
        translation_keys[url] = key
        alt_en = data["alternates"].get("en")
        if alt_en and alt_en in pages:
            translation_keys[alt_en] = key
    for url in pages:
        if url not in translation_keys:
            path = urlparse(url).path
            lang = path.strip("/").split("/")[0]
            translation_keys[url] = f"{lang}-" + ("-".join(tail_parts(path)) or "home")

    written = 0
    for url, data in pages.items():
        path = urlparse(url).path
        out_path = local_file_for(path, all_paths)
        if out_path is None:
            failures.append((url, "resolved to the language root, skipping"))
            continue
        body_md = to_markdown(data["body"])
        meta = {
            "title": data["title"],
            "translationKey": translation_keys[url],
            "sourceURL": url,
        }
        content = front_matter(meta) + body_md
        if args.dry_run:
            print(f"--- would write {out_path.relative_to(REPO_ROOT)} ---")
            continue
        out_path.parent.mkdir(parents=True, exist_ok=True)
        out_path.write_text(content, encoding="utf-8")
        written += 1

    print(f"\n{written} pages written" + (" (dry run)" if args.dry_run else ""))
    if failures:
        print(f"\n{len(failures)} pages could not be exported:")
        for url, reason in failures:
            print(f"  {url}: {reason}")


if __name__ == "__main__":
    sys.exit(main())
