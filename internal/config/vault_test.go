package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVault(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := s.Vault()
	if v.Exists() || v.Unlocked() {
		t.Fatal("a new store must have no vault")
	}
	if err := v.SetPassword("pg", "secret"); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("SetPassword on a missing vault: %v", err)
	}
	if err := v.Create("short"); err == nil {
		t.Fatal("expected an error for a short master password")
	}
	if err := v.Create("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := v.Create("correct horse"); err == nil {
		t.Fatal("expected an error creating the vault twice")
	}
	if err := v.SetPassword("pg", "secret"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Root, "vault.json"))
	if strings.Contains(string(data), "secret") {
		t.Fatal("the vault file contains the password in plaintext")
	}

	v.Lock()
	if _, _, err := v.Password("pg"); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("Password on a locked vault: %v", err)
	}
	if err := v.Unlock("wrong password"); !errors.Is(err, ErrWrongMasterPassword) {
		t.Fatalf("Unlock with a wrong password: %v", err)
	}
	if err := v.Unlock("correct horse"); err != nil {
		t.Fatal(err)
	}
	if pw, ok, err := v.Password("pg"); err != nil || !ok || pw != "secret" {
		t.Fatalf("Password = %q, %v, %v", pw, ok, err)
	}
	if _, ok, err := v.Password("other"); err != nil || ok {
		t.Fatalf("Password of a missing entry = %v, %v", ok, err)
	}

	// Changing the master password keeps the passwords.
	if err := v.ChangeMaster("wrong password", "new master"); !errors.Is(err, ErrWrongMasterPassword) {
		t.Fatalf("ChangeMaster with a wrong password: %v", err)
	}
	if err := v.ChangeMaster("correct horse", "new master"); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	if err := v.Unlock("correct horse"); !errors.Is(err, ErrWrongMasterPassword) {
		t.Fatal("the old master password must no longer work")
	}
	if err := v.Unlock("new master"); err != nil {
		t.Fatal(err)
	}
	if pw, _, _ := v.Password("pg"); pw != "secret" {
		t.Fatalf("password after ChangeMaster = %q", pw)
	}

	// Entries are bound to their name: a swapped entry does not decrypt.
	if err := v.SetPassword("other", "x"); err != nil {
		t.Fatal(err)
	}
	f, _ := v.read()
	f.Passwords["pg"], f.Passwords["other"] = f.Passwords["other"], f.Passwords["pg"]
	v.write(f)
	if _, _, err := v.Password("pg"); err == nil {
		t.Fatal("a swapped entry must not decrypt")
	}

	// Delete works while locked.
	v.Lock()
	if err := v.Delete("pg"); err != nil {
		t.Fatal(err)
	}
	v.Unlock("new master")
	if _, ok, _ := v.Password("pg"); ok {
		t.Fatal("Delete did not remove the entry")
	}

	// A vault replaced by another instance locks the stale key.
	other := &Vault{path: v.path}
	if err := other.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := other.Create("another master"); err != nil {
		t.Fatal(err)
	}
	if err := v.SetPassword("pg", "secret"); !errors.Is(err, ErrVaultLocked) || v.Unlocked() {
		t.Fatalf("SetPassword with a stale key: %v", err)
	}

	if err := v.Reset(); err != nil || v.Exists() {
		t.Fatalf("Reset: %v", err)
	}
}

func TestPasswordStorage(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(s.Root, "connections", name+".yaml"))
		return string(data)
	}
	pg := Connection{Name: "pg", Type: Postgres, Host: "db", Port: 5432, User: "u", Password: "secret", PasswordStorage: PasswordAsk}
	if err := s.SaveConnection(pg, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read("pg"), "secret") {
		t.Fatal("an asked password must not be written")
	}
	conns, _ := s.Connections()
	if conns[0].Password != "" || conns[0].PasswordStorage != PasswordAsk || !conns[0].NeedsPassword() {
		t.Fatalf("unexpected connection: %+v", conns[0])
	}

	// The vault must be unlocked to save a password in it.
	pg.PasswordStorage = PasswordVault
	if err := s.SaveConnection(pg, "pg"); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("saving to a locked vault: %v", err)
	}
	v := s.Vault()
	if err := v.Create("master password"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConnection(pg, "pg"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read("pg"), "secret") {
		t.Fatal("a vault password must not be written to the connection file")
	}
	if pw, _, _ := v.Password("pg"); pw != "secret" {
		t.Fatalf("vault password = %q", pw)
	}

	// A rename moves the password.
	pg2 := pg
	pg2.Name = "pg2"
	if err := s.SaveConnection(pg2, "pg"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := v.Password("pg"); ok {
		t.Fatal("the old name must leave the vault")
	}
	if pw, _, _ := v.Password("pg2"); pw != "secret" {
		t.Fatalf("renamed vault password = %q", pw)
	}

	// Back to plain text: the password returns to the file and leaves the vault.
	pg2.PasswordStorage = PasswordPlain
	if err := s.SaveConnection(pg2, "pg2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read("pg2"), "secret") {
		t.Fatal("a plain password must be written")
	}
	if _, ok, _ := v.Password("pg2"); ok {
		t.Fatal("a plain password must leave the vault")
	}

	// Deleting a connection deletes its vault password.
	pg2.PasswordStorage = PasswordVault
	s.SaveConnection(pg2, "pg2")
	if err := s.DeleteConnection("pg2"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := v.Password("pg2"); ok {
		t.Fatal("DeleteConnection must remove the vault password")
	}

	// Integrated authentication on SQL Server needs no password.
	ms := Connection{Name: "ms", Type: MSSQL, Host: "db", Port: 1433, PasswordStorage: PasswordAsk}
	if ms.NeedsPassword() {
		t.Fatal("SQL Server without a user needs no password")
	}
}
