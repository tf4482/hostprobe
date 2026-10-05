// hostprobe checks PostgreSQL host records once with ping or SSH.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

const help = projectName + ` — check PostgreSQL hosts once with ping or SSH

Usage:
  ` + projectName + `
  ` + projectName + ` --help | -h | help

Configuration:
  Uses the first existing file without merging:
  1. config.yml beside the executable
  2. ~/.config/` + projectName + `/config.yml
  The working directory is not searched.
  With no file, creates a user-only template at location 2 and exits 1.

Behavior:
  Validates the table, checks valid rows concurrently with separate ping and
  SSH limits, updates status and last_check, prints results, and exits.
  Text status storage is inferred as online/offline or true/false; native
  PostgreSQL booleans are supported. Terminal results use online/offline.
  Invalid rows are skipped unchanged. SSH requires trusted host keys and
  agent or key-based non-interactive authentication.

Exit codes:
  0    Every valid result was saved; offline hosts are not errors
  1    Configuration, validation, dependency, database, or skipped-row error
  130  Interrupted by the user
`

// Colored line output
type printer struct {
	out   io.Writer
	color bool
}

// Framed startup banner
func (p printer) header() {
	title := "  ⚙️  " + projectName + " — starting  "
	border := strings.Repeat("─", utf8.RuneCountInString(title))
	fmt.Fprintln(p.out)
	for _, line := range []string{"╭" + border + "╮", "│" + title + "│", "╰" + border + "╯"} {
		p.print(line, "36")
	}
}

func (p printer) line(icon, text, color string) {
	p.print(icon+" "+text, color)
}

func (p printer) print(text, color string) {
	if p.color && color != "" {
		text = "\033[" + color + "m" + text + "\033[0m"
	}
	fmt.Fprintln(p.out, text)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) == 1 && (arguments[0] == "-h" || arguments[0] == "--help" || arguments[0] == "help") {
		fmt.Print(help)
		return 0
	}
	if len(arguments) > 0 {
		fmt.Fprintf(os.Stderr, "❌ Unknown argument: %s\n", arguments[0])
		fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", projectName)
		return 1
	}

	_, noColor := os.LookupEnv("NO_COLOR")
	out := printer{os.Stdout, term.IsTerminal(int(os.Stdout.Fd())) && !noColor}

	// First Ctrl-C cancels cleanly, second one kills
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	context.AfterFunc(ctx, stop)

	code := probe(ctx, out)
	if ctx.Err() != nil {
		out.line("🛑", "Interrupted; running checks and connections closed", "33")
		return 130
	}
	return code
}

// Load, check, save, report
func probe(ctx context.Context, out printer) int {
	out.header()
	binaryDir, err := executableDir()
	if err != nil {
		out.line("❌", fmt.Sprintf("Configuration error: %v", err), "31")
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		out.line("❌", fmt.Sprintf("Configuration error: %v", err), "31")
		return 1
	}
	_, config, err := loadConfig(binaryDir, home)
	var created *configCreatedError
	if errors.As(err, &created) {
		out.line("📝", "Created configuration: "+created.path, "33")
		out.line("✏️", "Edit database name, user, password, and any other deployment values, then rerun", "")
		return 1
	}
	if err != nil {
		out.line("❌", fmt.Sprintf("Configuration error: %v", err), "31")
		return 1
	}

	records, err := fetchRecords(ctx, config.Database, config.Backups)
	if err != nil {
		out.line("❌", fmt.Sprintf("Database error: %v", err), "31")
		return 1
	}

	failed := false
	var valid []HostRecord
	for _, raw := range records.Records {
		record, reason := validateRecord(raw)
		if reason != "" {
			failed = true
			out.line("⏭️", fmt.Sprintf("%s · skipped · %s", record.Name, reason), "33")
			continue
		}
		valid = append(valid, record)
	}

	checked := checkHosts(ctx, valid, config.Settings)
	if ctx.Err() != nil {
		return 130
	}
	var writable []CheckedHost
	var updates []StatusUpdate
	for _, item := range checked {
		printObservation(out, item)
		if item.Result.State == stateError {
			failed = true
			continue
		}
		writable = append(writable, item)
		updates = append(updates, StatusUpdate{item.Record.ID, item.Result.State == stateOnline})
	}
	if len(updates) == 0 {
		return exitCode(failed)
	}

	// Same database for reads and updates
	saved, err := saveStatuses(ctx, records.Database, updates, records.Format)
	for index, item := range writable {
		message := ""
		if err != nil {
			message = err.Error()
		} else {
			message = saved[index]
		}
		if message != "" {
			failed = true
			out.line("💾", fmt.Sprintf("%s · observed %s · not saved · %s", item.Record.Name, item.Result.State, message), "31")
		}
	}
	return exitCode(failed)
}

func exitCode(failed bool) int {
	if failed {
		return 1
	}
	return 0
}

func printObservation(out printer, checked CheckedHost) {
	icon, color := "⚠️", "33"
	switch checked.Result.State {
	case stateOnline:
		icon, color = "✅", "32"
	case stateOffline:
		icon, color = "❌", "31"
	}
	out.line(icon, fmt.Sprintf("%s · %s · %s · %s",
		checked.Record.Name, checked.Record.Method, checked.Result.State, checked.Result.Reason), color)
}
