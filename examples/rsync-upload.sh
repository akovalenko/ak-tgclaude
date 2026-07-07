#!/usr/bin/env bash
# Example ak-tgclaude uploader — rsync a file to a share host over ssh and print
# its public URL. Point `upload_command` at a copy of this (adapt the env below):
#
#   upload_command = "~/uploaders/rsync-upload.sh"
#
# ── The send_document large-file fallback contract ──────────────────────────────
# ak-tgclaude invokes the uploader as argv [<this>, <file>, <name>]:
#   $1  the file to read and upload. ak-tgclaude passes a /proc/self/fd/N handle to an
#       already-opened, symlink-vetted fd (NOT a plain path) — just read it (rsync -L,
#       cat, or curl -T all work). Do not derive the destination name from it (it is
#       "3", not the real name) — that is what arg2 is for.
#   $2  a COLLISION-FREE destination basename ak-tgclaude generated: a random prefix
#       joined to the original name, e.g. a3f9c2e1-dist.tar.gz. Use it as the
#       destination so two files that share a name (two dist.tar.gz) never clobber
#       each other on the share host. (share-upload.sh-style one-arg uploaders that
#       reuse the ORIGINAL basename DO clobber — that is what arg2 fixes.) If arg2 is
#       empty — a manual one-arg run — fall back to the source basename.
# On success: print ONLY the public URL on stdout and exit 0.
# On failure: a message on stderr, a non-zero exit, and no URL (set -e guarantees
# the closing echo is never reached after a failed rsync).
#
# Runs UNSANDBOXED in the dispatcher (rsync needs the network); ssh access to
# SHARE_REMOTE is set up out of band.
set -euo pipefail

# Destination, via env — this is an example, so nothing is hardcoded:
#   SHARE_REMOTE   ssh target,           e.g. user@host
#   SHARE_DIR      remote directory rsync writes into
#   SHARE_URLBASE  public base URL that directory is served at (no trailing slash)
: "${SHARE_REMOTE:?set SHARE_REMOTE=user@host}"
: "${SHARE_DIR:?set SHARE_DIR=/remote/share/dir}"
: "${SHARE_URLBASE:?set SHARE_URLBASE=https://share.example/dir}"

src="${1:?usage: rsync-upload.sh <file> [dest-name]}"
[ -f "$src" ] || { echo "rsync-upload: not a readable file: $src" >&2; exit 1; }

# Prefer ak-tgclaude's collision-free name (arg2); fall back to the source basename
# for a manual one-arg run.
name="${2:-}"
[ -n "$name" ] || name="$(basename -- "$src")"

# Percent-encode the name for the URL. The original name may hold non-ASCII (e.g.
# Cyrillic) or spaces: the file keeps its real name on the share host, but the URL
# path must be encoded to be a valid link. LC_ALL=C makes the loop walk BYTES, so
# each UTF-8 byte becomes %XX — exactly what a URL wants.
urlencode() {
	local LC_ALL=C c out= i
	for (( i = 0; i < ${#1}; i++ )); do
		c="${1:i:1}"
		case "$c" in
			[a-zA-Z0-9._~-]) out+="$c" ;;
			*) printf -v c '%%%02X' "'$c"; out+="$c" ;;
		esac
	done
	printf '%s' "$out"
}

# -L (--copy-links): the source is /proc/self/fd/N — a procfs magic symlink to the
# already-opened, symlink-vetted fd — so without -L rsync says "skipping non-regular
# file". -L follows it and copies the referent. SAFE *because* the source is a
# /proc/self/fd handle: following it resolves to the exact open inode ak-tgclaude
# vetted (O_NOFOLLOW), NOT a re-resolution of a path string, so a symlink swapped in at
# the original path cannot redirect the copy. Do NOT read this as "-L is always fine":
# -L on a symlink in an ordinary directory IS a path re-resolve.
# -s (--protect-args): do not word-split the remote path, so a name with a space
# (or other shell-special char) reaches the far side intact. Quiet (no -v): silent
# on success, genuine errors still go to stderr.
rsync -L -s -- "$src" "$SHARE_REMOTE:$SHARE_DIR/$name"

echo "$SHARE_URLBASE/$(urlencode "$name")"
