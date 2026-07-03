# examples

Reference uploader scripts for the **large-file fallback** for `send_document`
(see the main README, "Large-file fallback"). Point `upload_command` at one:

```toml
upload_command = "~/uploaders/rsync-upload.sh"
upload_max_mb  = 300
```

## The uploader contract

ak-tgclaude runs the command **unsandboxed** (from the dispatcher — it needs the
network) as argv `[command, <file>, <name>]`:

| arg | meaning |
|-----|---------|
| `$1` | local path to the file to upload |
| `$2` | a **collision-free** destination basename ak-tgclaude generated — a random prefix joined to the original name, e.g. `a3f9c2e1-dist.tar.gz` |

- **Use `$2` as the destination name.** Two files that share a name (two
  `dist.tar.gz`) get distinct `$2` values, so they never clobber each other on the
  share host. A one-arg uploader that reuses the *original* basename **does**
  clobber — that is exactly what `$2` fixes.
- **Success:** print **only** the public URL on stdout and exit `0`.
- **Failure:** a message on stderr and a **non-zero** exit — print no URL. The
  dispatcher surfaces the stderr to the model as a tool error.

## Non-ASCII / Cyrillic names

`$2` preserves the original filename, so it may contain **non-ASCII** (e.g.
Cyrillic) or spaces. That is fine as a filename on the share host, but a raw
non-ASCII string is **not a valid URL** — an uploader that builds a URL must
**percent-encode** the name for the URL path (the file keeps its real name on
disk; only the link is encoded). `rsync-upload.sh` shows a pure-bash byte-wise
encoder (`LC_ALL=C`, each UTF-8 byte → `%XX`). ak-tgclaude passes the name through
`argv` (never a shell), so quoting `"$2"` in the uploader is enough to stay safe;
`rsync -s` keeps a spaced name intact across the ssh hop.

## `rsync-upload.sh`

rsyncs the file to `SHARE_REMOTE:SHARE_DIR/$name` over ssh and prints
`SHARE_URLBASE/<url-encoded name>`. Configure the destination via environment
(nothing is hardcoded):

```sh
SHARE_REMOTE=user@host \
SHARE_DIR=/srv/share \
SHARE_URLBASE=https://share.example/pub \
  rsync-upload.sh ./big.tar.gz a3f9c2e1-big.tar.gz
```

ssh access to `SHARE_REMOTE` is set up out of band (a key the dispatcher's user can
use non-interactively).
