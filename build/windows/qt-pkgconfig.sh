#!/bin/sh
# Usage: qt-pkgconfig.sh <qt prefix> [output dir]
#
# Writes pkg-config files for a static Qt 6 for Windows (a static Qt installs
# none) into the output dir, by default <qt prefix>/lib/pkgconfig. miqt asks
# for Qt6Core and Qt6Widgets; with the windowsqtstatic tag it imports the
# windows platform and style plugins, so Qt6Widgets links them too.
#
# The libraries come from Qt's .prl files, which list what each library or
# plugin needs (Qt's own resource objects, third-party and system libraries).
# Works with the Qt built in the Dockerfile and with MSYS2's qt6-static.
set -eu
P=$1
OUT=${2:-$P/lib/pkgconfig}

INC=$P/include
[ -d "$INC/qt6" ] && INC=$INC/qt6
PLUGINS=$P/plugins
[ -d "$PLUGINS" ] || PLUGINS=$P/share/qt6/plugins
VERSION=$(sed -n 's/^#define QT_VERSION_STR "\(.*\)".*/\1/p' "$INC/QtCore/qconfig.h" | tr -d '\r')

# prl_libs <file>: the QMAKE_PRL_LIBS of a .prl file, with paths resolved
# (MSYS2's .prl files have CRLF line endings).
prl_libs() {
	tr -d '\r' < "$1" | sed -n 's/^QMAKE_PRL_LIBS = //p' | sed \
		-e "s|\\\$\\\$\\[QT_INSTALL_LIBS\\]|$P/lib|g" \
		-e "s|\\\$\\\$\\[QT_INSTALL_PREFIX\\]|$P|g" \
		-e "s|\\\$\\\$\\[QT_INSTALL_PLUGINS\\]|$PLUGINS|g"
}

# uniq: the words of stdin, each once, in order of first appearance.
uniq() {
	tr -s ' \t\n' '\n\n\n' | awk 'NF && !seen[$0]++' | tr '\n' ' '
}

header() {
	printf 'Name: %s\nDescription: %s (static)\nVersion: %s\n' "$1" "$1" "$VERSION"
}

mkdir -p "$OUT"

{
	header Qt6Core
	echo "Cflags: -I$INC -I$INC/QtCore -DQT_CORE_LIB"
	echo "Libs: $( { echo "$P/lib/libQt6Core.a"; prl_libs "$P/lib/Qt6Core.prl"; } | uniq)-static"
} > "$OUT/Qt6Core.pc"

# pkg-config mangles repeated entries, so each library is listed once and
# the link order is left to a linker group.
WIDGETS=$( {
	prl_libs "$PLUGINS/platforms/qwindows.prl"
	prl_libs "$PLUGINS/styles/qmodernwindowsstyle.prl"
	prl_libs "$P/lib/Qt6Widgets.prl"
} | uniq)
{
	header Qt6Widgets
	echo "Cflags: -I$INC -I$INC/QtCore -I$INC/QtGui -I$INC/QtWidgets -DQT_CORE_LIB -DQT_GUI_LIB -DQT_WIDGETS_LIB"
	echo "Libs: -Wl,--start-group ${WIDGETS}-Wl,--end-group -static"
} > "$OUT/Qt6Widgets.pc"
