// Package postgres implements the Driver interface.
package postgres

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/db-journey/migrate/direction"
	"github.com/db-journey/migrate/driver"
	"github.com/db-journey/migrate/file"
	"github.com/lib/pq"
)

var _ driver.Driver = (*Driver)(nil)

// Driver is the postgres driver for journey.
type Driver struct {
	db *sql.DB
}

const tableName = "public.schema_migrations"
const txDisabledOption = "disable_ddl_transaction"

// make sure our driver still implements the driver.Driver interface
var _ driver.Driver = (*Driver)(nil)

// Initialize opens and verifies the database handle.
func (driver *Driver) Initialize(url string) error {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return err
	}
	if err := db.Ping(); err != nil {
		return err
	}
	driver.db = db

	return driver.ensureVersionTableExists()
}

// SetDB replaces the current database handle.
func (driver *Driver) SetDB(db *sql.DB) {
	driver.db = db
}

// Close closes the database handle.
func (driver *Driver) Close() error {
	return driver.db.Close()
}

func (driver *Driver) ensureVersionTableExists() error {
	// avoid DDL statements if possible for BDR (see #23)
	var c int
	if err := driver.db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_name = $1", tableName).Scan(&c); err != nil {
		return err
	}

	if c <= 0 {
		_, err := driver.db.Exec("CREATE TABLE IF NOT EXISTS " + tableName + " (version bigint not null primary key)")
		return err
	}

	// table schema_migrations already exists, check if the schema is correct, ie: version is a bigint

	var dataType string
	if err := driver.db.QueryRow("SELECT data_type FROM information_schema.columns where table_name = $1 and column_name = 'version'", tableName).Scan(&dataType); err != nil {
		return err
	}

	if dataType == "bigint" {
		return nil
	}

	_, err := driver.db.Exec("ALTER TABLE " + tableName + " ALTER COLUMN version TYPE bigint USING version::bigint")
	return err
}

// FilenameExtension returns "sql".
func (driver *Driver) FilenameExtension() string {
	return "sql"
}

// Migrate performs the migration of any one file.
func (driver *Driver) Migrate(f file.File, pipe chan interface{}) {
	defer close(pipe)
	pipe <- f

	tx, err := driver.db.Begin()
	if err != nil {
		pipe <- err
		return
	}

	if f.Direction == direction.Up {
		if _, err = tx.Exec("INSERT INTO "+tableName+" (version) VALUES ($1)", f.Version); err != nil {
			pipe <- err
			if err = tx.Rollback(); err != nil {
				pipe <- err
			}
			return
		}
	} else if f.Direction == direction.Down {
		if _, err = tx.Exec("DELETE FROM "+tableName+" WHERE version=$1", f.Version); err != nil {
			pipe <- err
			if err = tx.Rollback(); err != nil {
				pipe <- err
			}
			return
		}
	}

	if err = f.ReadContent(); err != nil {
		pipe <- err
		return
	}

	if txDisabled(fileOptions(f.Content)) {
		_, err = driver.db.Exec(string(f.Content))
	} else {
		_, err = tx.Exec(string(f.Content))
	}

	if err != nil {
		pqErr := err.(*pq.Error)
		offset, err := strconv.Atoi(pqErr.Position)
		if err == nil && offset >= 0 {
			lineNo, columnNo := file.LineColumnFromOffset(f.Content, offset-1)
			errorPart := file.LinesBeforeAndAfter(f.Content, lineNo, 5, 5, true)
			pipe <- fmt.Errorf("%s %v: %s in line %v, column %v:\n\n%s", pqErr.Severity, pqErr.Code, pqErr.Message, lineNo, columnNo, string(errorPart))
		} else {
			pipe <- fmt.Errorf("%s %v: %s", pqErr.Severity, pqErr.Code, pqErr.Message)
		}

		if err := tx.Rollback(); err != nil {
			pipe <- err
		}
		return
	}

	if err := tx.Commit(); err != nil {
		pipe <- err
		return
	}
}

// Version returns the current migration version.
func (driver *Driver) Version() (file.Version, error) {
	var version file.Version
	err := driver.db.QueryRow("SELECT version FROM " + tableName + " ORDER BY version DESC LIMIT 1").Scan(&version)
	if err == sql.ErrNoRows {
		return version, nil
	}

	return version, err
}

// Versions returns the list of applied migrations.
func (driver *Driver) Versions() (file.Versions, error) {
	rows, err := driver.db.Query("SELECT version FROM " + tableName + " ORDER BY version DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := file.Versions{}
	for rows.Next() {
		var version file.Version
		if err = rows.Scan(&version); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return versions, err
}

// Execute a SQL statement
func (driver *Driver) Execute(statement string) error {
	_, err := driver.db.Exec(statement)
	return err
}

// fileOptions returns the list of options extracted from the first line of the file content.
// Format: "-- <option1> <option2> <...>"
func fileOptions(content []byte) []string {
	firstLine := strings.SplitN(string(content), "\n", 2)[0]
	if !strings.HasPrefix(firstLine, "-- ") {
		return []string{}
	}
	opts := strings.TrimPrefix(firstLine, "-- ")
	return strings.Split(opts, " ")
}

func txDisabled(opts []string) bool {
	for _, v := range opts {
		if v == txDisabledOption {
			return true
		}
	}
	return false
}

func init() {
	// According to the PostgreSQL documentation (section 32.1.1.2), postgres
	// library supports two URI schemes: postgresql:// and postgres://
	// https://www.postgresql.org/docs/current/static/libpq-connect.html#LIBPQ-CONNSTRING
	drv := Driver{}
	driver.RegisterDriver("postgres", &drv)
	driver.RegisterDriver("postgresql", &drv)
}
