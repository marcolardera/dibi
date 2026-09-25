# Dibi - database admin tool

## Overview

A lightweight SQL database admin tool, similar to DBeaver or DataGrip but smaller, more efficient and less bloated.

## Requirements

The tool is developed in **Go** and compiled into a single binary. It must be portable to the 3 main operating systems (Linux, Windows and macOS). The GUI must feel native.

It should support, for the first iteration, the following databases:

- PostgreSQL
- MariaDB
- MySQL
- Oracle DB
- SQLite
- DuckDB

It should also support the following data formats (treated as database connections):

- CSV
- Parquet

The tool should allow the user to create a connection to any of those databases (storing the credentials and connection parameters). After that the user should be able to explore the objects inside the database (schemas, tables, views, stored procedures, indexes etc.), inspect metadata and schema of each. The user should also be able to look at a data preview of the first 50 rows of a table in a classic grid.

The tool must support running custom SQL queries in an editor (with syntax highlighting) and seeing the results. Exporting data to CSV or XLSX must be supported. Queries can be saved for later use.

## Technology stack

| Concern            | Choice                                                              |
|--------------------|---------------------------------------------------------------------|
| Language           | Go                                                                  |
| GUI                | Qt 6 via [miqt](https://github.com/mappu/miqt) bindings, statically linked |
| SQL editor widget  | QScintilla (via miqt)                                               |
| PostgreSQL         | `github.com/jackc/pgx`                                              |
| MySQL / MariaDB    | `github.com/go-sql-driver/mysql`                                    |
| Oracle             | `github.com/sijms/go-ora` (pure Go, no Oracle Instant Client needed) |
| SQLite             | `modernc.org/sqlite` or `github.com/mattn/go-sqlite3`               |
| DuckDB             | `github.com/duckdb/duckdb-go` (cgo, statically linked)              |
| CSV / Parquet      | DuckDB (`read_csv_auto`, `read_parquet`)                            |
| XLSX export        | `github.com/xuri/excelize` (streaming writer)                       |
| YAML               | `gopkg.in/yaml.v3`                                                  |

All queries run in background goroutines and use `context.Context`, so the UI never freezes and queries can be cancelled.

### Technical risks to validate first (spike)

Before building the real application, a throwaway prototype must prove on all 3 OSes:

1. A static single binary including miqt/Qt 6 and duckdb-go builds and runs.
2. A `QTableView` backed by a custom Go `QAbstractTableModel` scrolls smoothly over 1M rows.
3. A QScintilla editor with SQL highlighting works through miqt.
4. go-ora connects to an Oracle Free container and reads metadata.

Fallback if miqt proves insufficient: C++ with Qt 6 (same architecture, but Oracle would require Instant Client).

## High-level GUI idea

The GUI is composed of these parts:

- A toolbar (grouped, large icons with text labels below)
- A left side panel for inspecting the database object hierarchy
- A center area for visualizing information (such as a table schema when the user clicks on the left side panel)
- A SQL editor
- A data results grid
- A tab bar for switching between central panels (information widget, SQL editor + result grid)
- A status bar with room for adding icons/buttons later

No menu bar: the toolbar gives immediate access to the key actions.

### Toolbar

Not a literal tabbed ribbon: a single toolbar with buttons visually grouped into sections, large icons with the text label below. This looks native on all 3 OSes.

Groups and buttons:

- **Connection**
  - New Connection (opens a new sub-window)
- **Script**
  - New SQL script
  - Open SQL script
  - Save SQL script
- **Execute**
  - Run (enabled when the current tab is a SQL editor)
  - Stop (enabled while a query is running; cancels it on the server)
- ...

### Left side panel

The hierarchy depends on the database type:

| Database        | Hierarchy                                                         |
|-----------------|-------------------------------------------------------------------|
| PostgreSQL      | Connection → Database → Schema → object type → object            |
| MySQL / MariaDB | Connection → Database (= schema) → object type → object          |
| Oracle          | Connection → Schema (= user) → object type → object              |
| SQLite          | Connection → Database (`main` + attached) → object type → object |
| DuckDB          | Connection → Database → Schema → object type → object            |
| CSV / Parquet   | Connection (a single file) → the file, shown as one table        |

Object types include tables, views, stored procedures/functions and sequences, depending on what each database supports. Indexes, constraints and triggers are shown **under their table**, not next to it.

Children are **loaded lazily** when a node is expanded (Oracle schemas can contain thousands of objects).

Left click opens the information panel with the detailed information about the object:

- For tables: the full schema, including data types and constraints (primary key, foreign key, not null, unique, default values), plus its indexes.
- For views, stored procedures and functions: the **source code**, shown read-only with syntax highlighting.
- A data preview of the first 50 rows (tables and views).

The preview query uses the right syntax for each database type (e.g. `LIMIT 50`, or `FETCH FIRST 50 ROWS ONLY` on Oracle).

### New Connection window

- Choose the database type first; the form then shows only the fields relevant to that type (host, port, database, user, password for server databases; a file path for SQLite, DuckDB, CSV and Parquet).
- A **Test connection** button.

### SQL Editor

- Syntax highlighting for ANSI SQL keywords, strings, numbers and comments.
- No autocomplete.
- Each SQL editor tab has a **connection selector** (dropdown) at the top.
- Each SQL editor tab opens its **own database session** (separate connection), so tabs never block each other and Stop only affects the current tab.
- **Run** executes the current selection, if any; otherwise the whole content of the tab.
- Shortcut: `Ctrl+Enter` runs (`Cmd+Enter` on macOS).
- If the text to run contains multiple statements, Dibi splits it into statements (respecting strings, comments and dialect-specific blocks such as PL/SQL `BEGIN … END;`) and runs them in order. Each statement that returns rows opens its own result tab; for other statements, the affected row count is shown in the status bar. Execution stops at the first error, which is shown in the result area.

#### Default connection for new scripts

Choosing the right connection by default is a key usability goal (a main pain point of similar tools).

Dibi tracks the **last used connection**, updated whenever the user:

- selects, expands or opens any node in the left side panel (the node's connection),
- switches to a tab bound to a connection (information panel or SQL editor),
- changes the connection selector of a SQL editor tab, or runs a query in it.

"New SQL script" pre-selects the last used connection. Example: the user inspects a table of a PostgreSQL connection, then clicks "New SQL script": the new tab is bound to that PostgreSQL connection.

If no connection has been used yet in the session, the selector is preset to the connection used last in the previous session (persisted in the config directory), otherwise it is left empty.

#### Saving and opening scripts

- **Save** always writes into `dibi/queries/` (asks for a name the first time).
- **Open** shows a list of the saved queries in `dibi/queries/` only (no arbitrary file dialog).
- **Stop** cancels the running query on the server (e.g. `pg_cancel_backend`, `KILL QUERY`, Oracle break).
- Autocommit is on by default. Transaction handling will be refined in a later version.

### Result area

- Classic grid with alternating row colors and a horizontal scrollbar.
- Read-only (no editing of data in v1).
- Also used to show query error messages.

**Pagination / fetching:**

- Results are streamed from the database cursor. The query is **not** rewritten with `LIMIT/OFFSET`.
- Rows are fetched 1000 at a time (one page) on demand, and pages already fetched are cached in memory.
- The total number of pages is unknown until the cursor is exhausted: the status bar shows `Page N of ?` until then, and `Page N of M` afterwards.
- "Last page" fetches the remaining rows until the end of the cursor.

**Value rendering:**

- `NULL` is shown as the string `NULL` in gray, so it can be told apart from an empty string.
- Dates, times and timestamps are shown in ISO 8601 format (`YYYY-MM-DD`, `YYYY-MM-DD HH:MM:SS[.fff][±TZ]`).
- Long text is truncated in the cell (about 200 characters) with an ellipsis (`…`). The full value is always available through copy.
- Binary values are shown as a placeholder with their size, e.g. `<binary 2.3 KB>`.

**Copy:**

- `Ctrl+C` (`Cmd+C` on macOS) and a right-click "Copy" context menu copy the selected cell(s) with their **full, untruncated** value.
- Copying multiple cells produces tab-separated text (rows separated by newlines), so it can be pasted into a spreadsheet.

### Status bar

On the left side it shows status messages (like "Query completed in N seconds."). On the right side it contains:

- Pagination widget (current page, with icons for first page / previous page / next page / last page)
- Export button. It opens a dialog for choosing the format (CSV or XLSX), the output path and some format-specific options (separator, etc.).

### Export

Export **re-runs the query** and streams all rows to the output file (not only the pages fetched in the grid). The dialog states this clearly. Export runs in the background and can be cancelled.

## Data storage

As stated before, this tool must be a single binary. However, some data must be stored:

- Connection data (`.yaml` format)
- Saved queries (`.sql` text files)

Data is stored in the OS config directory (as returned by Go's `os.UserConfigDir()`):

| OS      | Location                                      |
|---------|-----------------------------------------------|
| Linux   | `$XDG_CONFIG_HOME/dibi`, or `~/.config/dibi`  |
| macOS   | `~/Library/Application Support/dibi`          |
| Windows | `%APPDATA%\dibi`                              |

The `dibi` folder contains two subfolders:

- `connections/`: **one YAML file per connection**
- `queries/`: one `.sql` file per saved query

Small application state (such as the last used connection) is stored in `dibi/state.yaml`.

## Supported versions

- PostgreSQL 12+
- Oracle 12c+
- MySQL, MariaDB, SQLite, DuckDB: currently supported releases

## Look and feel

- Light theme only in v1.

In v1, passwords are stored in **plaintext** in the connection YAML file. Moving them to the OS keychain is planned for a later version.

## Out of scope for v1

- Editing data in the grid
- SSH tunnels
- SSL/TLS connection options
- Autocomplete
- ER diagrams
- Generated DDL view
- OS keychain for credentials
- Advanced transaction handling (manual commit/rollback)
- Dark theme
- CSV/Parquet connections pointing to a folder of files
- Opening `.sql` files from outside `dibi/queries/`
