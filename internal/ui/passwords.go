package ui

import (
	"errors"
	"fmt"
	"strings"

	"dibi/internal/config"

	qt "github.com/mappu/miqt/qt6"
)

// resolvePassword fills in the password of a connection that does not keep
// it in its file: from the session, or from the vault (unlocking it), or by
// asking the user. It returns false if the user cancelled.
func (a *App) resolvePassword(c *config.Connection) bool {
	if !c.NeedsPassword() {
		return true
	}
	switch c.PasswordStorage {
	case config.PasswordAsk:
		if pw, ok := a.passwords[c.Name]; ok {
			c.Password = pw
			return true
		}
		pw, ok := passwordDialog(a.win.QWidget, c)
		if !ok {
			return false
		}
		a.passwords[c.Name] = pw
		c.Password = pw
	case config.PasswordVault:
		v := a.store.Vault()
		if !unlockVault(a.win.QWidget, v) {
			return false
		}
		pw, ok, err := v.Password(c.Name)
		if err != nil {
			qt.QMessageBox_Warning(a.win.QWidget, "Password Vault", err.Error())
			return false
		}
		if !ok {
			// No saved password, as after a vault reset: ask it and save it.
			if pw, ok = passwordDialog(a.win.QWidget, c); !ok {
				return false
			}
			if err := v.SetPassword(c.Name, pw); err != nil {
				qt.QMessageBox_Warning(a.win.QWidget, "Password Vault", "The password was not saved: "+err.Error())
			}
		}
		c.Password = pw
	}
	return true
}

// passwordDialog asks the password of a connection.
func passwordDialog(parent *qt.QWidget, c *config.Connection) (string, bool) {
	who := c.User
	if c.Host != "" {
		who += fmt.Sprintf("@%s:%d", c.Host, c.Port)
	}
	edit := qt.NewQLineEdit2()
	edit.SetEchoMode(qt.QLineEdit__Password)
	ok := passwordForm(parent, "Password for "+c.Name,
		fmt.Sprintf("Enter the password of %s.", who), "Connect",
		[]string{"Password:"}, []*qt.QLineEdit{edit}, nil, nil)
	return edit.Text(), ok
}

// unlockVault makes sure the vault is unlocked, asking for the master
// password, or for a new one if the vault does not exist yet. It returns
// false if the user cancelled.
func unlockVault(parent *qt.QWidget, v *config.Vault) bool {
	if v.Unlocked() {
		return true
	}
	if !v.Exists() {
		master, confirm := newPasswordEdit(), newPasswordEdit()
		return passwordForm(parent, "Create Password Vault",
			"Choose the master password that encrypts the saved passwords. "+
				"Dibi asks for it once per session.\n\n"+
				"If you forget it, the saved passwords cannot be recovered: "+
				"you will have to reset the vault and enter them again.",
			"Create", []string{"Master password:", "Confirm:"}, []*qt.QLineEdit{master, confirm},
			func() error {
				if master.Text() != confirm.Text() {
					return errors.New("the passwords do not match")
				}
				return v.Create(master.Text())
			}, nil)
	}
	master := newPasswordEdit()
	reset := false
	ok := passwordForm(parent, "Unlock Password Vault",
		"Enter the master password to use the saved passwords.", "Unlock",
		[]string{"Master password:"}, []*qt.QLineEdit{master},
		func() error {
			if err := v.Unlock(master.Text()); err != nil {
				master.SelectAll()
				return err
			}
			return nil
		},
		&extraButton{"Reset Vault…", func(dlg *qt.QDialog) {
			if qt.QMessageBox_Warning2(dlg.QWidget, "Reset Password Vault",
				"Delete the vault and every password saved in it?\n\n"+
					"The connections are kept: Dibi asks for their passwords the next time "+
					"they connect, and saves them in a new vault.",
				qt.QMessageBox__Reset, qt.QMessageBox__Cancel) != int(qt.QMessageBox__Reset) {
				return
			}
			if err := v.Reset(); err != nil {
				qt.QMessageBox_Warning(dlg.QWidget, "Reset Password Vault", err.Error())
				return
			}
			reset = true
			dlg.Reject()
		}})
	if reset {
		// Go on with a new vault.
		return unlockVault(parent, v)
	}
	return ok
}

// changeMasterDialog changes the master password of the vault.
func changeMasterDialog(parent *qt.QWidget, v *config.Vault) {
	current, master, confirm := newPasswordEdit(), newPasswordEdit(), newPasswordEdit()
	if passwordForm(parent, "Change Master Password",
		"The saved passwords are kept.", "Change",
		[]string{"Current password:", "New password:", "Confirm:"}, []*qt.QLineEdit{current, master, confirm},
		func() error {
			if master.Text() != confirm.Text() {
				return errors.New("the new passwords do not match")
			}
			return v.ChangeMaster(current.Text(), master.Text())
		}, nil) {
		qt.QMessageBox_Information(parent, "Change Master Password", "The master password was changed.")
	}
}

func newPasswordEdit() *qt.QLineEdit {
	e := qt.NewQLineEdit2()
	e.SetEchoMode(qt.QLineEdit__Password)
	return e
}

// extraButton is an additional button of a passwordForm.
type extraButton struct {
	text    string
	clicked func(dlg *qt.QDialog)
}

// passwordForm shows a dialog with a message and password fields. On OK,
// accept (if set) checks the input: an error is shown in the dialog, which
// stays open. It returns true if the dialog was accepted.
func passwordForm(parent *qt.QWidget, title, message, okText string, labels []string,
	edits []*qt.QLineEdit, accept func() error, extra *extraButton) bool {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle(title)
	dlg.SetMinimumWidth(420)
	layout := qt.NewQVBoxLayout(dlg.QWidget)
	msg := qt.NewQLabel3(message)
	msg.SetWordWrap(true)
	layout.AddWidget(msg.QWidget)
	form := qt.NewQFormLayout2()
	for i, e := range edits {
		form.AddRow3(labels[i], e.QWidget)
	}
	layout.AddLayout(form.QLayout)
	status := qt.NewQLabel2()
	status.SetWordWrap(true)
	setTextColor(status, th.errText)
	status.SetVisible(false)
	layout.AddWidget(status.QWidget)

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
	buttons.Button(qt.QDialogButtonBox__Ok).SetText(okText)
	if extra != nil {
		buttons.AddButton2(extra.text, qt.QDialogButtonBox__ResetRole).OnClicked(func() { extra.clicked(dlg) })
	}
	layout.AddWidget(buttons.QWidget)
	ok := false
	buttons.OnAccepted(func() {
		if accept != nil {
			if err := accept(); err != nil {
				msg := err.Error()
				if errors.Is(err, config.ErrWrongMasterPassword) {
					msg = "Wrong master password."
				} else {
					msg = strings.ToUpper(msg[:1]) + msg[1:] + "."
				}
				status.SetText(msg)
				status.SetVisible(true)
				return
			}
		}
		ok = true
		dlg.Accept()
	})
	buttons.OnRejected(dlg.Reject)
	edits[0].SetFocus()
	// Make room for the wrapped message, which the size hint ignores.
	dlg.SetMinimumHeight(layout.TotalHeightForWidth(dlg.MinimumWidth()))
	dlg.Exec()
	return ok
}
