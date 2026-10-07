package ui

import (
	qt "github.com/mappu/miqt/qt6"
)

// Theme names, as stored in the state file.
const (
	themeLight = "light"
	themeDark  = "dark"
)

// theme holds the colors of a theme: the Qt palette and the colors Dibi
// paints itself. The text colors have a contrast of at least 4.5:1 against
// the background they are drawn on (comments and NULLs are muted on purpose).
type theme struct {
	name string
	dark bool

	// Palette roles (active and inactive groups) and the disabled ones.
	palette  map[qt.QPalette__ColorRole]string
	disabled map[qt.QPalette__ColorRole]string

	// SQL syntax.
	keyword, number, quoted, str, comment string
	match                                 string // background of search matches

	gutter, lineNumber, currentLine string // editor gutter
	muted                           string // NULLs, placeholders, "(empty)"
	errText, okText                 string
}

var lightTheme = &theme{
	name: themeLight,
	// Fusion's standard light palette (the same on every platform), with an
	// AlternateBase dark enough for the grid stripes to be seen.
	palette: map[qt.QPalette__ColorRole]string{
		qt.QPalette__AlternateBase: "#eef1f5",
	},
	keyword: "#0033b3", number: "#1750eb", quoted: "#871094", str: "#067d17", comment: "#8c8c8c",
	match:  "#ffe58a",
	gutter: "#f3f3f3", lineNumber: "#9a9a9a", currentLine: "#303030",
	muted:   "#888888",
	errText: "#b00020", okText: "#1b7f3b",
}

var darkTheme = &theme{
	name: themeDark,
	dark: true,
	palette: map[qt.QPalette__ColorRole]string{
		qt.QPalette__Window:          "#2b2d30",
		qt.QPalette__WindowText:      "#dfe1e5",
		qt.QPalette__Base:            "#1e1f22",
		qt.QPalette__AlternateBase:   "#272a2f",
		qt.QPalette__Text:            "#dfe1e5",
		qt.QPalette__PlaceholderText: "#8f939a",
		qt.QPalette__Button:          "#35373b",
		qt.QPalette__ButtonText:      "#dfe1e5",
		qt.QPalette__BrightText:      "#ffffff",
		qt.QPalette__ToolTipBase:     "#393b40",
		qt.QPalette__ToolTipText:     "#dfe1e5",
		qt.QPalette__Highlight:       "#2f65ca",
		qt.QPalette__HighlightedText: "#ffffff",
		qt.QPalette__Link:            "#6ea8fe",
		qt.QPalette__LinkVisited:     "#c39af0",
		// Fusion draws frames and bevels with these.
		qt.QPalette__Light:    "#4a4c51",
		qt.QPalette__Midlight: "#3e4045",
		qt.QPalette__Mid:      "#26282b",
		qt.QPalette__Dark:     "#1a1b1e",
		qt.QPalette__Shadow:   "#0e0e10",
	},
	disabled: map[qt.QPalette__ColorRole]string{
		qt.QPalette__WindowText:      "#7d8189",
		qt.QPalette__Text:            "#7d8189",
		qt.QPalette__ButtonText:      "#7d8189",
		qt.QPalette__Base:            "#25272a",
		qt.QPalette__Button:          "#2f3134",
		qt.QPalette__Highlight:       "#43464c",
		qt.QPalette__HighlightedText: "#a0a4ab",
	},
	// The colors of IntelliJ's dark scheme, the counterpart of the light ones.
	keyword: "#cf8e6d", number: "#2aacb8", quoted: "#c77dbb", str: "#6aab73", comment: "#8c9098",
	match:  "#463c10",
	gutter: "#25272a", lineNumber: "#7d8189", currentLine: "#dfe1e5",
	muted:   "#9a9ea5",
	errText: "#f2777a", okText: "#5fb878",
}

// th is the current theme.
var th = lightTheme

func themeByName(name string) *theme {
	if name == themeDark {
		return darkTheme
	}
	return lightTheme
}

func color(hex string) *qt.QColor { return qt.NewQColor6(hex) }

// applyTheme sets the application style and palette. Dibi always uses the
// Fusion style: it draws everything from the palette on every platform,
// while the native Windows and macOS styles ignore custom palettes. Widgets
// with colors of their own update them with onThemeChange.
func applyTheme(t *theme) {
	th = t
	if qt.QApplication_Style().Name() != "fusion" {
		qt.QApplication_SetStyleWithStyle("Fusion")
	}
	pal := qt.QApplication_Style().StandardPalette()
	for role, c := range t.palette {
		pal.SetColor2(role, color(c))
	}
	for role, c := range t.disabled {
		pal.SetColor(qt.QPalette__Disabled, role, color(c))
	}
	qt.QApplication_SetPalette(pal)
}

// onThemeChange calls f each time the theme changes, for as long as w lives.
// Widgets learn of it from the change of their palette; f runs once per
// theme, as it may change the palette again (style sheets do).
func onThemeChange(w *qt.QWidget, f func()) {
	applied := th
	filter := qt.NewQObject2(w.QObject)
	filter.OnEventFilter(func(_ func(*qt.QObject, *qt.QEvent) bool, _ *qt.QObject, ev *qt.QEvent) bool {
		if ev.Type() == qt.QEvent__PaletteChange && applied != th {
			applied = th
			f()
		}
		return false
	})
	w.InstallEventFilter(filter)
}

// setTextColor colors the text of a label (no color: the palette's).
func setTextColor(l *qt.QLabel, hex string) {
	if hex == "" {
		l.SetStyleSheet("")
	} else {
		l.SetStyleSheet("color: " + hex + ";")
	}
}

// themeIcon draws the icon of the theme switch: a moon to switch to the
// dark theme, a sun to switch back. It is drawn rather than taken from the
// icon theme, which only exists on Linux.
func themeIcon(dark bool) *qt.QIcon {
	const size = 64
	pm := qt.NewQPixmap2(size, size)
	pm.FillWithFillColor(qt.NewQColor2(qt.Transparent))
	p := qt.NewQPainter2(pm.QPaintDevice)
	p.SetRenderHint(qt.QPainter__Antialiasing)
	p.SetPenWithStyle(qt.NoPen)
	if dark {
		// Sun: a disc and eight rays.
		p.SetBrush(qt.NewQBrush3(color("#f0b429")))
		p.DrawEllipse3(qt.NewQPointF3(32, 32), 13, 13)
		pen := qt.NewQPen3(color("#f0b429"))
		pen.SetWidthF(5)
		pen.SetCapStyle(qt.RoundCap)
		p.SetPenWithPen(pen)
		p.Translate2(32, 32)
		for i := 0; i < 8; i++ {
			p.DrawLine2(0, -20, 0, -27)
			p.Rotate(45)
		}
	} else {
		// Moon: a disc with an offset disc cut out of it.
		p.SetBrush(qt.NewQBrush3(color("#5b6b8c")))
		p.DrawEllipse3(qt.NewQPointF3(30, 34), 24, 24)
		p.SetCompositionMode(qt.QPainter__CompositionMode_Clear)
		p.DrawEllipse3(qt.NewQPointF3(44, 20), 20, 20)
	}
	p.End()
	return qt.NewQIcon2(pm)
}
