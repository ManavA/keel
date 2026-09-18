// Command seed creates the first admin account, using admin.Seed against a
// Postgres-backed admin.AdminStore (admin/pg).
//
// The password is never a command-line argument: anything passed as an
// argv entry is visible to every other process on the machine via
// /proc/<pid>/cmdline or `ps aux`, and often ends up in shell history too.
// Set ADMIN_SEED_PASSWORD, or leave it unset and this command reads one
// line from stdin instead:
//
//	ADMIN_SEED_PASSWORD=... seed -email=admin@example.com
//	printf '%s' "$PASSWORD" | seed -email=admin@example.com
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ManavA/keel/admin"
	adminpg "github.com/ManavA/keel/admin/pg"
	keelpg "github.com/ManavA/keel/pg"
)

// minSeedPasswordLength is deliberately higher than auth's end-user minimum:
// this credential can reach every admin-only field and endpoint an
// admin.Service protects.
const minSeedPasswordLength = 12

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var email, name, role string
	var force bool
	flag.StringVar(&email, "email", "", "admin email (required)")
	flag.StringVar(&name, "name", "Admin", "admin display name")
	flag.StringVar(&role, "role", "admin", "admin role")
	flag.BoolVar(&force, "force", false, "seed even if admin_users already has rows")
	flag.Parse()

	if email == "" {
		return errors.New("usage: seed -email=admin@example.com [-name=\"Admin User\"] [-role=admin] [-force]\n" +
			"password: set ADMIN_SEED_PASSWORD, or pipe one line on stdin")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	if len(password) < minSeedPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minSeedPasswordLength)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL environment variable is required")
	}

	ctx := context.Background()
	pool, err := keelpg.Open(ctx, keelpg.Options{URL: dbURL})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if !force {
		var count int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
			return fmt.Errorf("check for existing admins: %w", err)
		}
		if count > 0 {
			return fmt.Errorf("admin_users already has %d row(s); pass -force to seed anyway", count)
		}
	}

	store := adminpg.NewAdminStore(pool)
	id, err := admin.Seed(ctx, store, email, password, name, role)
	if err != nil {
		if errors.Is(err, admin.ErrDuplicateEmail) {
			return fmt.Errorf("an admin with email %q already exists", email)
		}
		return fmt.Errorf("seed admin: %w", err)
	}

	fmt.Printf("admin created: id=%s email=%s role=%s\n", id, email, role)
	return nil
}

// readPassword prefers ADMIN_SEED_PASSWORD, since a script or secret manager
// injecting an env var is the common automated path; interactively it falls
// back to one line of stdin, so the password never appears as a command-line
// argument.
func readPassword() (string, error) {
	if pw := os.Getenv("ADMIN_SEED_PASSWORD"); pw != "" {
		return pw, nil
	}
	fmt.Fprint(os.Stderr, "Password (ADMIN_SEED_PASSWORD not set, reading stdin): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
