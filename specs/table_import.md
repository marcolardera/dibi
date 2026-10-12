# Table import spec

This document describes the "import table" functionality: creating a new table from a CSV or XLSX file.

## Entry point

The user right clicks on a node of the object tree and clicks on "Import Table…". The entry is shown on:

- a schema (PostgreSQL, Oracle, SQL Server, DuckDB);
- a database, for the databases without a schema level (MySQL, MariaDB, SQLite);
- the "Tables" group of either, which imports into the same schema or database.

It is not offered for CSV and Parquet connections (read-only views of files), nor on the other groups (Views, Functions, …) or on tables. Importing into an existing table (append) is not supported.

## Dialog UI

Similar to the export one, the dialog prompts the user to select the format of the import file between CSV and XLSX. A note at the top names the destination ("Creates a table in schema public…") and says that the column types are inferred from the data.

### CSV Options

- File picker
- Has header flag ("The first row holds the column names"), on by default. If yes, use it for the column names, if no name them "column_<N>".
- Separator dropdown: comma, semicolon, tab, pipe.
- "NULL as" field: values equal to this text are imported as NULL. Empty means that empty values are NULL.
- Destination table name

### XLSX Options

- File picker
- Has header flag. If yes, use it for the column names, if no name them "column_<N>".
- Sheet name (if empty pick the first one). An unknown name is an error listing the sheets of the file.
- Destination table

Choosing a file selects the format from its extension (`.csv`, `.tsv`, `.txt` → CSV, `.tsv` also selecting the tab separator; `.xlsx`, `.xlsm` → XLSX) and fills the destination table with the file name (as in `FileTableName`), until the user edits it.

The dialog has "Cancel" and "Import" buttons. "Import" checks that the file exists and that a table name is given.

## Names

Table and column names are stored as the database would store them if typed unquoted: plain names (letters, digits, `_`) are folded to lower case on PostgreSQL and to upper case on Oracle; other names are kept exactly and quoted. Names are limited to 63 bytes (PostgreSQL's limit): a longer table name is an error, longer column names are shortened. Missing header names become `column_<N>`; duplicates (ignoring case) get a `_2`, `_3`, … suffix.

If the destination table already exists, the import fails before reading the file and the table is left untouched.

## Reading the file

The file is read twice.

The **first pass** checks the whole file and infers the type of each column. If it finds problems, nothing is written to the database and the problems are listed (the first 100):

- CSV: rows whose number of values differs from the header (or the first row), malformed quoting, text that is not valid UTF-8.
- XLSX: values in columns beyond the header.

The **second pass** streams the rows to the database.

CSV files are read as UTF-8, with an optional BOM; empty lines are skipped. XLSX sheets are read into memory (a worksheet holds at most about 1M rows); empty rows are skipped, missing cells at the end of a row are NULL, empty cells are NULL. XLSX values are taken as stored, not as displayed: numbers in full (rounded to the 15 significant digits Excel keeps), booleans as true/false, and numbers with a date or time format as dates or timestamps.

## Column types

Each column gets the narrowest type holding all its non-NULL values (leading and trailing spaces ignored):

| Inferred type | Values                                              | PostgreSQL       | MySQL / MariaDB         | SQL Server        | Oracle            | SQLite    | DuckDB    |
|---------------|-----------------------------------------------------|------------------|-------------------------|-------------------|-------------------|-----------|-----------|
| Boolean       | `true`, `false` (any case)                          | boolean          | BOOLEAN                 | bit               | NUMBER(1)         | BOOLEAN   | BOOLEAN   |
| Integer       | integers fitting 32 bits                            | integer          | INT                     | int               | NUMBER(10)        | INTEGER   | INTEGER   |
| Big integer   | integers fitting 64 bits                            | bigint           | BIGINT                  | bigint            | NUMBER(19)        | INTEGER   | BIGINT    |
| Decimal       | numbers with a decimal point (precision up to 38)   | numeric(p,s)     | DECIMAL(p,s)            | decimal(p,s)      | NUMBER(p,s)       | DECIMAL(p,s) | DECIMAL(p,s) |
| Float         | numbers in exponent notation (`1.5e3`)              | double precision | DOUBLE                  | float             | BINARY_DOUBLE     | REAL      | DOUBLE    |
| Date          | `YYYY-MM-DD`                                        | date             | DATE                    | date              | DATE              | DATE      | DATE      |
| Timestamp     | `YYYY-MM-DD hh:mm[:ss[.fff]]` (or with `T`)          | timestamp        | DATETIME[(fsp)]         | datetime2         | TIMESTAMP         | TIMESTAMP | TIMESTAMP |
| Text          | anything else                                       | text             | VARCHAR(n) or TEXT types | nvarchar(n) or nvarchar(max) | VARCHAR2(n CHAR) or CLOB | TEXT | VARCHAR |

- Integers with leading zeros (`007`) are codes, not numbers: they make the column text.
- Mixing kinds (numbers and dates, booleans and numbers, …) makes the column text. Integers and decimals together give a decimal; any float makes the column float; dates and timestamps together give a timestamp.
- A column with only NULLs is text.
- Text lengths are those of the longest value. MySQL uses `VARCHAR(n)` up to 255 characters while the row stays within MySQL's 65,535-byte limit, then `TEXT`, `MEDIUMTEXT` or `LONGTEXT`; SQL Server uses `nvarchar(max)` beyond 4,000 UTF-16 units; Oracle uses `CLOB` beyond 4,000 bytes.
- Empty values are NULL in columns that are not text. Oracle stores empty strings as NULL.
- SQLite stores dates and timestamps as ISO text.

## Loading

Each database is loaded with its fastest way in:

| Database        | Method                                                                                           |
|-----------------|--------------------------------------------------------------------------------------------------|
| PostgreSQL      | `COPY FROM STDIN` in binary format (pgx `CopyFrom`)                                              |
| SQL Server      | bulk copy (`INSERT BULK`, go-mssqldb `CopyIn`)                                                   |
| DuckDB          | the appender                                                                                     |
| MySQL / MariaDB | `LOAD DATA LOCAL INFILE` streamed from memory; if the server refuses it (`local_infile` is off, the default since MySQL 8), multi-row `INSERT`s |
| Oracle          | `INSERT`s with array binding, 1,000 rows at a time                                               |
| SQLite          | prepared `INSERT`s (there is no bulk path)                                                       |

The import is **all or nothing**: on any error or on cancellation, no table is left. On PostgreSQL, SQL Server, DuckDB and SQLite, `CREATE TABLE` and the load run in one transaction that is rolled back. On MySQL/MariaDB and Oracle, where DDL commits, the table is dropped. With `LOAD DATA LOCAL`, MySQL turns data errors into warnings and keeps going: any warning fails the import.

The import runs in the background with a progress dialog ("Reading… N rows", then "Importing… N rows") that can cancel it.

## Import output

In case of success, the status bar shows the number of rows, the file and the qualified table name ("Imported 2 rows from people.csv into public.people in 0.1 s."), and the "Tables" group of the destination is refreshed if it was loaded, so the new table shows.

In case of errors, the status bar says "Import failed; nothing was imported." and a Messages tab opens with the file, the table, the connection and the errors: the problems found in the file, or the error reported by the database. A cancelled import only reports "Import cancelled; nothing was imported." in the status bar.
