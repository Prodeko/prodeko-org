# Serving the new site on prodeko.org

A temporary cutover: for the duration of the hackathon demo, prodeko.org answers
with the Hugo site instead of the Django CMS, and afterwards it goes back. The
whole design is shaped by that. The switch is two edited files in one commit,
the revert is `git revert` of that commit plus a play run, and nothing in either
direction touches DNS, Keycloak, TLS certificates or CI.

The real cutover comes later and is a different document. It owes the
roughly 107 old addresses a verified redirect map, which does not exist yet;
see the migration section of
[the design document](2026-09-18-prodeko-git-cms-design.md). Nothing here
substitutes for that work.

## What serves prodeko.org today

The old site is Django CMS in Docker on prodeko-vm2 — the same machine that
serves the Hugo site. It listens on `127.0.0.1:8000` and Caddy reverse-proxies
four names to it in one site block:

```
ansible/host_files/prodeko-vm2/Caddyfile.j2:22
www.prodeko.org, prodeko.org, www.prodeko.fi, prodeko.fi {
	reverse_proxy 127.0.0.1:8000
}
```

`ansible/roles/prodeko_org/` owns the host side: the `deploy-prodeko-org` user,
`/srv/www/prodeko-org/`, the rendered `variables.txt`, and a forced-command SSH
key the prodeko-org-djangocms workflow deploys through. The role deliberately
installs no systemd unit; the containers carry `restart: unless-stopped` and
`deploy.sh` owns the stack
(`ansible/roles/prodeko_org/README.md:37-44`).

Two consequences matter here. The Django containers keep running throughout —
nothing in this plan stops, migrates or redeploys them, so the revert does not
have to bring anything back up. And because both sites already live behind the
same Caddy, the switch is a routing change inside one config file rather than a move
between machines.

DNS is already correct and stays untouched. The apex A record points at
prodeko-vm2 (`terraform/main.tf:40-46`, TTL 300) and so do `uusi` and `cms`
(`terraform/main.tf:83-89`, `:95-101`). Caddy already holds valid certificates
for `prodeko.org` and `www.prodeko.org`, because it serves them today, so
moving those names to a different site block reuses the cached certificate.
There is no ACME issuance in the switch and none in the revert. That is most
of why both are seconds rather than minutes.

## The switch

Two files in infra-prodeko, one commit, one play run. The first is the Caddyfile:
take the two `.org` names off the Django block and put them on the Hugo block.

```
ansible/host_files/prodeko-vm2/Caddyfile.j2:22
-www.prodeko.org, prodeko.org, www.prodeko.fi, prodeko.fi {
+www.prodeko.fi, prodeko.fi {

ansible/host_files/prodeko-vm2/Caddyfile.j2:32
-{{ prodeko_site_hostname }} {
+prodeko.org, www.prodeko.org, {{ prodeko_site_hostname }} {
+	redir / /fi/ 302
```

`prodeko_site_hostname` stays `uusi.prodeko.org`. Leaving it alone is what keeps
the Caddy half to two lines: `publish.sh` health-checks that name over loopback
after every release (`ansible/roles/prodeko_site/templates/publish.sh.j2:76-78`),
the gatus uusi check watches it, and uusi keeps serving the same tree
throughout. Three names, one site block, one release directory.

The `redir` line is the front door and is explained under *Canonical links*
below. It must sort before `file_server`, which it does at the top of the block.

The member section demos on the apex, so the second file is not optional. One
line in the member gate, because the configured redirect URI is absolute and
there is one of it:

```
ansible/roles/member_gate/defaults/main.yml:52
-member_gate_redirect_url: https://uusi.prodeko.org/oauth2/callback
+member_gate_redirect_url: https://prodeko.org/oauth2/callback
```

Then one command:

```bash
ansible-playbook playbooks/prodeko_vm.yml --tags caddy,member_gate
```

The caddy role renders to `/tmp/Caddyfile.new`, runs `caddy validate`, and only
then writes the live config and reloads
(`ansible/roles/caddy/tasks/config.yml:13-42`). A typo fails before anything is
served. The member_gate role rewrites `/opt/member-gate/.env` and restarts the
oauth2-proxy container, which signs out everyone currently holding a member
session — four or five people, and they sign in again.

Total wall time: under a minute, dominated by the ansible run.

### Why the member gate line cannot wait

Skipping it does not break the 302 — Caddy issues that locally from
`Caddyfile.j2:87-90` regardless. It breaks the journey two hops later. A member
starting at `https://prodeko.org/fi/jasenille/` is sent to Keycloak with
`redirect_uri=https://uusi.prodeko.org/oauth2/callback`; Keycloak honours it,
oauth2-proxy sets the session cookie on `uusi.prodeko.org` (there is no
`OAUTH2_PROXY_COOKIE_DOMAINS` in `templates/env.j2`, so the cookie is host-only),
and the relative `rd` lands the member on uusi. They are signed in on the wrong
hostname and the apex loops forever. The check under *Go / no-go* that greps the
`redirect_uri` out of the `/oauth2/start` Location header is the one that catches
this, and it is the single most valuable assertion in this document.

### The accepted cost: one host's member login at a time

With a single configured redirect URI, the breakage above is symmetric. While
the apex is live, a member login **started on uusi.prodeko.org strands**: uusi's
`/oauth2/start` sends `redirect_uri=https://prodeko.org/oauth2/callback`,
Keycloak returns the browser to the apex, the session cookie is set host-only on
`prodeko.org`, and the member ends up signed in on the apex rather than where
they started. Public pages on uusi are unaffected; only the member journey moves
hosts. It un-breaks itself on the revert.

That is accepted, and the mitigation needs no configuration: demo from the apex,
and close the uusi tabs left over from testing. The realistic way to hit this is
a teammate demonstrating the member section from a stale uusi tab, not a guild
member, who has no reason to know uusi exists.

It is worth being precise that this is a choice rather than a limit of the tool.
See the next section.

### Both hosts at once is possible, and is deliberately not in this change

oauth2-proxy can derive the callback host per request instead of taking a fixed
one, which would let uusi and the apex both complete logins for the whole demo
window. Verified against the pinned version rather than recalled —
`member_gate_image` is `v7.15.4` (`ansible/roles/member_gate/defaults/main.yml:7`),
and at that tag `getOAuthRedirectURI` in `oauthproxy.go` reads:

```go
func (p *OAuthProxy) getOAuthRedirectURI(req *http.Request) string {
	// if `p.redirectURL` already has a host, return it
	if p.relativeRedirectURL || p.redirectURL.Host != "" {
		return p.redirectURL.String()
	}

	// Otherwise figure out the scheme + host from the request
	rd := *p.redirectURL
	rd.Host = requestutil.GetRequestHost(req)
	rd.Scheme = requestutil.GetRequestProto(req)
	...
	// If CookieSecure is true, return `https` no matter what
	if p.CookieOptions.Secure {
		rd.Scheme = schemeHTTPS
	}
	return rd.String()
}
```

Omitting the option leaves `p.redirectURL.Host` empty, and `NewOAuthProxy`
defaults the path (`redirectURL.Path = fmt.Sprintf("%s/callback", opts.ProxyPrefix)`),
so the derived URI is `https://<request host>/oauth2/callback` — exactly the two
values already registered in Keycloak.

The derivation is sound here through two independent paths, which is what makes
it trustworthy rather than merely plausible:

- **Host.** `requestutil.GetRequestHost` prefers `X-Forwarded-Host` when
  `CanTrustForwardedHeaders` allows it and falls back to `req.Host` otherwise.
  Caddy's `reverse_proxy` sets `X-Forwarded-Host` by default *and* passes the
  original `Host` through unmodified to a plain-HTTP upstream, which
  `127.0.0.1:4180` is. Both sources carry the same correct value, so the
  outstanding `--trusted-proxy-ip` warning noted at
  `ansible/roles/member_gate/README.md:183-188` cannot change the outcome.
- **Scheme.** `OAUTH2_PROXY_COOKIE_SECURE=true` (`templates/env.j2:34`) forces
  `https` unconditionally, so the derived scheme is right even if
  `X-Forwarded-Proto` were absent.

Nothing else in the gate is disturbed: `/oauth2/auth` builds no redirect URI, so
`forward_auth` is untouched, and `rd` stays a bare path, so the relative-redirect
check still applies and `--whitelist-domain` is still unnecessary
(`Caddyfile.j2:83-86`).

So it works. It stays out of this change for three concrete reasons, none of
them technical doubt:

1. It needs the safety assert relaxed. `ansible/roles/member_gate/tasks/assert.yml:36`
   requires `member_gate_redirect_url is match('^https://[^/]+/oauth2/callback$')`,
   which an empty value fails. That assert exists because a malformed redirect
   URI fails at the last step of a login with an error naming nothing, and
   loosening it hours before a demo removes the guard exactly when it is most
   wanted.
2. It needs `templates/env.j2:19` to omit `OAUTH2_PROXY_REDIRECT_URL` entirely
   rather than render it empty — a Jinja conditional on a security-relevant
   variable, written under time pressure.
3. It makes **every** hostname on the site block need its own registered
   callback. `www.prodeko.org` has none. Under a fixed apex URI, a member
   starting on www is simply carried to the apex and works; under derivation
   they get a Keycloak `invalid redirect_uri` error. So it also forces either a
   third hand-made Keycloak registration or restructuring www into its own
   redirect block.

Two files and a bounded, mitigable cost beats four files, a relaxed assert and a
new Keycloak entry on the one path that must not break. The upgrade is genuinely
worth making — it deletes a hand-edited hostname from the config surface
permanently, so the real cutover later would need no member-gate change at all —
but it belongs in its own change, with its own play run and its own login test,
on a day when nothing is being demonstrated. The verification above is the
expensive part and it is already done.

## Prepare ahead

Four things, all doable days early, none of them visible to a visitor, and none
of them needing to be undone afterwards. Every one of them is a place where
"both hostnames work at once" is expressible, which is what makes the switch and
the revert symmetric.

**Keycloak is already done.** Both redirect URIs are registered on the
`prodeko-member-gate` client in the `membership-registry` realm, exactly these
two and no wildcards (`ansible/roles/member_gate/README.md:99-102`):

```
https://uusi.prodeko.org/oauth2/callback
https://prodeko.org/oauth2/callback
```

Verify it in the admin console before the switch rather than discovering it
during. The `cms-auth-proxy` client needs nothing at all: its redirect URI is
`https://cms.prodeko.org/callback`, against a name that never moves
(`ansible/roles/cms_auth_proxy/defaults/main.yml:26-31`). Both extra
registrations can stay registered forever; there is nothing to remove on the
revert.

**Let the editor proxy accept both origins.** `CMS_ORIGINS` is
comma-separated, and the handshake page checks the opener's origin against the
list by exact match, admitting any of them
(`proxy/internal/auth/handshake.go:35-39, 118`; CORS at
`proxy/cmd/proxy/main.go:190`). So:

```
ansible/roles/cms_auth_proxy/defaults/main.yml:48
-cms_auth_proxy_site_origins: https://uusi.prodeko.org
+cms_auth_proxy_site_origins: https://uusi.prodeko.org,https://prodeko.org
```

This is the most forgettable item in the whole plan, for two reasons. Rendering
`.env.production` does not restart the container — `ansible/roles/cms_auth_proxy/tasks/main.yml:107`
has no handler, because `deploy.sh` owns the container — so the play run appears
to succeed and changes nothing. Redeploy by hand afterwards:

```bash
sudo -u deploy-cms-auth-proxy env IMAGE_TAG="$(cat /srv/www/cms-auth-proxy/.image-tag)" \
  /srv/www/cms-auth-proxy/deploy.sh
```

And the failure it prevents is silent from the browser's side: `/admin` loads,
the Keycloak popup completes, and then either the handshake refuses the origin
or every GitHub call fails CORS. The proxy logs it
(`proxy/cmd/proxy/main.go:246-249`) and nothing else does.

Both origins listed permanently is harmless — each is a host we control — so
this never needs reverting.

**Repoint the gatus apex check at a page.** It currently asks for `/` and
requires 200 (`ansible/vars/prodeko_vm.yml:52-61`). With the `redir` line above,
`/` answers 302 on the new site and the check pages Telegram. `/fi/` returns 200
on both the old site and the new one, so changing it now is safe in both states:

```
ansible/vars/prodeko_vm.yml:55
-    url: "https://prodeko.org"
+    url: "https://prodeko.org/fi/"
```

**Decide the five redirect vhosts that will 404.** Ten subdomains redirect into
the apex (`Caddyfile.j2:164-198`). Six of their targets do not exist in the new
content tree:

| Vhost | Target | New site |
|---|---|---|
| `abit` (`:165`) | `/fi/abit/` | exists |
| `alumni` (`:169`) | `/fi/alumni/` | exists |
| `alumnirekisteri` (`:173`) | `/fi/matrikkeli/` | 404 |
| `matrikkeli` (`:185`) | `/fi/matrikkeli/` | 404 |
| `proleko` (`:189`) | `/fi/palvelut/proleko/` | 404 |
| `tiedotteet` (`:193`) | `/fi/palvelut/viikkotiedote/` | 404 |
| `vaalit` (`:197`) | `/fi/palvelut/vaalit/` | 404 |
| `lifelonglearning` (`:181`) | `https://www.prodeko.org/lifelonglearning/` | 404 |
| `fuksiopas` (`:177`) | static.prodeko.org | unaffected |

The 404 is worse than it looks: `handle_errors` rewrites to `/404.html`
(`Caddyfile.j2:104-108`) and the Hugo build emits no `404.html`, so the visitor
gets a blank body. Either repoint these at real pages in the same commit
(`lifelonglearning` has an obvious home at `/fi/alumni/lifelong-learning/`), or
accept them, but decide rather than discover. This is pre-existing, not caused
by the cutover — it is simply the first moment anyone follows those links into
the new tree.

## Canonical links, and what the tree actually contains

The instinct is that serving a tree built against `uusi.prodeko.org` from the
apex will bounce visitors back to uusi. Measured against the built tree
(`audit-tree/`, 115 HTML files), that is almost entirely false:

- Zero absolute URLs in `<a href>`. Zero in `<img>`, `<script>` or stylesheet
  links. Navigation, the mega-menu, the footer, breadcrumbs and every body link
  are relative, because the templates use `RelPermalink` throughout.
- 225 absolute URLs in `<link rel="alternate" hreflang>` head tags
  (`site/layouts/partials/head.html:1-3`, the one template using `.Permalink`).
  Invisible to a visitor.
- 334 in the three sitemap files.
- Six in exactly two files, and these are the only clickable ones: the alias
  stubs. `audit-tree/index.html` is the root's meta-refresh to `baseURL + /fi/`,
  and `audit-tree/en/studies/studies/index.html` is the tree's single
  `aliases:` entry (`site/content/en/studies/_index.md:6-7`).

So the entire visitor-facing exposure is the front door. Someone typing
`prodeko.org` gets a meta-refresh to `https://uusi.prodeko.org/fi/` and the
address bar leaves the apex on the first click. That is precisely what the
comment at `.github/workflows/build.yml:26-32` warns about.

**Recommendation: leave `HUGO_BASEURL` pointing at uusi and handle the front
door in Caddy.** The `redir / /fi/ 302` line intercepts the stub before
`file_server` ever reads it, and costs nothing on the revert because it lives in
the same reverted commit. The alternative — editing
`.github/workflows/build.yml:32` to the apex — buys correct hreflang and sitemap
values that no judge will look at, and costs a full CI build on the switch *and*
another on the revert. That audit job installs Chromium; the revert stops being
a Caddy reload and starts being a wait on GitHub Actions. For a temporary demo
the revert time is the thing being optimised, so the one-line Caddy redirect
wins.

The tradeoff to accept consciously: `view-source:` shows `uusi.prodeko.org` in
the hreflang tags, and `https://prodeko.org/sitemap.xml` lists uusi URLs. If the
demo is judged on either, flip `HUGO_BASEURL` instead and budget two CI
round-trips.

## Revert

Equal in weight to the switch, and the reason the switch is acceptable at all.

```bash
# in infra-prodeko
git revert --no-edit <cutover-commit>
ansible-playbook playbooks/prodeko_vm.yml --tags caddy,member_gate
```

That is the whole thing. `caddy validate` runs first, the reload is graceful,
and the Django containers were never stopped, so the apex is answering from
Django again on the next request. Under a minute, and it does not depend on
GitHub Actions, on the container registry, or on anyone remembering a second
step.

The prepare-ahead changes stay. Both Keycloak redirect URIs stay registered,
both CMS origins stay allowed, the gatus check stays pointed at `/fi/`. None of
them are wrong in the reverted state and unwinding them would add steps to the
path that has to be fast.

Rehearse it once before the demo, ideally the same day: run the switch, run the
go/no-go list, run the revert, run the revert checks. A revert that has been
executed once is a different kind of promise from one that has been written down.

### What does not cleanly revert

**Browser and proxy caches, for five minutes.** The Hugo vhost sets
`Cache-Control: public, max-age=300` on everything (`Caddyfile.j2:101`). After
the revert a browser may hold new-site HTML for up to that long. Bounded,
harmless, and the shortest thing that was ever going to be practical. Nothing in
the tree is emitted with a longer freshness.

**Nothing is a 301.** Caddy's bare `redir` is 302, which is what every redirect
vhost in the file already uses and what the added `redir / /fi/ 302` states
explicitly. Do not add `permanent` anywhere in this work. A 301 from the apex is
cached by browsers indefinitely and is the one change in this area that a revert
genuinely cannot undo.

**HSTS is already there and does not change.** The shared `security-headers`
snippet sets `Strict-Transport-Security: max-age=31536000` with no
`includeSubDomains` and no `preload` (`Caddyfile.j2:8`), and the current Django
apex block already imports it (`Caddyfile.j2:23`). The Hugo block imports the
same snippet. The header the apex sends is byte-identical before, during and
after. There is no new commitment to unwind.

**Keycloak second redirect URIs simply stay.** No action on revert, no action
ever. Same for the second CMS origin.

**DNS is untouched in both directions.** The apex A record does not move, so
there is no TTL to wait out. The soak-and-raise note at `terraform/main.tf:37-39`
belongs to the real cutover later, not to this one — leave the TTL at 300.

**Recorded analytics and published edits.** Pageviews counted against apex paths
during the demo stay in GoatCounter, and anything an editor publishes through
`/admin` is a real commit on `main` and a real deploy. Both are intended, and
neither is affected by the apex revert.

**Gatus will alert during both windows.** The apex check flips state twice.
Expect the Telegram noise or silence the check for the demo; do not let the
alert be the first thing anyone notices.

## Every place a hostname is baked in

Twenty-one places across the two repositories plus two hand-managed systems.
Only three change at the switch.

### Changes at the switch

| Where | What | If forgotten |
|---|---|---|
| `infra-prodeko: ansible/host_files/prodeko-vm2/Caddyfile.j2:22` | drop `prodeko.org`, `www.prodeko.org` from the Django block | both blocks claim the same name; `caddy validate` fails and nothing deploys |
| `infra-prodeko: ansible/host_files/prodeko-vm2/Caddyfile.j2:32` | add the two names to the Hugo block, plus `redir / /fi/ 302` | the apex still serves Django, or serves the new site but bounces `/` to uusi |
| `infra-prodeko: ansible/roles/member_gate/defaults/main.yml:52` | `member_gate_redirect_url` to the apex form | member login completes on uusi, cookie lands on the wrong host, the apex loops |

### Changes ahead of the switch, and stays

| Where | What | If forgotten |
|---|---|---|
| Keycloak `prodeko-member-gate` client | apex redirect URI registered (already done per `ansible/roles/member_gate/README.md:99-102`) | Keycloak refuses the callback; login dies at the last step with an error naming nothing |
| `infra-prodeko: ansible/roles/cms_auth_proxy/defaults/main.yml:48` | add `https://prodeko.org` to `cms_auth_proxy_site_origins`, **then redeploy the container by hand** | `/admin` loads, login popup completes, every save fails CORS. Silent in the browser |
| `infra-prodeko: ansible/vars/prodeko_vm.yml:55` | apex gatus check to `/fi/` | Telegram alert the moment the `redir` lands |
| `infra-prodeko: ansible/host_files/prodeko-vm2/Caddyfile.j2:173,181,185,189,193,197` | repoint or accept the six redirect vhosts whose targets 404 | six guild subdomains land on a blank error page |

### Named but deliberately unchanged

| Where | Why it stays |
|---|---|
| `prodeko-org: site/hugo.toml:1` | `baseURL = "https://prodeko.org/"` is already the apex; CI overrides it |
| `prodeko-org: .github/workflows/build.yml:32` | `HUGO_BASEURL` stays at uusi so the switch and the revert need no CI round-trip. See *Canonical links* |
| `prodeko-org: .github/workflows/build.yml:165`, `proxy.yml:159` | the SSH deploy targets are already `…@prodeko.org`, pinned by `.github/known_hosts` |
| `infra-prodeko: ansible/roles/prodeko_site/defaults/main.yml:20` | `prodeko_site_hostname` stays `uusi.prodeko.org`; uusi keeps serving and `publish.sh` keeps health-checking it |
| `infra-prodeko: ansible/roles/prodeko_site/templates/publish.sh.j2:76-78` | rendered from the above; deploys keep working through the demo and the revert |
| `infra-prodeko: ansible/roles/cms_auth_proxy/defaults/main.yml:31` | `cms.prodeko.org` is permanent by design; the Keycloak redirect URI is registered against it |
| `prodeko-org: site/static/admin/config.yml:20,22` | `base_url` and `api_root` name `cms.prodeko.org`, which does not move |
| `infra-prodeko: ansible/roles/member_gate/tasks/assert.yml:36`, `templates/env.j2:19` | the redirect-URI shape assert stays in force, because the shipped mode keeps a fixed absolute URI. Both would have to change for derived-host mode |
| `infra-prodeko: ansible/vars/prodeko_vm.yml:62-98` | the uusi and member-gate gatus checks stay green; the member-gate one asserts a 302 Caddy issues locally |
| `infra-prodeko: terraform/main.tf:40-101` | every A record, including the apex and its TTL |
| `prodeko-org: site/data/embeds.yaml`, `megamenu.yaml`, `footer.yaml` | every service hostname they name is a separate vhost, untouched |

### The three most forgettable

The CMS origin redeploy, because the ansible run reports success and does
nothing. The member gate redirect URL, because the visible 302 keeps working and
only the completed login breaks. And the gatus apex check, because it fires ten
minutes later when everyone has moved on.

## The in-flight features

**Analytics (`analytics.prodeko.org`).** Self-hosted GoatCounter resolves which
site a hit belongs to from the `Host` header of the request to `/count` — that
is, from its own vhost, `analytics.prodeko.org` — not from the hostname of the
page that fired the beacon, and `/count` carries no origin allowlist. So counting
keeps working when the counted pages move from uusi to the apex, with no change
to the site record. What the site record's link-domain field does affect is
whether paths in the dashboard are clickable and where they point; leaving it at
uusi makes dashboard links open the wrong host. Cosmetic, and for a two-day demo
not worth a change that would have to be reverted. **Confirm this against the
analytics branch before relying on it** — it is the one claim here derived from
GoatCounter's general behaviour rather than from a file in these repositories.

**Previews (`preview.prodeko.org`).** Its own vhost, its own DNS record, serving
per-PR builds. It reads nothing from `prodeko_site_hostname`, appears nowhere in
the Caddy blocks being edited, and is not an origin the CMS proxy or the member
gate knows about. Unaffected in both directions. One curl in the go/no-go list
confirms it rather than assuming it.

**Search.** Built into the tree at build time, entirely same-origin, no
hostname anywhere. Unaffected.

## What explicitly does not change

Say this out loud to whoever is helping, because the failure mode of a cutover
is somebody helpfully fixing something that was already right.

`static.prodeko.org` keeps every upload — the alumni newsletters, the freshman
guides, the officer photos — at the addresses it already has, and the new site
links to them. `id.prodeko.org` and the `membership-registry` realm are not
reconfigured; the only Keycloak touch in this entire plan is verifying a redirect
URI that is already registered. `cms.prodeko.org` keeps its name, its vhost, its
certificate and its redirect URI. `ilmo`, `gallery`, `store`, `viikkotiedote`,
`vaalit`, `vaalikoppi`, `matrikkeli` and every other embed target are separate
services on separate names; the embeds in `site/data/embeds.yaml` point at
absolute URLs that stay correct. `preview.prodeko.org` is independent.
`prodeko.fi` and `www.prodeko.fi` stay on Django. `mta-sts.prodeko.org`,
`kiltis`, `beszel` and `uptime` are not in the blast radius. No DNS record moves
and no TTL changes. The Django containers are not stopped, migrated or
redeployed.

## Go / no-go

Runnable in about two minutes. Each line is one assertion; anything unexpected
means run the revert and diagnose afterwards.

```bash
# 1. The apex serves the new site off disk, not Django.
#    `public, max-age=300` comes from Caddyfile.j2:101 and only the Hugo block sets it.
curl -sI https://prodeko.org/fi/ | grep -i '^cache-control'
#    -> cache-control: public, max-age=300

# 2. The front door stays on the apex.
curl -sI https://prodeko.org/ | grep -iE '^(HTTP|location)'
#    -> HTTP/2 302  /  location: /fi/
curl -s https://prodeko.org/ | grep -i uusi
#    -> no output. Any output means the redir line is missing or sorted wrong.

# 3. www works too.
curl -so /dev/null -w '%{http_code}\n' https://www.prodeko.org/fi/
#    -> 200

# 4. The member gate refuses a stranger.
curl -sI https://prodeko.org/fi/jasenille/ | grep -iE '^(HTTP|location)'
#    -> HTTP/2 302  /  location: /oauth2/start?rd=/fi/jasenille/

# 5. The gate will send them to the right callback. THE ONE THAT MATTERS.
curl -sI 'https://prodeko.org/oauth2/start?rd=/fi/jasenille/' \
  | grep -io 'redirect_uri=[^&]*'
#    -> redirect_uri=https%3A%2F%2Fprodeko.org%2Foauth2%2Fcallback
#       If it says uusi, member_gate_redirect_url was not flipped.

# 6. A page behind the gate is not readable.
curl -so /dev/null -w '%{http_code}\n' https://prodeko.org/fi/jasenille/poytakirjat/
#    -> 302, never 200

# 7. The editing screen loads, and the proxy allows the apex origin.
curl -so /dev/null -w '%{http_code}\n' https://prodeko.org/admin/
#    -> 200
curl -sI -X OPTIONS https://cms.prodeko.org/github/user \
  -H 'Origin: https://prodeko.org' \
  -H 'Access-Control-Request-Method: GET' | grep -i 'access-control-allow-origin'
#    -> access-control-allow-origin: https://prodeko.org
#       Missing means CMS_ORIGINS was changed but the container was not redeployed.

# 8. Analytics counts from the apex.
curl -so /dev/null -w '%{http_code}\n' 'https://analytics.prodeko.org/count?p=/fi/&t=cutover-check'
#    -> 200 or 204. Note this records one real pageview.

# 9. Previews are unaffected.
curl -so /dev/null -w '%{http_code}\n' https://preview.prodeko.org/
#    -> 200

# 10. An old address still resolves, because paths were preserved rather than redirected.
curl -so /dev/null -w '%{http_code}\n' https://prodeko.org/fi/guild/arvot/
#    -> 200

# 11. Nothing else moved.
for h in cms.prodeko.org/healthz id.prodeko.org ilmo.prodeko.org uusi.prodeko.org; do
  printf '%s %s\n' "$(curl -so /dev/null -w '%{http_code}' "https://$h")" "$h"
done
#    -> 200 on all four
```

**12. A real member login completes on the apex.** The member section is part of
the demo, so this is a required check and not a nicety — checks 4 and 5 make the
outcome very likely, but only the round trip proves it. In a fresh private
window:

1. Open `https://prodeko.org/fi/jasenille/`.
2. Sign in with an account that holds `membership`.
3. Confirm the page renders and the address bar reads `prodeko.org` — not
   `uusi.prodeko.org`. Landing on uusi means the redirect URI did not flip.
4. Reload. It stays served without a second sign-in, which proves the session
   cookie was set on the apex.

Then the negative half, which no curl can stand in for
(`ansible/roles/member_gate/README.md:79-90`): sign in with an account that does
**not** hold `membership` and confirm the refusal — a 403 at `/oauth2/callback`
and `[AuthFailure] ... unauthorized` in `docker logs member_gate`. A misconfigured
gate admits everyone silently, so this is the only check that can see it.

Expect, and do not be alarmed by, a member login started on `uusi.prodeko.org`
finishing on the apex. That is the accepted cost stated above, not a fault.

### After the revert

```bash
# The apex is Django again: no file_server cache header.
curl -sI https://prodeko.org/fi/ | grep -i '^cache-control'
#    -> anything except `public, max-age=300`
curl -so /dev/null -w '%{http_code}\n' https://prodeko.org/fi/
#    -> 200

# uusi still serves the new site, and its gate points home again.
curl -sI https://uusi.prodeko.org/fi/ | grep -i '^cache-control'
#    -> cache-control: public, max-age=300
curl -sI 'https://uusi.prodeko.org/oauth2/start?rd=/fi/jasenille/' \
  | grep -io 'redirect_uri=[^&]*'
#    -> redirect_uri=https%3A%2F%2Fuusi.prodeko.org%2Foauth2%2Fcallback

# The redirect vhosts still land somewhere.
curl -sI https://abit.prodeko.org/ | grep -i '^location'
#    -> location: https://prodeko.org/fi/abit/
```

## Open decisions

**Does the demo need canonical links on the apex?** The recommendation is no:
leave `HUGO_BASEURL` at uusi, take the one-line Caddy redirect, keep the revert
at a Caddy reload. Flipping it costs a CI round-trip each way and buys hreflang
and sitemap values nobody clicks. Decide before the switch; it is the only choice
here that changes the shape of the revert.

**Take the derived-host upgrade now or later?** The recommendation is later, for
the three reasons set out under *Both hosts at once*. Taking it now buys a demo
window with no broken path at all, at the price of four files, a relaxed safety
assert and a third Keycloak registration for `www`, landed on the login path
shortly before the demo. Taking it later is a permanent tidy-up — it removes a
hand-edited hostname from the config surface, so the real cutover needs no
member-gate change — and the verification it depended on is already done.

**The six 404ing redirect vhosts.** Repoint them in the cutover commit, or accept
that six guild subdomains land on a blank error page for the duration. Related
and independently worth an hour: the build emits no `404.html`, so every 404 on
the new site is blank.

**Should `www.prodeko.org` move with the apex?** The recommendation is yes — it
is what people type, and leaving it on Django means half the audience sees the
old site. Confirm nothing else depends on `www` resolving to Django. Under the
fixed redirect URI being shipped, a member who starts on www is carried to the
apex and signs in normally, so www needs no Keycloak entry of its own; that
changes if the derived-host upgrade is ever taken.

**`prodeko.fi` and `www.prodeko.fi` stay on Django.** Confirm that is intended;
they are in the same block being edited and it would be one more name each way
to move them.

**Who holds the revert, and for how long does the demo run?** The revert is one
`git revert` and one play run, which means whoever holds ansible access to
prodeko-vm2 holds it. Name that person, and rehearse the revert once before the
demo rather than reading it for the first time under pressure.
