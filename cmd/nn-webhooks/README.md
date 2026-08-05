# nn-webhooks

The command-line interface for [NimbusNexus Webhooks](https://github.com/NimbusNexus/webhooks-go).

A single static binary — no runtime, no virtualenv, nothing to install first. That is the whole
reason it is Go: an operator is often on a box that has nothing on it.

## Install

```bash
# macOS / Linux
brew install nimbusnexus/tap/nn-webhooks

# with a Go toolchain
go install github.com/NimbusNexus/webhooks-go/cmd/nn-webhooks@latest
```

Any other platform: signed archives and a `checksums.txt` are attached to every
[release](https://github.com/NimbusNexus/webhooks-go/releases). The archive name carries the
version, so there is no version-agnostic download URL — take the one matching your platform:

```bash
curl -sSLO https://github.com/NimbusNexus/webhooks-go/releases/download/v0.5.1/nn-webhooks_0.5.1_linux_amd64.tar.gz
tar xzf nn-webhooks_0.5.1_linux_amd64.tar.gz
```

## Configure

```bash
nn-webhooks configure                 # prompts for URL + API key
nn-webhooks whoami                    # what would be used, and where it came from
```

Credentials live in `$XDG_CONFIG_HOME/nn-webhooks/credentials.json` (mode `0600`), as **named
profiles** — so a second deployment is a flag rather than overwriting the first:

```bash
nn-webhooks --profile staging configure --url https://api.staging.example.com
nn-webhooks --profile staging endpoints list
```

For CI, skip the file entirely:

```bash
export NN_WEBHOOKS_URL=https://api.webhooks.example.com
export NN_WEBHOOKS_API_KEY=whk_...
nn-webhooks deliveries list --status dead
```

Precedence is `--api-key` > `$NN_WEBHOOKS_API_KEY` > profile, and **`whoami` reports which one
won**. That matters more than it sounds: a stale environment variable silently shadowing the
profile you just wrote is the usual confusion, and "configured" on its own cannot explain it.

The key is never printed — only its last four characters, because the output of a diagnostic
command is exactly what gets pasted into an issue.

## Commands

```bash
nn-webhooks publish deploy.completed --data '{"id":1}' --idempotency-key deploy-1

nn-webhooks endpoints list                       # add --project-id to scope it
nn-webhooks endpoints create --url https://example.com/hook --subscribe prefix:deploy.
nn-webhooks endpoints get ep_123
nn-webhooks endpoints update ep_123 --set max_attempts=10 --set status=disabled
nn-webhooks endpoints rotate-secret ep_123
nn-webhooks endpoints enable ep_123              # recover an auto-disabled endpoint
nn-webhooks endpoints delete ep_123

nn-webhooks keys create --name ci --scope publish --expires-in-days 90
nn-webhooks keys revoke key_123

nn-webhooks deliveries list --status dead
nn-webhooks deliveries redeliver dlv_456

# Verify a received webhook — the raw body comes from stdin; prints "ok"/"failed", exits 0/1:
nn-webhooks verify --secret whsec_... --signature sha256=... --timestamp "$TS" < body.json
```

Subscriptions are `kind:pattern` and the flag repeats — `prefix:deploy.`, `exact:deploy.completed`,
`suffix:.failed`. A value with no colon is a bare match kind with an empty pattern, which is how you
subscribe to everything: `--subscribe all`. Values given to `--set` are JSON-coerced, falling back to
a string; use `null` to clear a field.

Everything prints JSON, so it pipes into `jq` without a `--format` flag to remember.

## Versions

`nn-webhooks version` reports two numbers, and they can legitimately differ:

- `version` — the release this binary came from (stamped at build time)
- `sdk` — the client library compiled into it

A binary built from source rather than a release has no tag to name, so it reports the SDK version
with a `+source` suffix instead of pretending to be a release.
