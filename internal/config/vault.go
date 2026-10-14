package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Vault keeps connection passwords encrypted under a master password, in a
// single file next to the connections.
//
// A random data key encrypts each password (XChaCha20-Poly1305, bound to the
// connection name so that entries cannot be swapped). The data key is
// itself encrypted with a key derived from the master password (Argon2id),
// so changing the master password only re-encrypts the data key. A wrong
// master password is detected by the authentication of the data key.
//
// The vault is locked until Unlock (or Create) is called; the data key then
// stays in memory until Lock or Reset. Every operation reads the file again,
// so concurrent Dibi instances do not lose each other's changes.
type Vault struct {
	path string
	key  []byte // the data key; nil while locked
}

// ErrVaultLocked is returned by operations that need the vault unlocked.
var ErrVaultLocked = errors.New("the password vault is locked")

// ErrWrongMasterPassword is returned when the master password does not open the vault.
var ErrWrongMasterPassword = errors.New("wrong master password")

// MinMasterPassword is the minimum length of a master password, in characters.
const MinMasterPassword = 8

const (
	vaultVersion   = 1
	vaultKeyAD     = "dibi vault key"
	vaultEntryAD   = "dibi vault password\x00"
	vaultCheckAD   = "dibi vault check"
	argonTime      = 3
	argonMemoryKiB = 64 * 1024
	argonThreads   = 4
)

type vaultKDF struct {
	Name      string `json:"name"` // "argon2id"
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
	Salt      []byte `json:"salt"`
}

type vaultFile struct {
	Version int      `json:"version"`
	KDF     vaultKDF `json:"kdf"`
	// Key is the data key sealed with the master key (nonce + ciphertext).
	Key []byte `json:"key"`
	// Check is an empty message sealed with the data key: it tells whether a
	// data key in memory still belongs to this file.
	Check []byte `json:"check"`
	// Passwords maps a connection name to its sealed password.
	Passwords map[string][]byte `json:"passwords"`
}

// Vault returns the password vault of the store.
func (s *Store) Vault() *Vault {
	if s.vault == nil {
		s.vault = &Vault{path: filepath.Join(s.Root, "vault.json")}
	}
	return s.vault
}

// Exists reports whether the vault file has been created.
func (v *Vault) Exists() bool {
	_, err := os.Stat(v.path)
	return err == nil
}

// Unlocked reports whether the data key is in memory.
func (v *Vault) Unlocked() bool { return v.key != nil }

// Create makes a new, empty vault protected by master and unlocks it.
func (v *Vault) Create(master string) error {
	if v.Exists() {
		return errors.New("the password vault already exists")
	}
	if len([]rune(master)) < MinMasterPassword {
		return fmt.Errorf("the master password must have at least %d characters", MinMasterPassword)
	}
	key := make([]byte, chacha20poly1305.KeySize)
	rand.Read(key)
	f := &vaultFile{Version: vaultVersion, Check: seal(key, nil, vaultCheckAD), Passwords: map[string][]byte{}}
	f.wrapKey(key, master)
	if err := v.write(f); err != nil {
		return err
	}
	v.key = key
	return nil
}

// Unlock opens the vault with its master password.
func (v *Vault) Unlock(master string) error {
	f, err := v.read()
	if err != nil {
		return err
	}
	key, err := f.unwrapKey(master)
	if err != nil {
		return err
	}
	v.key = key
	return nil
}

// Lock forgets the data key.
func (v *Vault) Lock() {
	clear(v.key)
	v.key = nil
}

// ChangeMaster re-encrypts the data key with a new master password, after
// checking the current one. The saved passwords are untouched.
func (v *Vault) ChangeMaster(current, master string) error {
	if len([]rune(master)) < MinMasterPassword {
		return fmt.Errorf("the master password must have at least %d characters", MinMasterPassword)
	}
	f, err := v.read()
	if err != nil {
		return err
	}
	key, err := f.unwrapKey(current)
	if err != nil {
		return err
	}
	f.wrapKey(key, master)
	if err := v.write(f); err != nil {
		return err
	}
	v.key = key
	return nil
}

// Reset deletes the vault and every password in it, for when the master
// password is forgotten. The connections themselves are kept.
func (v *Vault) Reset() error {
	v.Lock()
	err := os.Remove(v.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Password returns the saved password of a connection, and false if the
// vault has none for it.
func (v *Vault) Password(name string) (string, bool, error) {
	if v.key == nil {
		return "", false, ErrVaultLocked
	}
	f, err := v.read()
	if err != nil {
		return "", false, err
	}
	if err := v.checkKey(f); err != nil {
		return "", false, err
	}
	sealed, ok := f.Passwords[name]
	if !ok {
		return "", false, nil
	}
	pw, err := open(v.key, sealed, vaultEntryAD+name)
	if err != nil {
		return "", false, fmt.Errorf("the saved password of %q cannot be decrypted", name)
	}
	return string(pw), true, nil
}

// SetPassword saves the password of a connection.
func (v *Vault) SetPassword(name, password string) error {
	if v.key == nil {
		return ErrVaultLocked
	}
	f, err := v.read()
	if err != nil {
		return err
	}
	if err := v.checkKey(f); err != nil {
		return err
	}
	f.Passwords[name] = seal(v.key, []byte(password), vaultEntryAD+name)
	return v.write(f)
}

// Delete removes the password of a connection, if any. It works while the
// vault is locked.
func (v *Vault) Delete(name string) error {
	f, err := v.read()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := f.Passwords[name]; !ok {
		return nil
	}
	delete(f.Passwords, name)
	return v.write(f)
}

func (v *Vault) read() (*vaultFile, error) {
	data, err := os.ReadFile(v.path)
	if err != nil {
		return nil, err
	}
	var f vaultFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(v.path), err)
	}
	if f.Version != vaultVersion || f.KDF.Name != "argon2id" {
		return nil, fmt.Errorf("%s: unsupported vault format", filepath.Base(v.path))
	}
	if f.Passwords == nil {
		f.Passwords = map[string][]byte{}
	}
	return &f, nil
}

// write replaces the vault file atomically, so that a crash cannot leave a
// truncated vault and lose every password.
func (v *Vault) write(f *vaultFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(v.path), ".vault-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), v.path)
}

// wrapKey seals the data key with a key derived from master and a new salt.
func (f *vaultFile) wrapKey(key []byte, master string) {
	salt := make([]byte, 16)
	rand.Read(salt)
	f.KDF = vaultKDF{Name: "argon2id", Time: argonTime, MemoryKiB: argonMemoryKiB, Threads: argonThreads, Salt: salt}
	f.Key = seal(f.KDF.derive(master), key, vaultKeyAD)
}

func (f *vaultFile) unwrapKey(master string) ([]byte, error) {
	key, err := open(f.KDF.derive(master), f.Key, vaultKeyAD)
	if err != nil {
		return nil, ErrWrongMasterPassword
	}
	return key, nil
}

// checkKey locks the vault and fails if the data key in memory does not
// belong to f, as happens when another Dibi instance reset the vault and
// created a new one.
func (v *Vault) checkKey(f *vaultFile) error {
	if _, err := open(v.key, f.Check, vaultCheckAD); err != nil {
		v.Lock()
		return fmt.Errorf("the password vault was replaced: %w", ErrVaultLocked)
	}
	return nil
}

func (k vaultKDF) derive(master string) []byte {
	return argon2.IDKey([]byte(master), k.Salt, k.Time, k.MemoryKiB, k.Threads, chacha20poly1305.KeySize)
}

// seal encrypts plain with a random nonce and returns nonce + ciphertext.
func seal(key, plain []byte, ad string) []byte {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		panic(err) // the key size is fixed
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte(ad))
}

func open(key, sealed []byte, ad string) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("sealed data too short")
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, ct, []byte(ad))
}
