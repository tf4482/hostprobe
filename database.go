package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Storage vocabulary of the status column
type statusFormat int

const (
	statusWords statusFormat = iota
	statusBooleanText
	statusBoolean
)

// Unvalidated row as stored
type RawRecord struct {
	ID      any
	Name    any
	Status  any
	Method  any
	IP      any
	SSHUser any
	SSHPort any
}

// Rows plus the database they came from
type RecordSet struct {
	Records  []RawRecord
	Format   statusFormat
	Database DatabaseConfig
}

// Observed state for one identifier
type StatusUpdate struct {
	ID     any
	Online bool
}

// Marker for an unreachable database
type connectError struct{ err error }

func (e *connectError) Error() string { return connectReason(e.err) }

var hostsTable = pgx.Identifier{dbSchema, dbTable}.Sanitize()

// Read from the first reachable database
func fetchRecords(ctx context.Context, primary DatabaseConfig, backups []DatabaseConfig) (*RecordSet, error) {
	var failures []string
	for _, target := range append([]DatabaseConfig{primary}, backups...) {
		records, err := fetchRecordsFrom(ctx, target)
		var unreachable *connectError
		if !errors.As(err, &unreachable) {
			return records, err
		}
		failures = append(failures, fmt.Sprintf("%s:%d: %v", target.Host, target.Port, unreachable))
	}
	return nil, fmt.Errorf("PostgreSQL connection failed for all configured databases: %s",
		strings.Join(failures, "; "))
}

// Validate the table and load every row
func fetchRecordsFrom(ctx context.Context, config DatabaseConfig) (*RecordSet, error) {
	conn, err := connect(ctx, config)
	if err != nil {
		return nil, &connectError{err}
	}
	defer disconnect(conn)

	columns, err := columnMetadata(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := requireColumns(columns, "name", "status", "method", "ip", "last_check", dbIDColumn); err != nil {
		return nil, err
	}
	if err := validateIdentifiers(ctx, conn); err != nil {
		return nil, err
	}
	records, err := fetchRows(ctx, conn, false)
	if err != nil {
		return nil, err
	}
	// SSH columns only required when SSH rows exist
	for _, record := range records {
		if method, ok := record.Method.(string); ok && strings.ToLower(strings.TrimSpace(method)) == "ssh" {
			if err := requireColumns(columns, "ssh_user", "ssh_port"); err != nil {
				return nil, err
			}
			if records, err = fetchRows(ctx, conn, true); err != nil {
				return nil, err
			}
			break
		}
	}
	format, err := inferStatusFormat(columns["status"], records)
	if err != nil {
		return nil, err
	}
	return &RecordSet{Records: records, Format: format, Database: config}, nil
}

// Write each status in its own transaction; one message per update, empty on success
func saveStatuses(ctx context.Context, config DatabaseConfig, updates []StatusUpdate, format statusFormat) ([]string, error) {
	conn, err := connect(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL connection failed while saving results: %s", connectReason(err))
	}
	defer disconnect(conn)

	query := fmt.Sprintf("UPDATE %s SET status = $1, last_check = CURRENT_TIMESTAMP WHERE %s = $2",
		hostsTable, pgx.Identifier{dbIDColumn}.Sanitize())
	results := make([]string, 0, len(updates))
	for _, update := range updates {
		results = append(results, saveStatus(ctx, conn, query, storedStatus(update.Online, format), update.ID))
	}
	return results, nil
}

func saveStatus(ctx context.Context, conn *pgx.Conn, query string, status, id any) string {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "PostgreSQL update failed: " + oneLine(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, query, status, id)
	if err != nil {
		return "PostgreSQL update failed: " + oneLine(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Sprintf("status update matched %d rows", tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return "PostgreSQL update failed: " + oneLine(err)
	}
	return ""
}

func connect(ctx context.Context, config DatabaseConfig) (*pgx.Conn, error) {
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s connect_timeout=10",
		dsnValue(config.Host), config.Port, dsnValue(config.Name), dsnValue(config.User), dsnValue(config.Password))
	return pgx.Connect(ctx, dsn)
}

// Quote a keyword/value connection string value
func dsnValue(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// Bounded close, even after cancellation
func disconnect(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn.Close(ctx)
}

// Single-line error text for terminal output
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// Short cause of a failed connection attempt
func connectReason(err error) string {
	var server *pgconn.PgError
	if errors.As(err, &server) {
		return server.Message
	}
	var network *net.OpError
	if errors.As(err, &network) {
		return network.Error()
	}
	return oneLine(err)
}

func readFailed(err error) error {
	return fmt.Errorf("PostgreSQL read failed: %s", oneLine(err))
}

// Column names and data types of the hosts table
func columnMetadata(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT column_name::text, data_type::text
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2`, dbSchema, dbTable)
	if err != nil {
		return nil, readFailed(err)
	}
	columns := map[string]string{}
	var name, dataType string
	if _, err := pgx.ForEachRow(rows, []any{&name, &dataType}, func() error {
		columns[name] = dataType
		return nil
	}); err != nil {
		return nil, readFailed(err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %s.%s does not exist or is not visible", dbSchema, dbTable)
	}
	return columns, nil
}

func requireColumns(columns map[string]string, required ...string) error {
	var missing []string
	for _, name := range required {
		if _, ok := columns[name]; !ok {
			missing = append(missing, name)
		}
	}
	if missing == nil {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("missing required database column(s): %s", strings.Join(missing, ", "))
}

// Identifier must be non-null and unique
func validateIdentifiers(ctx context.Context, conn *pgx.Conn) error {
	id := pgx.Identifier{dbIDColumn}.Sanitize()
	var nulls int64
	err := conn.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s IS NULL", hostsTable, id)).Scan(&nulls)
	if err != nil {
		return readFailed(err)
	}
	var duplicate bool
	err = conn.QueryRow(ctx, fmt.Sprintf(
		"SELECT EXISTS (SELECT 1 FROM %s GROUP BY %s HAVING count(*) > 1)", hostsTable, id)).Scan(&duplicate)
	if err != nil {
		return readFailed(err)
	}
	if nulls > 0 {
		return fmt.Errorf("identifier %s contains null values", dbIDColumn)
	}
	if duplicate {
		return fmt.Errorf("identifier %s is not unique", dbIDColumn)
	}
	return nil
}

func fetchRows(ctx context.Context, conn *pgx.Conn, includeSSH bool) ([]RawRecord, error) {
	names := []string{dbIDColumn, "name", "status", "method", "ip"}
	if includeSSH {
		names = append(names, "ssh_user", "ssh_port")
	}
	for index, name := range names {
		names[index] = pgx.Identifier{name}.Sanitize()
	}
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(names, ", "), hostsTable))
	if err != nil {
		return nil, readFailed(err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RawRecord, error) {
		values, err := row.Values()
		if err != nil {
			return RawRecord{}, err
		}
		record := RawRecord{ID: values[0], Name: values[1], Status: values[2], Method: values[3], IP: values[4]}
		if includeSSH {
			record.SSHUser, record.SSHPort = values[5], values[6]
		}
		return record, nil
	})
	if err != nil {
		return nil, readFailed(err)
	}
	return records, nil
}

// Detect how existing rows spell their status
func inferStatusFormat(dataType string, records []RawRecord) (statusFormat, error) {
	if dataType == "boolean" {
		for _, record := range records {
			if _, ok := record.Status.(bool); !ok {
				return 0, errors.New("boolean status values must not be null")
			}
		}
		return statusBoolean, nil
	}
	if dataType != "text" && dataType != "character varying" && dataType != "character" {
		return 0, fmt.Errorf("unsupported status column type: %s", dataType)
	}
	words, booleans := 0, 0
	for _, record := range records {
		text, _ := record.Status.(string)
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "":
			return 0, errors.New("text status values must not be null or blank")
		case "online", "offline":
			words++
		case "true", "false":
			booleans++
		default:
			return 0, errors.New("text status values must consistently use online/offline or true/false")
		}
	}
	switch {
	case booleans == 0:
		return statusWords, nil
	case words == 0:
		return statusBooleanText, nil
	}
	return 0, errors.New("text status values must consistently use online/offline or true/false")
}

// Observed state in the table's vocabulary
func storedStatus(online bool, format statusFormat) any {
	switch format {
	case statusBoolean:
		return online
	case statusBooleanText:
		return fmt.Sprint(online)
	}
	if online {
		return "online"
	}
	return "offline"
}
