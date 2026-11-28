package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rcss-labs/yamdrake/internal/adminuser"
	"github.com/rcss-labs/yamdrake/internal/api"
	"github.com/rcss-labs/yamdrake/internal/app"
	"github.com/rcss-labs/yamdrake/internal/audit"
	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	createAdminUse = `usage: yumdrake-orchestrator create-admin --email <email> --name <name>

Creates a local user in the admin group "Administrators" (made if it does not
exist). The password is read from standard input, one line, never from an
argument or the environment:

  printf '%s\n' "$PASSWORD" | yumdrake-orchestrator create-admin --email a@example.com --name "Ada"

The database is configured through the same YUMDRAKE_* variables as the
service. Prints "created" on success. Exit codes: 0 created, 1 failure, 2 usage.
`
)

const unlockUse = `usage: yumdrake-orchestrator unlock --email <email>

Clears the failed sign-in counter of one email, so a locked account (5 wrong
passwords within 15 minutes) can sign in again at once. Use it when someone
keeps the break-glass admin locked. The unlock is audited as auth.unlock.

The database and Valkey are configured through the same YUMDRAKE_* variables
as the service. Prints "unlocked" on success. Exit codes: 0 unlocked, 1 failure
(for example, no such user), 2 usage.
`

// unlock implements the unlock subcommand and returns its exit code.
func unlock(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unlock", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, unlockUse) }
	email := flags.String("email", "", "email address to unlock")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return app.ExitOK
		}
		return app.ExitUsage
	}
	if flags.NArg() > 0 || strings.TrimSpace(*email) == "" {
		_, _ = io.WriteString(stderr, "unlock: --email is required and nothing else is accepted\n\n"+unlockUse)
		return app.ExitUsage
	}
	cfg, err := config.LoadOrchestrator(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return app.ExitUsage
	}
	scrub := func(err error) string { return config.Scrub(err.Error(), cfg.DatabaseURL) }

	openCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	pool, err := db.Open(openCtx, cfg.DatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock: open database:", scrub(err))
		return app.ExitFailure
	}
	defer pool.Close()
	client, err := cache.Open(app.CacheOptions(cfg.Common))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock:", err)
		return app.ExitFailure
	}
	defer client.Close()

	user, err := store.UserByEmail(ctx, pool, *email)
	if errors.Is(err, store.ErrNotFound) {
		_, _ = fmt.Fprintln(stderr, "unlock: no user has that email")
		return app.ExitFailure
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock:", scrub(err))
		return app.ExitFailure
	}
	// The audit entry and the deletion go together: the entry is committed only
	// once the counter is gone.
	tx, err := pool.Begin(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock: begin:", scrub(err))
		return app.ExitFailure
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := audit.Record(ctx, tx, nil, time.Now(), audit.Event{Action: "auth.unlock", TargetType: "user",
		TargetID: strconv.FormatInt(user.ID, 10), Detail: map[string]any{"email": user.Email, "via": "unlock command"}}); err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock:", scrub(err))
		return app.ExitFailure
	}
	key := api.LoginFailureKey(cfg.ValkeyKeyPrefix, user.Email)
	if err := client.Do(ctx, client.B().Del().Key(key).Build()).Error(); err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock: clear the failure counter:", err)
		return app.ExitFailure
	}
	if err := tx.Commit(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "unlock: the counter was cleared but the audit entry could not be saved:", scrub(err))
		return app.ExitFailure
	}
	_, _ = fmt.Fprintln(stdout, "unlocked")
	return app.ExitOK
}

// createAdmin implements the create-admin subcommand and returns its exit code.
func createAdmin(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, createAdminUse) }
	email := flags.String("email", "", "email address of the new admin")
	name := flags.String("name", "", "display name of the new admin")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return app.ExitOK
		}
		return app.ExitUsage
	}
	if flags.NArg() > 0 || strings.TrimSpace(*email) == "" || strings.TrimSpace(*name) == "" {
		_, _ = io.WriteString(stderr, "create-admin: --email and --name are required and nothing else is accepted\n\n"+createAdminUse)
		return app.ExitUsage
	}
	cfg, err := config.LoadOrchestrator(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return app.ExitUsage
	}
	password, ok := readPassword(stdin, stderr)
	if !ok {
		_, _ = io.WriteString(stderr, "create-admin: the password must be given on standard input\n\n"+createAdminUse)
		return app.ExitUsage
	}

	// Hash before the database is touched: a weak password fails fast.
	hash, err := auth.HashPassword(password)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "create-admin:", err)
		return app.ExitFailure
	}
	openCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	pool, err := db.Open(openCtx, cfg.DatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "create-admin: open database:", config.Scrub(err.Error(), cfg.DatabaseURL))
		return app.ExitFailure
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "create-admin: begin:", config.Scrub(err.Error(), cfg.DatabaseURL))
		return app.ExitFailure
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := adminuser.Create(ctx, tx, *email, *name, hash, time.Now(), "create-admin"); err != nil {
		_, _ = fmt.Fprintln(stderr, "create-admin:", config.Scrub(err.Error(), cfg.DatabaseURL))
		return app.ExitFailure
	}
	if err := tx.Commit(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "create-admin: commit:", config.Scrub(err.Error(), cfg.DatabaseURL))
		return app.ExitFailure
	}
	_, _ = fmt.Fprintln(stdout, "created")
	return app.ExitOK
}

// readPassword reads one line from stdin. It reports false when there is no
// line at all (a missing or empty input), so "nothing given" is a usage error
// while an empty or short line is a rejected password. On a terminal it turns
// echo off for the read.
func readPassword(stdin io.Reader, stderr io.Writer) (string, bool) {
	if stdin == nil {
		return "", false
	}
	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			restore := disableEcho(file, stderr)
			defer restore()
		}
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 4096)).ReadString('\n')
	if line == "" && err != nil {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

// disableEcho turns terminal echo off with stty and returns the function that
// turns it back on. Without stty it does nothing.
func disableEcho(tty *os.File, stderr io.Writer) func() {
	off := exec.Command("stty", "-echo") //nolint:gosec // fixed command
	off.Stdin = tty
	if err := off.Run(); err != nil {
		return func() {}
	}
	return func() {
		on := exec.Command("stty", "echo") //nolint:gosec // fixed command
		on.Stdin = tty
		_ = on.Run()
		_, _ = fmt.Fprintln(stderr)
	}
}
