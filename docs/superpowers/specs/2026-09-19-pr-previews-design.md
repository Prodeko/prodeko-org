# Pull request previews

Every pull request gets a browsable copy of the site at an address of its own,
`https://pr-<number>.preview.prodeko.org/`, behind a Prodeko login. An editor
saving a draft in Decap sees the link next to their entry and can look at their
change before it is published, without a GitHub account and without a
developer.

This is the design document for that feature. The site itself is described in
[the main design document](2026-09-18-prodeko-git-cms-design.md), whose
"Editing and publishing" section promises this and is satisfied by it.

## How it works

A preview is the same two Hugo trees the live site is built from, published to
prodeko-vm2 by the same kind of streamed SSH command, and served by the same
Caddy. Nothing new is invented; three existing patterns are instantiated a
second time.

```
pull request opened or pushed
  │
  ├─ hugo, twice, baseURL https://pr-<N>.preview.prodeko.org/
  ├─ check-trees.sh
  ├─ rm -rf site/public/admin
  ├─ pagefind, once for public and once per member section
  ├─ check-trees.sh, again, now against the indexes
  │
  └─ tar public members | ssh deploy-prodeko-preview@prodeko.org "preview publish pr-<N>"
        │
        └─ /srv/www/prodeko-preview/sites/pr-<N>/{public,members}
              │
              └─ caddy, *.preview.prodeko.org, behind oauth2-proxy
                    │
                    └─ commit status on the PR head → Decap shows the link
```

### Names and certificates

Previews live under a `preview` label rather than directly under the apex. A
wildcard at `*.prodeko.org` would answer for every unregistered name in the
guild's main zone, swallowing typos and shadowing future services before they
are provisioned, and it would force the ACME credential to hold write access to
the zone carrying the guild's MX, SPF and DKIM records.

`preview.prodeko.org` is therefore a delegated Azure DNS zone of its own in the
`prodeko-rg` resource group, with an NS record in `prodeko.org` pointing at it.
Inside that zone an `A` record at the apex and a wildcard `A` record both point
at the VM on 20.224.96.31. The service principal Caddy uses for the DNS
challenge, `prodeko-preview-dns`, holds DNS Zone Contributor scoped to the
child zone alone, so a credential stolen off the web VM cannot touch guild
email.

The zone, its records and the service principal are managed by hand through the
az CLI, matching the parent zone, which Terraform does not manage either.

One Caddy site block names both `preview.prodeko.org` and
`*.preview.prodeko.org`, which yields a single certificate carrying both as
subject alternative names. Let's Encrypt's limit of fifty new certificates per
registered domain per week is therefore untouched by preview traffic: the count
is one, issued once and renewed on the ordinary sixty-day cycle.

The wildcard requires the DNS-01 challenge, which requires a DNS provider
module in the Caddy binary. DNS-01 never receives an inbound HTTP request, so
the authentication gate in front of the host cannot interfere with certificate
issuance and renewal, and no path needs to be excluded from it.

### The gate

A second oauth2-proxy container sits in front of everything on both names. It
is a copy of the `member_gate` role with different values, not an extension of
it: sharing the live gate would mean changing the configuration that protects
published member content in order to ship a preview feature, and would require
a cookie domain spanning both hosts.

The required realm role is `prodeko-org-admin`, so previews are visible to
editors and administrators and not to the guild at large. Drafts are therefore
readable by fewer people than published member content, which is the safe
direction. Widening this is a one-variable change:
`preview_gate_required_role` in the role defaults.

The session cookie is scoped to `.preview.prodeko.org` so that one login covers
every preview. It is never scoped to `.prodeko.org`; that single character is
what keeps preview sessions off the site's own origin, and the role asserts it
rather than only documenting it.

Sign-in starts on `preview.prodeko.org`, which is the one host registered as a
Keycloak redirect URI. A preview subdomain answering 401 redirects the visitor
to the apex with an absolute return address, which oauth2-proxy accepts because
`.preview.prodeko.org` is in its whitelist of redirect domains.

### Two trees, one host

Because the whole host is authenticated, previews carry the member tree as well
as the public one. The tarball has the same two top-level directories,
`public` and `members`, as the live deploy, and the preview Caddy block mirrors
the site block's member matcher to choose the right root for
`/fi/jasenille` and `/en/members`.

As in the live configuration, `root` and `file_server` live inside the same
`handle` as `forward_auth`. That is what makes the gate fail closed: when
oauth2-proxy is down, `forward_auth` cannot reach it, Caddy answers 502, and
nothing is served. Hoisting them out would silently open every preview.

### Publishing

A third system user, `deploy-prodeko-preview`, holds a key pinned to a forced
command that accepts `preview publish pr-<N>` and `preview delete pr-<N>` and
nothing else. It writes only `/srv/www/prodeko-preview/sites/`, which is the
only directory it can write, and it cannot reach the live release tree: that is
a different user, a different directory and a different forced command.

The script caps the bytes it reads from the stream, extracts exactly the two
named members, rejects any payload containing a symlink, requires a non-empty
`public/index.html`, and stages into a temporary directory that a trap removes
unless the publish completes. All of this is `publish.sh` behaviour, copied
because it is right, not because previews need ceremony.

## Verified facts

Everything in this section is measured or quoted from source. The design rests
on it.

### A pull request can put executing script into a preview page

`site/hugo.toml` sets `unsafe = true` under `[markup.goldmark.renderer]` so
that editors can paste embed snippets. It is set at the root of the
configuration, and neither `config/development/` nor `config/members/`
overrides `markup`, so it applies to every build.

A markdown file containing a `<script>` tag and an `<img src=x onerror=...>`
builds without complaint and renders both verbatim into the page, with no
escaping and no stripping. This is the correct setting for this site — it is
what makes the embed shortcodes work — and it means the origin a preview is
served from is a security decision rather than a convenience.

### Origin and cookie isolation

Decap stores the signed-in editor's proxy session token in `localStorage` under
the key `decap-cms-user`, defined in `decap-cms-core/src/backend.ts` as
`LocalStorageAuthStore`. The editing screen is static content under
`site/static/admin/`, which Hugo copies into the root of every built tree, so
that token belongs to the origin serving the site: `uusi.prodeko.org` today,
`prodeko.org` after the cutover. `cms.prodeko.org` is the login proxy, reached
cross-origin, and holds no browser session for the editing screen.

Preview subdomains are disjoint origins from the site, so that token is out of
reach. The live member gate sets no cookie domain, so its session cookie is
host-only on the site's own name and is never sent to a preview. Both
properties hold only while the preview gate's cookie domain stays
`.preview.prodeko.org`.

Within the preview namespace the cookie is deliberately shared, so a script in
one preview can cause authenticated requests to a sibling preview. It cannot
read the responses: previews are separate origins from each other, `file_server`
emits no CORS headers, and the `X-Frame-Options: SAMEORIGIN` from the shared
`security-headers` snippet blocks the framing route. The session cookie itself
is out of reach of script, because oauth2-proxy sets `HttpOnly` by default.
What remains is a script that can make unreadable requests and render a
convincing fake login, aimed at an audience of authenticated editors looking at
drafts. That is accepted.

### oauth2-proxy role semantics

`checkAllowedGroups` in `oauthproxy.go` returns true on the first match between
the session's groups and the allowed set:

```go
func checkAllowedGroups(req *http.Request, s *sessionsapi.SessionState) bool {
	allowedGroups := extractAllowedEntities(req, "allowed_groups")
	if len(allowedGroups) == 0 {
		return true
	}

	for _, group := range s.Groups {
		if _, ok := allowedGroups[group]; ok {
			return true
		}
	}

	return false
}
```

Listing several roles therefore widens access rather than narrowing it, which
is the opposite of the intuition, and is why the gate names exactly one role.
The empty case returning true is the same failure mode the `member_gate` README
warns about: a misspelt role name admits everyone who can log in to the realm.
The acceptance test is consequently a negative one, described under "Setting it
up".

### The Caddy binary

Caddy is installed from the Cloudsmith apt repository and the stock binary
carries no DNS provider modules. `caddy add-package` is not the answer: it
replaces `/usr/bin/caddy`, which is dpkg-owned, so the next `apt upgrade caddy`
reverts it, Caddy can no longer parse the DNS challenge configuration, and
every vhost on the VM fails on the following reload.

Instead the apt package keeps owning the unit, the user, the group and
`/etc/caddy`, and a custom build is installed alongside it at
`/usr/local/bin/caddy`. A systemd drop-in overrides `ExecStart` and
`ExecReload` to point at it and adds the `EnvironmentFile` the stock unit lacks.
The stock unit is:

```ini
ExecStart=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/bin/caddy reload --config /etc/caddy/Caddyfile --force
```

An apt upgrade then cannot break the DNS module, and a missing custom binary
fails loudly at service start rather than silently losing a module.

No Go toolchain and no `xcaddy` on the host. Caddy's download API builds the
binary:

```
https://caddyserver.com/api/download?os=linux&arch=amd64&p=github.com/caddy-dns/azure
```

That endpoint answers 200 with
`content-disposition: attachment; filename="caddy_linux_amd64_custom"`. Ansible
fetches it with a recorded `sha256` so the build is pinned and an upstream
change fails the play rather than landing unnoticed, the same discipline the
`member_gate` role already applies to its container image digest.

The `caddy-dns/azure` module is configured as:

```
tls {
  dns azure {
    subscription_id {$AZURE_SUBSCRIPTION_ID}
    resource_group_name {$AZURE_RESOURCE_GROUP_NAME}
    tenant_id {$AZURE_TENANT_ID}
    client_id {$AZURE_CLIENT_ID}
    client_secret {$AZURE_CLIENT_SECRET}
  }
}
```

The `caddy` role runs `caddy validate` from `PATH`, so it gains a `caddy_bin`
variable. The default stays `caddy` because the role is shared with
`kiltis_server_services.yml`, which must be unaffected.

The five `{$NAME}` forms above are adapt-time substitutions, resolved by
whichever process performs the reload out of that process's own environment.
The role's reload therefore goes through `systemctl reload caddy` and not
`caddy reload --config`: a command Ansible runs over SSH has no environment, so
it would replace the live config with one whose DNS provider carries no
credentials. Measured against the pinned binary, `caddy adapt` without the
environment emits `"provider": {"name": "azure"}`, `caddy validate` accepts it,
the reload exits 0, and the symptom is a wildcard certificate that never
issues. Only the unit has the `EnvironmentFile`, so only systemd can reload
this host.

### Host routing

Tested against Caddy v2.11.4 with a local config and `Host` headers rather than
taken from documentation.

The `map` directive validates the hostname and extracts the preview identifier
in one construct:

```
map {host} {pr} {
	~^(pr-[0-9]{1,6})\.preview\.prodeko\.org$  "${1}"
	default ""
}
```

A request for `pr-42.preview.prodeko.org` yields `{pr}` of `pr-42`. Requests
for `evil.preview.prodeko.org` and `pr-abc.preview.prodeko.org` both fall to
the default and are answered 404 by a `vars {pr} ""` matcher. The apex is
separated inside the same site block by `@apex host preview.prodeko.org`.

Using `{pr}` rather than a label index keeps validation and extraction in one
place. For the record, `{labels.*}` is indexed from the right, so for
`pr-42.preview.prodeko.org` the value of `{labels.3}` is `pr-42`,
`{labels.2}` is `preview` and `{labels.0}` is `org`.

### HUGO_BASEURL

Each preview build sets `HUGO_BASEURL=https://pr-<N>.preview.prodeko.org/`.
Built that way, the Finnish front page carries 130 links under `/fi/`, four
under `/en/`, a stylesheet at `/css/bundle.min.<hash>.css`, and a root alias
that meta-refreshes to `https://pr-<N>.preview.prodeko.org/fi/`. Everything
resolves, because the preview is at the root of a host of its own.

This matters because the site is not portable to a subdirectory. Navigation and
footer URLs come from `data/navigation.yaml`, `data/megamenu.yaml` and
`data/footer.yaml` as root-absolute strings emitted raw by the templates, and
Hugo's URL helpers do not rescue them: under a baseURL with a path,
`relURL "/fi/guild/"` returns `/fi/guild/` and `absURL "/fi/guild/"` drops the
path too. Only a host of its own makes the tree work unmodified, which is one
of the reasons per-pull-request subdomains are worth their cost.

Leaving the baseURL at the production value fails quietly rather than loudly:
every link 404s, and the root alias sends the editor to the live site, where
everything looks right and none of their changes are present.

### Per-preview size

Built from the current content, the public tree is 9,554,306 bytes and the
member tree is 2,588,780 bytes, so a preview occupies about 12.6 MB.

## Changes in infra-prodeko

Terraform is not touched. The preview zone, its records and the service
principal exist already, created by hand, and declaring them in
`terraform/main.tf` would conflict with the live resources on the next apply.
Adopting them into Terraform later is an `import`, and is worth doing at the
same time as the parent zone rather than on its own.

The `caddy` role, which is shared with the kiltis host and must stay
backward compatible:

- `defaults/main.yml` gains `caddy_bin: caddy`, plus the download URL and
  `sha256` for the custom build, both empty by default.
- `tasks/install.yml` fetches the custom binary to `/usr/local/bin/caddy` with
  its checksum, renders `/etc/caddy/azure-dns.env` at 0640 `root:caddy` with
  `no_log`, installs the systemd drop-in, and asserts
  `dns.providers.azure` appears in `caddy list-modules`.
- `tasks/config.yml` validates with `{{ caddy_bin }}` and asserts that a host
  without the drop-in leaves `caddy_config_dest` where the stock unit reloads
  from; `handlers/main.yml` reloads through systemd.
- `README.md` describes the custom binary and how to re-pin it.

New roles, each modelled on an existing one:

- `roles/preview_gate/` is `member_gate` with its own port 4181, cookie domain
  `.preview.prodeko.org`, whitelist domain, cookie name, Keycloak client
  `prodeko-preview-gate`, and `preview_gate_required_role: prodeko-org-admin`.
  Its assert task additionally rejects a cookie domain that is not under
  `preview.prodeko.org`.
- `roles/prodeko_preview/` is `prodeko_site` with `sites/` in place of
  `releases/`, `templates/preview.sh.j2` in place of `publish.sh.j2`, and
  `files/deploy-prodeko-preview.pub`.

Configuration:

- `host_files/prodeko-vm2/Caddyfile.j2` gains the
  `preview.prodeko.org, *.preview.prodeko.org` block: the `tls` DNS challenge,
  the `map`, the apex handle carrying `/oauth2/*`, and the gated handle
  carrying `forward_auth`, the member matcher and the two roots.
- `playbooks/prodeko_vm.yml` sets `caddy_bin` on the caddy role and adds
  `preview_gate` and `prodeko_preview` after `prodeko_site`.
- `vars/prodeko_vm.yml` gains the DNS challenge configuration and two gatus
  endpoints. Only the client secret is a secret: `kv_secret_map` maps
  `preview_dns_client_secret` to the Key Vault secret
  `preview-dns-client-secret`. The other four values are identifiers rather
  than credentials and are plain variables:

  ```yaml
  preview_dns_subscription_id: ca8bb147-7f8b-4bf7-9832-6d0b568ca9e0
  preview_dns_resource_group: prodeko-rg
  preview_dns_client_id: c83356ff-a01b-4419-85cc-d11c1cca4aa9
  preview_dns_tenant_id: 74a41ed8-db28-4b67-ad9d-d0d96a330706
  ```

  The preview gate's Keycloak client secret and cookie secret are mapped
  alongside the existing member gate entries. The gatus endpoints are the apex
  expecting 200 and a preview subdomain expecting 302, which proves the gate is
  closed.
- `roles/member_gate/README.md` is corrected. See the last section.

## Changes in prodeko-org

- `.github/workflows/preview.yml` is new. It triggers on `pull_request` with
  types `opened`, `synchronize`, `reopened` and `closed`. The publish job runs
  only for same-repository pull requests, builds both trees at the preview
  baseURL, runs `./check-trees.sh`, removes `site/public/admin`, builds the
  search indexes with the same pinned Pagefind and the same three runs as
  `build.yml` and runs `./check-trees.sh` again against them, streams
  `public` and `members` into the forced command, and posts the commit status
  and a sticky comment. The cleanup job runs on `closed` and sends
  `preview delete`.
- `site/static/admin/config.yml` gains `preview_context: prodeko/preview` under
  `backend:`, and a `preview_path` per collection.
- `README.md` gains a short section on what a preview is and when it
  disappears, next to the description of the two trees.
- `docs/roadmap.md` drops preview builds from the "If time runs out" list.

Two details in the workflow are easy to get wrong and silently produce nothing.

The commit status must be posted against
`github.event.pull_request.head.sha`. Decap resolves an entry's `cms/` branch
to its pull request and requests
`GET /repos/{owner}/{repo}/commits/{head.sha}/status`, whereas `github.sha` on
a `pull_request` event is the merge commit. The editor login proxy already
allows that path; `proxy/internal/forward/allow.go` permits
`GET /commits/{sha}/status` with a comment naming this feature, so the proxy
needs no change.

`preview_context` must be set. With it unset, Decap matches any status context
containing the substring `deploy`, and `prodeko/preview` does not:

```typescript
export function isPreviewContext(context: string, previewContext: string) {
  if (previewContext) {
    return context === previewContext;
  }
  return PREVIEW_CONTEXT_KEYWORDS.some(keyword => context.includes(keyword));
}
```

Removing `site/public/admin` matters because the editing screen is static
content copied into every build. On an authenticated preview host a Decap that
loads and offers a login is plausible enough for an editor to try, and it fails
at the last step because the proxy's CORS allows the site's origin and not this
one. Only the public tree needs it: `config/members/` sets `staticDir = []`, so
the member tree has no copy.

Adding the preview host to `cms_auth_proxy_site_origins` is the natural-looking
response to that failure and is the one change that must not be made. It would
give preview pages credentialed access to the repository through the proxy.

## Setting it up

The DNS half is done. The child zone, the NS delegation from `prodeko.org`, the
apex and wildcard `A` records to 20.224.96.31, the `prodeko-preview-dns`
service principal scoped to the child zone, and its client secret in
`prodeko-vault` as `preview-dns-client-secret` all exist, and delegation and
wildcard resolution are verified live. That was the long pole, because an NS
delegation has to propagate before Caddy can complete a DNS challenge in the
child zone.

What remains, in dependency order:

1. Register the Keycloak client `prodeko-preview-gate` in the
   `membership-registry` realm: confidential, standard flow only, PKCE S256,
   one redirect URI `https://preview.prodeko.org/oauth2/callback`, and an
   Audience mapper with included client audience `prodeko-preview-gate` on both
   the ID and access tokens. Put its client secret and a fresh cookie secret in
   Key Vault. No realm roles mapper is needed, the same as the member gate and
   the opposite of the editor login proxy.
2. Generate the deploy key pair, commit the public half to
   `roles/prodeko_preview/files/`, and add the private half to the
   prodeko-org repository as the repository secret
   `PRODEKO_PREVIEW_DEPLOY_KEY`. It is deliberately not an environment secret:
   same-repository pull requests must be able to use it.
3. Run the play at a watched moment. Replacing the Caddy binary affects every
   vhost on the VM, so schedule it rather than doing it in passing. The
   rollback is to delete the drop-in, run `systemctl daemon-reload` and restart
   Caddy.
4. Confirm the certificate covers both names and that an anonymous request to a
   preview subdomain answers 302.
5. Sign in with an account that holds `membership` but not `prodeko-org-admin`
   and confirm it is refused. Nothing in the configuration fails loudly if the
   allowed role is wrong, so this negative test is the only real check that the
   gate gates.

## Retention and garbage collection

A preview is about 12.6 MB. The defaults keep at most 15 previews for at most
14 days, bounding the tree at roughly 190 MB. With a handful of editors the
realistic number of concurrent drafts is well under fifteen, and a draft older
than a fortnight is abandoned rather than pending.

Deletion happens three ways, because the first is not reliable on its own. The
`closed` event, which fires on both merge and discard, is the normal path and
is immediate. Every publish then prunes anything past the age limit, and prunes
oldest-first if the count is over the limit. Nothing runs on a timer: a cron
job would be a fourth place to look when something is wrong, and the
publish-time prune covers the same ground for as long as anyone is opening pull
requests.

## Rules

Previews build the merge commit, which is what an editor means by asking what
the site will look like. The commit status is still posted against the head
commit, because that is where Decap looks. When the merge reference does not
exist because the pull request conflicts, the job fails and says so in its
comment rather than quietly building the head instead.

Fork pull requests get no preview. GitHub does not expose secrets to them, so
the publish cannot run, and the job is guarded on the head repository matching
the base repository so that this shows as a skipped job rather than a failed
SSH on every outside contribution. Editors are unaffected: the editorial
workflow commits to `cms/` branches in the repository itself and never forks.

`pull_request_target` is not used. It would run trusted code with the deploy
key in scope against content from an untrusted branch.

## Risks accepted

The preview deploy key is a repository secret, so anyone who can push a branch
to the repository can publish content to a host that editors trust and that
carries member drafts. The key remains write-only and confined to one directory
tree, and pushing a branch already implies repository write access, so the
marginal capability is small. The alternative, an environment with required
reviewers, removes the automatic preview that is the point of the feature.

Caddy security updates do not arrive through apt, because the binary the
service runs is the custom build. Re-pinning the download checksum and
rerunning the play is the upgrade path, and it is the same discipline the
pinned oauth2-proxy image digest already requires.

## The member_gate README correction

`ansible/roles/member_gate/README.md` rules out Caddy plugins on the grounds
that they need `xcaddy`, which the apt-package constraint forbids. The VM runs
a custom Caddy build, so that constraint does not hold and the sentence has to
go.

The conclusion it supports does not change. oauth2-proxy is still the right
choice over a Caddy authentication plugin, for the reasons the same README
gives and which are untouched by this: it authorises on a claim value rather
than merely on a valid login, and a plugin would mean browser session security
maintained by nobody. Only the reasoning that appeals to the packaging
constraint is replaced.
