<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# GitHub Pages custom domains

`koryph dns github-pages` reconciles the narrowly scoped Cloudflare DNS
records required by a GitHub Pages custom domain. It is intended for the
domain setup step after GitHub Pages has been configured to deploy from
Actions.

The command creates any missing records and makes matching records DNS-only
with Cloudflare's automatic TTL:

- four apex `A` records and four apex `AAAA` records pointing to GitHub Pages;
- one `www` CNAME pointing to the supplied `<owner>.github.io` domain.

It does not delete records, and it does not expose general Cloudflare account
administration.

For a hostname such as `docs.example.com`, koryph looks for its Cloudflare
zone and then each parent in order, so it normally uses the `example.com`
zone. The token must have access to that selected parent zone.

## Prerequisites

Create a Cloudflare API token scoped only to the target zone, with `Zone:Read`
and `DNS:Edit` permissions. Store that token in one of koryph's supported vault
providers. Do not put the token in `koryph.project.json`, a shell environment
variable, or the command line: the command accepts only its vault reference.

When `--vault-provider` is omitted, koryph uses the normal fallback ladder:
the project vault default, legacy project signing vault, global vault default,
then the OS default provider.

## Configure DNS

Run the command from the registered project's directory, or pass its ID:

```sh
koryph dns github-pages --project myproject \
  --domain docs.example.com \
  --pages-domain acme.github.io \
  --vault-ref 'op://Infrastructure/Cloudflare Pages token'
```

Pass `--vault-provider` only when the token is in a provider other than the
configured default:

```sh
koryph dns github-pages --project myproject \
  --domain docs.example.com \
  --pages-domain acme.github.io \
  --vault-provider onepassword \
  --vault-ref 'op://Infrastructure/Cloudflare Pages token'
```

The token is retrieved in memory only for Cloudflare's HTTPS API request. It
is neither written to project configuration nor emitted in command output.

After the records propagate, set the custom domain in GitHub Pages and wait
for GitHub's DNS health check before enabling HTTPS enforcement.

## See also

- [Choosing a forge](forges.md)
- [`koryph dns github-pages` flags reference](../reference/cli.md#koryph-dns-github-pages)
