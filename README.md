# Dibi

A lightweight SQL database admin tool: explore database objects, inspect
their metadata, run queries and export results. A smaller, faster
alternative to DBeaver or DataGrip.

Supported sources: **PostgreSQL**, **MySQL**, **MariaDB**, **Oracle** (no
Instant Client needed), **SQL Server**, **SQLite**, **DuckDB**, and **CSV** /
**Parquet** files or folders of them (queried through DuckDB, one view per
file, so they can be joined).

See [spec.md](spec.md) for the full specification.

## Features

- Object tree per connection, loaded lazily: databases, schemas, tables,
  views, materialized views, functions, procedures, packages, sequences,
  and indexes/triggers under their table.
  A filter box narrows it as you type.
- Information panel: columns (type, nullability, key, default, comment),
  constraints, indexes, source code of views/routines/triggers, and a
  50-row data preview. Its **Refresh** button reloads it all; refreshing a
  node in the tree also reloads the open panels of the objects under it.
- `Ctrl+C` on a table or view in the tree copies its query-ready name
  (e.g. `schema.table`, quoted where needed).
- SQL editor with syntax highlighting, line numbers, find/replace
  (`Ctrl+F`, `Ctrl+H`), auto-closed brackets and quotes, automatic
  indentation (`Tab`/`Shift+Tab` indent and outdent the selected lines),
  and comment toggling (`Ctrl+K`). **Run** (`Ctrl+Enter`) executes the
  selection, or the whole script statement by statement; each result set
  gets its own tab. Each editor tab has its own database session.
- New scripts default to the connection (and database) you used last.
- **Stop** cancels the running query on the server and keeps the session.
- Results are streamed from the cursor, 1000 rows per page.
- Copy cells with `Ctrl+C` (full values, tab-separated).
- Export to CSV or XLSX (re-runs the query and streams all rows).
- Saved queries.
- Light and dark themes (toolbar button, remembered).
- TLS for server connections (SSL modes from *disable* to *verify-full*,
  CA and client certificates, Oracle wallets).

## Building (Linux)

Requirements: Go 1.26+, a C/C++ toolchain and the Qt 6 development files.

```sh
sudo apt install qt6-base-dev qt6-base-private-dev build-essential
make build        # produces bin/dibi
```

The first build compiles the Qt bindings and takes a few minutes.

## Building (Windows)

`dibi.exe` is a single static executable (Qt, DuckDB and the C/C++ runtime
linked in) that only needs Windows 10 or later. It can be built in two ways.

**Cross-compiled on Linux**, in Docker:

```sh
make windows      # produces bin/dibi.exe
```

The first run builds the Docker image, which compiles Qt from source and
takes about 20 minutes; see [build/windows/Dockerfile](build/windows/Dockerfile)
for why.

**On Windows**, with [MSYS2](https://www.msys2.org/). In the *UCRT64* shell:

```sh
pacman -S make git mingw-w64-ucrt-x86_64-{go,gcc,pkgconf,qt6-static} \
  mingw-w64-ucrt-x86_64-{zlib,libpng,freetype,harfbuzz,pcre2,openssl,libb2,brotli,bzip2,glib2,gettext-runtime,libiconv,graphite2}
make windows-native   # produces bin/dibi.exe
```

The second line installs the static libraries that MSYS2's Qt links against.
`QT_STATIC` points to another static Qt (default `/ucrt64/qt6-static`).
Use the UCRT64 toolchain, not MINGW64: DuckDB's prebuilt library needs the
UCRT runtime.

## Configuration

Stored in the user config directory (`~/.config/dibi` on Linux):

- `connections/` — one YAML file per connection. **Passwords are stored in
  plaintext** in this version.
- `queries/` — saved queries, one `.sql` file each.
- `state.yaml` — the last used connection and the theme.

## Development

```sh
make test          # unit tests + embedded databases
make test-servers  # also PostgreSQL, MySQL, MariaDB, Oracle, SQL Server (Docker)
```

The server tests expect these containers:

```sh
docker run -d --name dibi-pg -p 5433:5432 -e POSTGRES_USER=dibi -e POSTGRES_PASSWORD=dibi -e POSTGRES_DB=dibi postgres:17
docker run -d --name dibi-mariadb -p 3307:3306 -e MARIADB_ROOT_PASSWORD=dibi -e MARIADB_DATABASE=dibi mariadb:11
docker run -d --name dibi-mysql -p 3308:3306 -e MYSQL_ROOT_PASSWORD=dibi -e MYSQL_DATABASE=dibi mysql:8.4
docker run -d --name dibi-oracle -p 1521:1521 -e ORACLE_PASSWORD=dibi -e APP_USER=dibi -e APP_USER_PASSWORD=dibi gvenzl/oracle-free:slim
docker run -d --name dibi-mssql -p 1433:1433 -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD=Dibi_pass1 mcr.microsoft.com/mssql/server:2022-latest
```

`make autopilot` builds a variant of the app that drives its own UI through
the main workflows and saves screenshots (`DIBI_SHOTS=dir`); use it with a
throwaway `XDG_CONFIG_HOME`.

### Layout

| Path | Contents |
|------|----------|
| `cmd/dibi` | entry point |
| `internal/config` | connections, saved queries, state |
| `internal/db` | data sources, sessions, metadata per database |
| `internal/sqlsplit` | dialect-aware script splitter |
| `internal/pager` | streaming, paged result sets |
| `internal/value` | value formatting |
| `internal/export` | CSV/XLSX export |
| `internal/ui` | Qt user interface |
| `spikes/` | throwaway prototypes that validated the stack |

## License

MIT. Dibi uses Qt under the LGPL v3.
