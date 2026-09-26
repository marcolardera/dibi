package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dibi/internal/config"
	"dibi/internal/db"
	"dibi/internal/export"

	qt "github.com/mappu/miqt/qt6"
)

// connectionDialog asks for the parameters of a connection. It returns the
// connection and true if the user saved it.
func connectionDialog(parent *qt.QWidget, store *config.Store, existing *config.Connection) (config.Connection, bool) {
	dlg := qt.NewQDialog(parent)
	title := "New Connection"
	if existing != nil {
		title = "Edit Connection"
	}
	dlg.SetWindowTitle(title)
	dlg.SetMinimumWidth(460)
	layout := qt.NewQVBoxLayout(dlg.QWidget)
	form := qt.NewQFormLayout2()
	layout.AddLayout(form.QLayout)

	typeCombo := qt.NewQComboBox2()
	for _, t := range config.Types {
		typeCombo.AddItem4(connIcon(t), t.Label(), qt.NewQVariant14(string(t)))
	}
	name := qt.NewQLineEdit2()
	host := qt.NewQLineEdit2()
	port := qt.NewQSpinBox2()
	port.SetRange(1, 65535)
	database := qt.NewQLineEdit2()
	user := qt.NewQLineEdit2()
	password := qt.NewQLineEdit2()
	password.SetEchoMode(qt.QLineEdit__Password)
	encrypt := qt.NewQCheckBox3("Encrypt connection")
	trustCert := qt.NewQCheckBox3("Trust server certificate")
	trustCert.SetToolTip("Accept a self-signed or otherwise unverified server certificate")
	tlsRow := qt.NewQWidget2()
	tlsRow.SetLayout(hbox(encrypt.QWidget, trustCert.QWidget).QLayout)
	path := qt.NewQLineEdit2()
	browse := qt.NewQPushButton3("Browse…")
	pathRow := qt.NewQWidget2()
	pathLayout := hbox(path.QWidget, browse.QWidget)
	pathRow.SetLayout(pathLayout.QLayout)

	dbLabel := qt.NewQLabel3("Database:")
	form.AddRow3("Type:", typeCombo.QWidget)
	form.AddRow3("Name:", name.QWidget)
	hostLabel, portLabel, userLabel, pwLabel, pathLabel :=
		qt.NewQLabel3("Host:"), qt.NewQLabel3("Port:"), qt.NewQLabel3("User:"), qt.NewQLabel3("Password:"), qt.NewQLabel3("File:")
	form.AddRow(hostLabel.QWidget, host.QWidget)
	form.AddRow(portLabel.QWidget, port.QWidget)
	form.AddRow(dbLabel.QWidget, database.QWidget)
	form.AddRow(userLabel.QWidget, user.QWidget)
	form.AddRow(pwLabel.QWidget, password.QWidget)
	tlsLabel := qt.NewQLabel3("Security:")
	form.AddRow(tlsLabel.QWidget, tlsRow)
	form.AddRow(pathLabel.QWidget, pathRow)

	status := qt.NewQLabel2()
	status.SetWordWrap(true)
	status.SetTextInteractionFlags(qt.TextSelectableByMouse)
	layout.AddWidget(status.QWidget)

	buttons := qt.NewQDialogButtonBox2()
	testBtn := buttons.AddButton2("Test Connection", qt.QDialogButtonBox__ActionRole)
	buttons.AddButtonWithButton(qt.QDialogButtonBox__Save)
	buttons.AddButtonWithButton(qt.QDialogButtonBox__Cancel)
	layout.AddWidget(buttons.QWidget)

	currentType := func() config.Type { return config.Type(typeCombo.CurrentData().ToString()) }
	serverRows := []*qt.QWidget{hostLabel.QWidget, host.QWidget, portLabel.QWidget, port.QWidget,
		dbLabel.QWidget, database.QWidget, userLabel.QWidget, user.QWidget, pwLabel.QWidget, password.QWidget}
	fileRows := []*qt.QWidget{pathLabel.QWidget, pathRow}
	updateFields := func(resetPort bool) {
		t := currentType()
		for _, w := range serverRows {
			w.SetVisible(!t.IsFile())
		}
		for _, w := range fileRows {
			w.SetVisible(t.IsFile())
		}
		tlsLabel.SetVisible(t == config.MSSQL)
		tlsRow.SetVisible(t == config.MSSQL)
		if t == config.Oracle {
			dbLabel.SetText("Service name:")
		} else {
			dbLabel.SetText("Database:")
		}
		if t == config.MSSQL {
			host.SetPlaceholderText(`host or host\instance`)
			user.SetPlaceholderText("empty for Windows authentication")
		} else {
			host.SetPlaceholderText("")
			user.SetPlaceholderText("")
		}
		if resetPort && !t.IsFile() {
			port.SetValue(t.DefaultPort())
		}
		dlg.AdjustSize()
	}
	typeCombo.OnActivated(func(int) { updateFields(true) })

	browse.OnClicked(func() {
		filter := "All files (*)"
		switch currentType() {
		case config.SQLite:
			filter = "SQLite databases (*.db *.sqlite *.sqlite3 *.db3);;All files (*)"
		case config.DuckDB:
			filter = "DuckDB databases (*.duckdb *.db);;All files (*)"
		case config.CSV:
			filter = "CSV files (*.csv *.tsv *.txt);;All files (*)"
		case config.Parquet:
			filter = "Parquet files (*.parquet);;All files (*)"
		}
		if f := qt.QFileDialog_GetOpenFileName4(dlg.QWidget, "Choose File", filepath.Dir(path.Text()), filter); f != "" {
			path.SetText(f)
			if name.Text() == "" {
				name.SetText(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)))
			}
		}
	})

	collect := func() config.Connection {
		c := config.Connection{Name: strings.TrimSpace(name.Text()), Type: currentType()}
		if c.Type.IsFile() {
			c.Path = strings.TrimSpace(path.Text())
		} else {
			c.Host = strings.TrimSpace(host.Text())
			c.Port = port.Value()
			c.Database = strings.TrimSpace(database.Text())
			c.User = strings.TrimSpace(user.Text())
			c.Password = password.Text()
			if c.Type == config.MSSQL {
				c.Encrypt, c.TrustServerCert = encrypt.IsChecked(), trustCert.IsChecked()
			}
		}
		return c
	}
	showStatus := func(ok bool, msg string) {
		color := "#1b7f3b"
		if !ok {
			color = "#b00020"
		}
		status.SetStyleSheet("color: " + color + ";")
		status.SetText(msg)
	}

	testBtn.OnClicked(func() {
		c := collect()
		if c.Name == "" {
			c.Name = "test"
		}
		if err := c.Validate(); err != nil {
			showStatus(false, err.Error())
			return
		}
		testBtn.SetEnabled(false)
		showStatus(true, "Connecting…")
		start := time.Now()
		async(func() error {
			src, err := db.NewSource(c)
			if err != nil {
				return err
			}
			defer src.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return src.Ping(ctx)
		}, func(err error) {
			testBtn.SetEnabled(true)
			if err != nil {
				showStatus(false, "Connection failed: "+err.Error())
				return
			}
			showStatus(true, fmt.Sprintf("Connection succeeded (%d ms).", time.Since(start).Milliseconds()))
		})
	})

	var result config.Connection
	saved := false
	oldName := ""
	if existing != nil {
		oldName = existing.Name
	}
	buttons.OnAccepted(func() {
		c := collect()
		if err := store.SaveConnection(c, oldName); err != nil {
			showStatus(false, err.Error())
			return
		}
		result, saved = c, true
		dlg.Accept()
	})
	buttons.OnRejected(dlg.Reject)

	// Initial values.
	if existing != nil {
		typeCombo.SetCurrentIndex(max(typeCombo.FindData(qt.NewQVariant14(string(existing.Type))), 0))
		name.SetText(existing.Name)
		host.SetText(existing.Host)
		port.SetValue(existing.Port)
		database.SetText(existing.Database)
		user.SetText(existing.User)
		password.SetText(existing.Password)
		encrypt.SetChecked(existing.Encrypt)
		trustCert.SetChecked(existing.TrustServerCert)
		path.SetText(existing.Path)
		updateFields(false)
	} else {
		host.SetText("localhost")
		updateFields(true)
	}
	name.SetFocus()
	dlg.Exec()
	return result, saved
}

// exportDialog asks for export options. It returns false if cancelled.
func exportDialog(parent *qt.QWidget, note string) (export.Options, bool) {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("Export Data")
	dlg.SetMinimumWidth(480)
	layout := qt.NewQVBoxLayout(dlg.QWidget)

	if note == "" {
		note = "The query is run again and all its rows are exported, not only the pages shown."
	}
	noteLabel := qt.NewQLabel3(note)
	noteLabel.SetWordWrap(true)
	layout.AddWidget(noteLabel.QWidget)

	form := qt.NewQFormLayout2()
	layout.AddLayout(form.QLayout)
	csvRadio := qt.NewQRadioButton3("CSV")
	xlsxRadio := qt.NewQRadioButton3("Excel (XLSX)")
	csvRadio.SetChecked(true)
	formatRow := qt.NewQWidget2()
	fl := hbox(csvRadio.QWidget, xlsxRadio.QWidget)
	fl.AddStretch()
	formatRow.SetLayout(fl.QLayout)
	form.AddRow3("Format:", formatRow)

	home, _ := os.UserHomeDir()
	path := qt.NewQLineEdit3(filepath.Join(home, "export.csv"))
	browse := qt.NewQPushButton3("Browse…")
	pathRow := qt.NewQWidget2()
	pathRow.SetLayout(hbox(path.QWidget, browse.QWidget).QLayout)
	form.AddRow3("File:", pathRow)

	header := qt.NewQCheckBox3("Write column names in the first row")
	header.SetChecked(true)
	form.AddRow3("", header.QWidget)

	sepCombo := qt.NewQComboBox2()
	seps := []struct {
		label string
		r     rune
	}{{"Comma ( , )", ','}, {"Semicolon ( ; )", ';'}, {"Tab", '\t'}, {"Pipe ( | )", '|'}}
	for _, s := range seps {
		sepCombo.AddItem(s.label)
	}
	nullText := qt.NewQLineEdit2()
	nullText.SetPlaceholderText("empty")
	sepLabel, nullLabel := qt.NewQLabel3("Separator:"), qt.NewQLabel3("NULL as:")
	form.AddRow(sepLabel.QWidget, sepCombo.QWidget)
	form.AddRow(nullLabel.QWidget, nullText.QWidget)

	sheet := qt.NewQLineEdit3("Sheet1")
	sheetLabel := qt.NewQLabel3("Sheet name:")
	form.AddRow(sheetLabel.QWidget, sheet.QWidget)

	isXLSX := func() bool { return xlsxRadio.IsChecked() }
	update := func() {
		for _, w := range []*qt.QWidget{sepLabel.QWidget, sepCombo.QWidget, nullLabel.QWidget, nullText.QWidget} {
			w.SetVisible(!isXLSX())
		}
		sheetLabel.SetVisible(isXLSX())
		sheet.SetVisible(isXLSX())
		// Keep the extension in line with the format.
		p := path.Text()
		ext := ".csv"
		if isXLSX() {
			ext = ".xlsx"
		}
		if e := strings.ToLower(filepath.Ext(p)); e == ".csv" || e == ".xlsx" {
			path.SetText(strings.TrimSuffix(p, filepath.Ext(p)) + ext)
		}
	}
	csvRadio.OnToggled(func(bool) { update() })
	browse.OnClicked(func() {
		filter := "CSV files (*.csv)"
		if isXLSX() {
			filter = "Excel files (*.xlsx)"
		}
		if f := qt.QFileDialog_GetSaveFileName4(dlg.QWidget, "Export To", path.Text(), filter); f != "" {
			path.SetText(f)
		}
	})

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
	buttons.Button(qt.QDialogButtonBox__Ok).SetText("Export")
	layout.AddWidget(buttons.QWidget)
	var opts export.Options
	ok := false
	buttons.OnAccepted(func() {
		p := strings.TrimSpace(path.Text())
		if p == "" {
			qt.QMessageBox_Warning(dlg.QWidget, "Export", "Choose a file to export to.")
			return
		}
		if _, err := os.Stat(p); err == nil {
			if qt.QMessageBox_Question(dlg.QWidget, "Export", fmt.Sprintf("%s already exists. Replace it?", p)) != qt.QMessageBox__Yes {
				return
			}
		}
		opts = export.Options{Path: p, Header: header.IsChecked()}
		if isXLSX() {
			opts.Format = export.XLSX
			opts.SheetName = strings.TrimSpace(sheet.Text())
		} else {
			opts.Format = export.CSV
			opts.Separator = seps[max(sepCombo.CurrentIndex(), 0)].r
			opts.NullText = nullText.Text()
		}
		ok = true
		dlg.Accept()
	})
	buttons.OnRejected(dlg.Reject)
	update()
	dlg.Exec()
	return opts, ok
}

// openQueryDialog lets the user pick a saved query. It returns its name.
func openQueryDialog(parent *qt.QWidget, store *config.Store) (string, bool) {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("Open Saved Query")
	dlg.Resize(420, 440)
	layout := qt.NewQVBoxLayout(dlg.QWidget)
	filter := qt.NewQLineEdit2()
	filter.SetPlaceholderText("Filter…")
	filter.SetClearButtonEnabled(true)
	layout.AddWidget(filter.QWidget)
	list := qt.NewQListWidget2()
	layout.AddWidget(list.QWidget)

	fill := func() {
		list.Clear()
		names, err := store.Queries()
		if err != nil {
			qt.QMessageBox_Warning(dlg.QWidget, "Open", err.Error())
		}
		list.AddItems(names)
		if list.Count() > 0 {
			list.SetCurrentRow(0)
		}
	}
	fill()
	filter.OnTextChanged(func(text string) {
		text = strings.ToLower(text)
		first := -1
		for i := 0; i < list.Count(); i++ {
			hidden := !strings.Contains(strings.ToLower(list.Item(i).Text()), text)
			list.Item(i).SetHidden(hidden)
			if !hidden && first < 0 {
				first = i
			}
		}
		if first >= 0 {
			list.SetCurrentRow(first)
		}
	})

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Open | qt.QDialogButtonBox__Cancel)
	del := buttons.AddButton2("Delete", qt.QDialogButtonBox__DestructiveRole)
	layout.AddWidget(buttons.QWidget)
	selected := ""
	accept := func() {
		if item := list.CurrentItem(); item != nil && !item.IsHidden() {
			selected = item.Text()
			dlg.Accept()
		}
	}
	buttons.OnAccepted(accept)
	buttons.OnRejected(dlg.Reject)
	list.OnItemDoubleClicked(func(*qt.QListWidgetItem) { accept() })
	del.OnClicked(func() {
		item := list.CurrentItem()
		if item == nil {
			return
		}
		name := item.Text()
		if qt.QMessageBox_Question(dlg.QWidget, "Delete Query", fmt.Sprintf("Delete the saved query %q?", name)) != qt.QMessageBox__Yes {
			return
		}
		if err := store.DeleteQuery(name); err != nil {
			qt.QMessageBox_Warning(dlg.QWidget, "Delete Query", err.Error())
		}
		fill()
	})
	if list.Count() == 0 {
		list.AddItem("(no saved queries)")
		list.Item(0).SetFlags(qt.NoItemFlags)
		buttons.Button(qt.QDialogButtonBox__Open).SetEnabled(false)
		del.SetEnabled(false)
	}
	filter.SetFocus()
	dlg.Exec()
	return selected, selected != ""
}
