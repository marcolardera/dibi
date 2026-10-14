package ui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dibi/internal/config"
	"dibi/internal/db"
	"dibi/internal/export"
	"dibi/internal/importer"

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
	pwStorage := qt.NewQComboBox2()
	for _, p := range config.PasswordStorages {
		pwStorage.AddItem3(p.Label(), qt.NewQVariant14(string(p)))
	}
	pwStorage.SetToolTip("Plain text: saved unencrypted in the connection file.\n" +
		"Never store: asked when connecting, and kept in memory until Dibi is closed.\n" +
		"Encrypted with master password: saved in a vault encrypted with a master password,\n" +
		"which Dibi asks once per session.")
	changeMaster := qt.NewQPushButton3("Change Master Password…")
	changeMaster.OnClicked(func() { changeMasterDialog(dlg.QWidget, store.Vault()) })
	storageRow := qt.NewQWidget2()
	storageRow.SetLayout(hbox(pwStorage.QWidget, changeMaster.QWidget).QLayout)
	sslMode := qt.NewQComboBox2()
	sslMode.SetToolTip("Disable: never use TLS.\n" +
		"Prefer: use TLS if the server supports it, without checking its certificate.\n" +
		"Require: always use TLS, without checking the certificate (accepts a self-signed one).\n" +
		"Verify CA: also check that the certificate is signed by a trusted CA.\n" +
		"Verify full: also check that the certificate names the host.")
	rootCert, rootCertRow := fileField(dlg.QWidget, "Choose CA Certificate", "Certificates (*.pem *.crt *.cer);;All files (*)")
	rootCert.SetPlaceholderText("system CAs")
	clientCert, clientCertRow := fileField(dlg.QWidget, "Choose Client Certificate", "Certificates (*.pem *.crt *.cer);;All files (*)")
	clientCert.SetPlaceholderText("none")
	clientKey, clientKeyRow := fileField(dlg.QWidget, "Choose Client Key", "Keys (*.pem *.key);;All files (*)")
	clientKey.SetPlaceholderText("none")
	wallet, walletRow := fileField(dlg.QWidget, "Choose Wallet Folder", "")
	wallet.SetPlaceholderText("folder with cwallet.sso")
	path := qt.NewQLineEdit2()
	browse := qt.NewQPushButton3("Browse…")
	// CSV and Parquet connections can also point to a folder of files.
	browseDir := qt.NewQPushButton3("Folder…")
	pathRow := qt.NewQWidget2()
	pathLayout := hbox(path.QWidget, browse.QWidget, browseDir.QWidget)
	pathRow.SetLayout(pathLayout.QLayout)

	// CSV options; the empty value means detected from the file.
	separator := qt.NewQComboBox2()
	for _, o := range [][2]string{{"Auto-detect", ""}, {"Comma ( , )", ","}, {"Semicolon ( ; )", ";"}, {"Tab", "\t"}, {"Pipe ( | )", "|"}} {
		separator.AddItem3(o[0], qt.NewQVariant14(o[1]))
	}
	quote := qt.NewQComboBox2()
	for _, o := range [][2]string{{"Auto-detect", ""}, {`Double quote ( " )`, `"`}, {"Single quote ( ' )", "'"}} {
		quote.AddItem3(o[0], qt.NewQVariant14(o[1]))
	}

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
	storageLabel := qt.NewQLabel3("Password storage:")
	form.AddRow(storageLabel.QWidget, storageRow)
	sslLabel, rootCertLabel, clientCertLabel, clientKeyLabel, walletLabel :=
		qt.NewQLabel3("SSL mode:"), qt.NewQLabel3("CA certificate:"), qt.NewQLabel3("Client certificate:"),
		qt.NewQLabel3("Client key:"), qt.NewQLabel3("Wallet:")
	form.AddRow(sslLabel.QWidget, sslMode.QWidget)
	form.AddRow(rootCertLabel.QWidget, rootCertRow)
	form.AddRow(clientCertLabel.QWidget, clientCertRow)
	form.AddRow(clientKeyLabel.QWidget, clientKeyRow)
	form.AddRow(walletLabel.QWidget, walletRow)
	form.AddRow(pathLabel.QWidget, pathRow)
	sepLabel, quoteLabel := qt.NewQLabel3("Separator:"), qt.NewQLabel3("Quote character:")
	form.AddRow(sepLabel.QWidget, separator.QWidget)
	form.AddRow(quoteLabel.QWidget, quote.QWidget)

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
	currentSSL := func() config.SSLMode { return config.SSLMode(sslMode.CurrentData().ToString()) }
	currentStorage := func() config.PasswordStorage {
		return config.PasswordStorage(pwStorage.CurrentData().ToString())
	}
	updateStorage := func() {
		switch currentStorage() {
		case config.PasswordAsk:
			password.SetPlaceholderText("not saved: asked when connecting")
		default:
			password.SetPlaceholderText("")
		}
		changeMaster.SetVisible(currentStorage() == config.PasswordVault && store.Vault().Exists())
	}
	pwStorage.OnActivated(func(int) { updateStorage() })
	// updateSSL shows the TLS fields that apply to the type and SSL mode.
	updateSSL := func() {
		t, mode := currentType(), currentSSL()
		server, useTLS := !t.IsFile(), !t.IsFile() && mode != config.SSLDisable
		sslLabel.SetVisible(server)
		sslMode.SetVisible(server)
		for _, w := range []*qt.QWidget{rootCertLabel.QWidget, rootCertRow} {
			w.SetVisible(useTLS && mode.Verifies())
		}
		// SQL Server has no client certificate authentication.
		for _, w := range []*qt.QWidget{clientCertLabel.QWidget, clientCertRow, clientKeyLabel.QWidget, clientKeyRow} {
			w.SetVisible(useTLS && t != config.MSSQL)
		}
		walletLabel.SetVisible(useTLS && t == config.Oracle)
		walletRow.SetVisible(useTLS && t == config.Oracle)
		dlg.AdjustSize()
	}
	// setSSLModes lists the modes of the type and selects want, or the default
	// if the type does not support it.
	setSSLModes := func(want config.SSLMode) {
		sslMode.Clear()
		for _, m := range currentType().SSLModes() {
			sslMode.AddItem3(m.Label(), qt.NewQVariant14(string(m)))
		}
		sslMode.SetCurrentIndex(max(sslMode.FindData(qt.NewQVariant14(string(want))), 0))
	}
	sslMode.OnActivated(func(int) { updateSSL() })
	serverRows := []*qt.QWidget{hostLabel.QWidget, host.QWidget, portLabel.QWidget, port.QWidget,
		dbLabel.QWidget, database.QWidget, userLabel.QWidget, user.QWidget, pwLabel.QWidget, password.QWidget,
		storageLabel.QWidget, storageRow}
	fileRows := []*qt.QWidget{pathLabel.QWidget, pathRow}
	updateFields := func(resetPort bool) {
		t := currentType()
		for _, w := range serverRows {
			w.SetVisible(!t.IsFile())
		}
		for _, w := range fileRows {
			w.SetVisible(t.IsFile())
		}
		for _, w := range []*qt.QWidget{sepLabel.QWidget, separator.QWidget, quoteLabel.QWidget, quote.QWidget} {
			w.SetVisible(t == config.CSV)
		}
		folders := t == config.CSV || t == config.Parquet
		browseDir.SetVisible(folders)
		if folders {
			pathLabel.SetText("File or folder:")
			browse.SetText("File…")
		} else {
			pathLabel.SetText("File:")
			browse.SetText("Browse…")
		}
		// Opening a local file checks nothing a missing path would not;
		// Save checks the path instead.
		testBtn.SetVisible(!t.IsFile())
		status.Clear()
		if resetPort {
			setSSLModes("")
		} else {
			setSSLModes(currentSSL())
		}
		updateSSL()
		if t == config.Oracle {
			dbLabel.SetText("Service name:")
		} else {
			dbLabel.SetText("Database:")
		}
		if t == config.SQLite || t == config.DuckDB {
			path.SetPlaceholderText(config.MemoryPath + " for an in-memory database")
		} else {
			path.SetPlaceholderText("")
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
			filter = "CSV files (*.csv)"
		case config.Parquet:
			filter = "Parquet files (*.parquet);;All files (*)"
		}
		dir := path.Text()
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			dir = filepath.Dir(dir)
		}
		if f := qt.QFileDialog_GetOpenFileName4(dlg.QWidget, "Choose File", dir, filter); f != "" {
			path.SetText(f)
			if name.Text() == "" {
				name.SetText(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)))
			}
		}
	})
	browseDir.OnClicked(func() {
		dir := path.Text()
		if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
			dir = filepath.Dir(dir)
		}
		if f := qt.QFileDialog_GetExistingDirectory3(dlg.QWidget, "Choose Folder", dir); f != "" {
			path.SetText(f)
			if name.Text() == "" {
				name.SetText(filepath.Base(f))
			}
		}
	})

	collect := func() config.Connection {
		c := config.Connection{Name: strings.TrimSpace(name.Text()), Type: currentType()}
		if c.Type.IsFile() {
			c.Path = strings.TrimSpace(path.Text())
			if c.Type == config.CSV {
				c.Separator = separator.CurrentData().ToString()
				c.Quote = quote.CurrentData().ToString()
			}
		} else {
			c.Host = strings.TrimSpace(host.Text())
			c.Port = port.Value()
			c.Database = strings.TrimSpace(database.Text())
			c.User = strings.TrimSpace(user.Text())
			c.Password = password.Text()
			c.PasswordStorage = currentStorage()
			c.SSLMode = currentSSL()
			if c.SSLMode != config.SSLDisable {
				if c.SSLMode.Verifies() {
					c.SSLRootCert = strings.TrimSpace(rootCert.Text())
				}
				if c.Type != config.MSSQL {
					c.SSLCert, c.SSLKey = strings.TrimSpace(clientCert.Text()), strings.TrimSpace(clientKey.Text())
				}
				if c.Type == config.Oracle {
					c.Wallet = strings.TrimSpace(wallet.Text())
				}
			}
		}
		return c
	}
	showStatus := func(ok bool, msg string) {
		if ok {
			setTextColor(status, th.okText)
		} else {
			setTextColor(status, th.errText)
		}
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
		if c.Type.IsFile() && c.Path != "" && !c.InMemory() {
			fi, err := os.Stat(c.Path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				showStatus(false, c.Path+" does not exist.")
				return
			case err != nil:
				showStatus(false, err.Error())
				return
			case fi.IsDir() && c.Type != config.CSV && c.Type != config.Parquet:
				showStatus(false, c.Path+" is a folder; choose a "+c.Type.Label()+" database file.")
				return
			case !fi.IsDir() && c.Type == config.CSV && !strings.EqualFold(filepath.Ext(c.Path), db.DataExtension(c.Type)):
				showStatus(false, "Choose a .csv file or a folder.")
				return
			}
		}
		if c.PasswordStorage == config.PasswordVault && !unlockVault(dlg.QWidget, store.Vault()) {
			showStatus(false, "Unlock the password vault to save the password in it.")
			return
		}
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
		pwStorage.SetCurrentIndex(max(pwStorage.FindData(qt.NewQVariant14(string(existing.PasswordStorage))), 0))
		rootCert.SetText(existing.SSLRootCert)
		clientCert.SetText(existing.SSLCert)
		clientKey.SetText(existing.SSLKey)
		wallet.SetText(existing.Wallet)
		path.SetText(existing.Path)
		separator.SetCurrentIndex(max(separator.FindData(qt.NewQVariant14(existing.Separator)), 0))
		quote.SetCurrentIndex(max(quote.FindData(qt.NewQVariant14(existing.Quote)), 0))
		setSSLModes(existing.TLSMode())
		updateFields(false)
	} else {
		host.SetText("localhost")
		updateFields(true)
	}
	updateStorage()
	name.SetFocus()
	dlg.Exec()
	return result, saved
}

// fileField returns a line edit for a path with a Browse button, and the row
// holding both. An empty filter chooses a folder.
func fileField(parent *qt.QWidget, caption, filter string) (*qt.QLineEdit, *qt.QWidget) {
	edit := qt.NewQLineEdit2()
	browse := qt.NewQPushButton3("Browse…")
	browse.OnClicked(func() {
		dir := filepath.Dir(edit.Text())
		var f string
		if filter == "" {
			f = qt.QFileDialog_GetExistingDirectory3(parent, caption, edit.Text())
		} else {
			f = qt.QFileDialog_GetOpenFileName4(parent, caption, dir, filter)
		}
		if f != "" {
			edit.SetText(f)
		}
	})
	row := qt.NewQWidget2()
	row.SetLayout(hbox(edit.QWidget, browse.QWidget).QLayout)
	return edit, row
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

// importDialog asks for the options of a table import into target (the
// name of a schema or database). It returns false if cancelled.
func importDialog(parent *qt.QWidget, target string) (importer.Options, bool) {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("Import Table")
	dlg.SetMinimumWidth(480)
	layout := qt.NewQVBoxLayout(dlg.QWidget)

	noteLabel := qt.NewQLabel3(fmt.Sprintf("Creates a table in %s and loads the file into it. "+
		"Column types are inferred from the data.", target))
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

	path := qt.NewQLineEdit2()
	browse := qt.NewQPushButton3("Browse…")
	pathRow := qt.NewQWidget2()
	pathRow.SetLayout(hbox(path.QWidget, browse.QWidget).QLayout)
	form.AddRow3("File:", pathRow)

	header := qt.NewQCheckBox3("The first row holds the column names")
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
	nullText.SetToolTip("Values equal to this text are imported as NULL.\n" +
		"Empty values are NULL in columns that are not text.")
	sepLabel, nullLabel := qt.NewQLabel3("Separator:"), qt.NewQLabel3("NULL as:")
	form.AddRow(sepLabel.QWidget, sepCombo.QWidget)
	form.AddRow(nullLabel.QWidget, nullText.QWidget)

	sheet := qt.NewQLineEdit2()
	sheet.SetPlaceholderText("first sheet")
	sheetLabel := qt.NewQLabel3("Sheet name:")
	form.AddRow(sheetLabel.QWidget, sheet.QWidget)

	table := qt.NewQLineEdit2()
	table.SetPlaceholderText("name of the new table")
	form.AddRow3("Destination table:", table.QWidget)
	// The table is named after the file, until the user names it.
	tableEdited := false
	table.OnTextEdited(func(string) { tableEdited = true })

	isXLSX := func() bool { return xlsxRadio.IsChecked() }
	update := func() {
		for _, w := range []*qt.QWidget{sepLabel.QWidget, sepCombo.QWidget, nullLabel.QWidget, nullText.QWidget} {
			w.SetVisible(!isXLSX())
		}
		sheetLabel.SetVisible(isXLSX())
		sheet.SetVisible(isXLSX())
	}
	csvRadio.OnToggled(func(bool) { update() })
	path.OnTextChanged(func(p string) {
		switch strings.ToLower(filepath.Ext(strings.TrimSpace(p))) {
		case ".csv", ".tsv", ".txt":
			csvRadio.SetChecked(true)
		case ".xlsx", ".xlsm":
			xlsxRadio.SetChecked(true)
		}
		if strings.EqualFold(filepath.Ext(p), ".tsv") {
			sepCombo.SetCurrentIndex(2)
		}
		if !tableEdited && strings.TrimSpace(p) != "" {
			table.SetText(db.FileTableName(strings.TrimSpace(p)))
		}
	})
	browse.OnClicked(func() {
		dir := filepath.Dir(path.Text())
		if path.Text() == "" {
			dir, _ = os.UserHomeDir()
		}
		filter := "CSV files (*.csv *.tsv *.txt);;Excel files (*.xlsx *.xlsm);;All files (*)"
		if isXLSX() {
			filter = "Excel files (*.xlsx *.xlsm);;CSV files (*.csv *.tsv *.txt);;All files (*)"
		}
		if f := qt.QFileDialog_GetOpenFileName4(dlg.QWidget, "Import From", dir, filter); f != "" {
			path.SetText(f)
		}
	})

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
	buttons.Button(qt.QDialogButtonBox__Ok).SetText("Import")
	layout.AddWidget(buttons.QWidget)
	var opts importer.Options
	ok := false
	buttons.OnAccepted(func() {
		p := strings.TrimSpace(path.Text())
		if p == "" {
			qt.QMessageBox_Warning(dlg.QWidget, "Import", "Choose a file to import.")
			return
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			qt.QMessageBox_Warning(dlg.QWidget, "Import", fmt.Sprintf("%s is not a readable file.", p))
			return
		}
		name := strings.TrimSpace(table.Text())
		if name == "" {
			qt.QMessageBox_Warning(dlg.QWidget, "Import", "Enter the name of the table to create.")
			return
		}
		opts = importer.Options{Path: p, Header: header.IsChecked(), Table: name}
		if isXLSX() {
			opts.Format = importer.XLSX
			opts.SheetName = strings.TrimSpace(sheet.Text())
		} else {
			opts.Format = importer.CSV
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
