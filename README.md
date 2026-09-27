# Dibi

A lightweight SQL database admin tool: explore database objects, inspect
their metadata, run queries and export results. A smaller, faster
alternative to DBeaver or DataGrip.

Supported sources: **PostgreSQL**, **MySQL**, **MariaDB**, **Oracle** (no
Instant Client needed), **SQL Server**, **SQLite**, **DuckDB**, and **CSV** /
**Parquet** files (queried through DuckDB).

See [spec.md](spec.md) for the full specification.

## Features

- Object tree per connection, loaded lazily: databases, schemas, tables,
  views, materialized views, functions, procedures, packages, sequences,
  and indexes/triggers under their table.
  A filter box narrows it as you type.
- Information panel: columns (type, nullability, key, default, comment),
  constraints, indexes, source code of views/routines/triggers, and a
  50-row data preview.
- `Ctrl+C` on a table or view in the tree copies its query-ready name
  (e.g. `schema.table`, quoted where needed).
- SQL editor with syntax highlighting, line numbers and find/replace
  (`Ctrl+F`, `Ctrl+H`). **Run** (`Ctrl+Enter`) executes the
  selection, or the whole script statement by statement; each result set
  gets its own tab. Each editor tab has its own database session.
- New scripts default to the connection (and database) you used last.
- **Stop** cancels the running query on the server and keeps the session.
- Results are streamed from the cursor, 1000 rows per page.
- Copy cells with `Ctrl+C` (full values, tab-separated).
- Export to CSV or XLSX (re-runs the query and streams all rows).
- Saved queries.
- TLS for server connections (SSL modes from *disable* to *verify-full*,
  CA and client certificates, Oracle wallets).

## Building (Linux)

Requirements: Go 1.26+, a C/C++ toolchain and the Qt 6 development files.

```sh
sudo apt install qt6-base-dev qt6-base-private-dev build-essential
make build        # produces bin/dibi
```

The first build compiles the Qt bindings and takes a few minutes.

## Configuration

Stored in the user config directory (`~/.config/dibi` on Linux):

- `connections/` — one YAML file per connection. **Passwords are stored in
  plaintext** in this version.
- `queries/` — saved queries, one `.sql` file each.
- `state.yaml` — the last used connection.

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
