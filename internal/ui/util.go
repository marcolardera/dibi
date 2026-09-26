// Package ui implements the Dibi main window with Qt (via miqt).
//
// Threading rule: Qt objects are only touched on the UI thread. Database
// work runs in goroutines and reports back with async/onUI.
package ui

import (
	"html"
	"strings"
	"unicode/utf16"
	"unsafe"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
)

// async runs work in a goroutine and then done on the UI thread.
func async[T any](work func() T, done func(T)) {
	go func() {
		r := work()
		mainthread.Start(func() { done(r) })
	}()
}

// onUI runs f on the UI thread, without waiting.
func onUI(f func()) { mainthread.Start(f) }

// onUIWait runs f on the UI thread and waits for it to finish.
func onUIWait(f func()) { mainthread.Wait(f) }

// key identifies a Qt object in Go maps.
func key(p unsafe.Pointer) uintptr { return uintptr(p) }

// icon returns a theme icon, falling back to a standard Qt icon.
func icon(name string, fallback qt.QStyle__StandardPixmap) *qt.QIcon {
	return qt.QIcon_FromTheme2(name, qt.QApplication_Style().StandardIcon(fallback, nil, nil))
}

// monoFont returns the system fixed-width font.
func monoFont() *qt.QFont {
	return qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont)
}

// hbox builds a horizontal layout without margins holding the widgets.
func hbox(widgets ...*qt.QWidget) *qt.QHBoxLayout {
	l := qt.NewQHBoxLayout2()
	l.SetContentsMargins(0, 0, 0, 0)
	for _, w := range widgets {
		l.AddWidget(w)
	}
	return l
}

// utf16Len returns the length of s in UTF-16 code units, the unit Qt uses
// for text positions.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// errorHTML formats an error message for a QPlainTextEdit.
func errorHTML(prefix, msg string) string {
	return `<span style="color:#b00020;">` + html.EscapeString(prefix) +
		strings.ReplaceAll(html.EscapeString(msg), "\n", "<br>") + `</span>`
}

// firstLine returns the first line of s, shortened to max runes.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i] + " …"
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
