# Password storage spec

This document describes where Dibi keeps the passwords of server connections, and the encrypted vault.

## Why not the OS keychain

The OS keychain is not used. Linux has no keychain every desktop provides: the Secret Service API (GNOME Keyring, KWallet, KeePassXC) is missing on minimal window managers, over SSH and on WSL. Dibi's own vault works the same everywhere. On Linux it is no weaker than the keychain: the Secret Service lets any process of the user read an unlocked collection.

Storing passwords encrypted with a key Dibi knows (hardcoded, or derived from the machine) is not an option either: anyone with the files could decrypt them as Dibi does. Real protection needs a secret that is not on disk, typed by the user.

## Connection dialog

Below the Password field, a **Password storage** dropdown (server connections only):

| Option                         | Value in the YAML (`password_storage`) | Where the password is                          |
|--------------------------------|----------------------------------------|------------------------------------------------|
| Plain text (default)           | (absent)                               | in the connection file, unencrypted            |
| Never store                    | `ask`                                  | nowhere: asked when connecting                 |
| Encrypted with master password | `vault`                                | in the vault, encrypted                        |

Only *Plain text* writes `password:` to the connection file. Existing connections have no `password_storage` and stay *Plain text*.

- *Never store*: the password field shows "not saved: asked when connecting". A password typed there is used by Test Connection and kept for the session on Save.
- *Encrypted with master password*: saving needs the vault unlocked (or created, the first time); if the user cancels, the connection is not saved. A **Change Master Password…** button is shown next to the dropdown once the vault exists.
- Editing a vault connection unlocks the vault first, so the dialog shows the password; cancelling the unlock cancels the edit.
- Switching away from the vault, renaming or deleting a connection removes its password from the vault.

## Connecting

A connection that needs a password not in its file is resolved when its data source is first created: when the connection is expanded in the tree, or chosen in a SQL tab, or a script runs on it. SQL Server connections without a user (integrated authentication) need none.

- *Never store*: a "Password for <connection>" dialog asks it (showing `user@host:port`). It is kept in memory until Dibi is closed. **Disconnect** forgets it, so a mistyped password can be entered again.
- *Encrypted with master password*: the vault is unlocked if needed, then the password is read from it. If the vault has none (as after a reset), it is asked like *Never store* and saved in the vault.
- Cancelling a prompt cancels the operation: the tree node collapses, the SQL tab is left without a connection.

## Vault

### Dialogs

- **Create Password Vault**, the first time a password goes to the vault: master password and confirmation (at least 8 characters), with a warning that a forgotten master password cannot be recovered.
- **Unlock Password Vault**, once per session: the master password. A wrong one is reported in the dialog, which stays open. **Reset Vault…** (after a confirmation) deletes the vault and every password in it, keeping the connections, then creates a new vault.
- **Change Master Password**: current password, new password and confirmation. The saved passwords are kept.

### File format

`vault.json` in the config directory, written atomically (temporary file + rename):

```json
{
  "version": 1,
  "kdf": {"name": "argon2id", "time": 3, "memory_kib": 65536, "threads": 4, "salt": "<16 bytes>"},
  "key": "<data key sealed with the master key>",
  "check": "<empty message sealed with the data key>",
  "passwords": {"<connection name>": "<password sealed with the data key>"}
}
```

Binary fields are base64. "Sealed" means XChaCha20-Poly1305 with a random 24-byte nonce, stored as nonce + ciphertext.

- A random 256-bit **data key** encrypts the passwords. The **master key**, derived from the master password with Argon2id and the salt, encrypts the data key (envelope encryption): changing the master password only re-encrypts the data key, with a new salt.
- A wrong master password fails the authentication of the data key; no password hash is stored.
- Each password is authenticated with the connection name as additional data, so entries cannot be swapped between connections.
- `check` tells whether a data key in memory still belongs to the file: if another Dibi instance reset the vault and created a new one, the stale key is dropped and the vault asks to be unlocked again.
- Entries can be deleted while the vault is locked; reading and writing them needs the data key.

### Limits

- While the vault is unlocked, the data key and the passwords in use are in memory; malware running as the user can read them, as with any password manager. Go strings cannot be reliably wiped.
- Connection names and the other connection parameters are not encrypted.
- SSL client keys and Oracle wallets are files referenced by path, and are not encrypted by Dibi.
