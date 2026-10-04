# Prints treedigest.Tree of $1 with busybox alone; forks per batch, never per entry (per-entry forking took >10 min on 50k files).
set -eu
set -o pipefail
cd "$1"
export LC_ALL=C
nl='
'
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

hexnames() {
	od -An -v -tx1 | awk '{ for (i = 1; i <= NF; i++) { if ($i == "00") { print substr(h, 5); h = "" } else h = h $i } }'
}

# Names holding a newline take a per-file path: sha256sum's "hash  name" output is one line per file only without them.
files() {
	x=$1
	shift
	find . -type f "$@" ! -path "*$nl*" -print0 >"$tmp/n"
	find . -type f "$@" -path "*$nl*" -print0 >"$tmp/m"
	xargs -0 -r sha256sum -- <"$tmp/n" | cut -c1-64 >"$tmp/s"
	xargs -0 -r sh -c 'set -eu; for p; do s=$(sha256sum <"$p"); echo "${s%% *}"; done' sh <"$tmp/m" >>"$tmp/s"
	cat "$tmp/n" "$tmp/m" | hexnames >"$tmp/h"
	# A count mismatch means a misread name; fail rather than hash a shifted pairing.
	[ "$(wc -l <"$tmp/h")" = "$(wc -l <"$tmp/s")" ]
	paste -d' ' "$tmp/h" "$tmp/s" | awk -v x="$x" '{ print $1 " f " x " " $2 }'
}

{
	find . -mindepth 1 -type d -print0 | hexnames | awk '{ print $1 " d" }'
	find . -mindepth 1 ! -type d ! -type f ! -type l -print0 | hexnames | awk '{ print $1 " o" }'
	# readlink appends exactly one newline, stripped by the sub().
	find . -type l -exec sh -c 'set -eu; for p; do printf "%s\0" "$p"; readlink "$p"; printf "\0"; done' sh {} + |
		od -An -v -tx1 |
		awk '{ for (i = 1; i <= NF; i++) { if ($i == "00") { if (n) { sub(/0a$/, "", h); print p " l " h; n = 0 } else { p = substr(h, 5); n = 1 }; h = "" } else h = h $i } }'
	files x -perm +111
	files - ! -perm +111
} >"$tmp/all"

sort "$tmp/all" | sha256sum | cut -c1-64
