#!/bin/sh
# Usage: duckdb-licenses.sh
#
# Writes build/licenses/duckdb.txt: the licenses of DuckDB and of the
# libraries it bundles, from the DuckDB source of the version linked by
# duckdb-go-bindings (vMAJOR.MINOR.PATCH, encoded as v0.<M><mm><pp>.0).
# Run it after upgrading duckdb-go; notices.sh refuses an outdated file.
set -eu
cd "$(dirname "$0")/.."

N=$(sed -n 's|^\s*github.com/duckdb/duckdb-go-bindings v0\.\([0-9]*\)\..*|\1|p' go.mod)
VERSION=$((N / 10000)).$((N / 100 % 100)).$((N % 100))

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "https://github.com/duckdb/duckdb/archive/refs/tags/v$VERSION.tar.gz" |
	tar -xz -C "$TMP" --strip-components=1

# DuckDB itself, its compression algorithms, extensions and third-party
# libraries (not the test data, the shell or the other language packages).
FILES="LICENSE $(cd "$TMP" && find src extension third_party -type f \
	\( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' \) |
	grep -v '/test/' | LC_ALL=C sort)"

mkdir -p build/licenses
{
	echo "DuckDB $VERSION"
	echo
	echo "The licenses below come from the DuckDB source tree"
	echo "(https://github.com/duckdb/duckdb/tree/v$VERSION)."
	for f in $FILES; do
		printf '\n----- %s\n\n' "$f"
		cat "$TMP/$f"
	done
} > build/licenses/duckdb.txt
echo "wrote build/licenses/duckdb.txt (DuckDB $VERSION, $(echo $FILES | wc -w) files)"
