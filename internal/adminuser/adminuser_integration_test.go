//go:build integration

package adminuser_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/adminuser"
	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

func create(t *testing.T, dbURL, email string) error {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := adminuser.Create(ctx, tx, email, "Ada", "hash-not-checked-here", time.Now(), "test"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestCreateMakesAnAdminAndReusesTheGroup(t *testing.T) {
	dbURL := testdb.New(t)
	if err := db.Migrate(context.Background(), dbURL); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, email := range []string{"first@example.test", "second@example.test"} {
		if err := create(t, dbURL, email); err != nil {
			t.Fatalf("Create(%s) = %v", email, err)
		}
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	groups, err := store.ListGroups(ctx, pool)
	if err != nil || len(groups) != 1 || groups[0].Name != "Administrators" || !groups[0].Admin {
		t.Fatalf("groups = %+v, %v; want one admin group Administrators", groups, err)
	}
	user, err := store.UserByEmail(ctx, pool, "second@example.test")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.Resolve(ctx, pool, user.ID, "session")
	if err != nil || !principal.Admin {
		t.Fatalf("principal = %+v, %v; want a global admin", principal, err)
	}
}

func TestCreateReportsAnExistingUser(t *testing.T) {
	dbURL := testdb.New(t)
	if err := db.Migrate(context.Background(), dbURL); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := create(t, dbURL, "ada@example.test"); err != nil {
		t.Fatal(err)
	}
	err := create(t, dbURL, "ada@example.test")
	if !errors.Is(err, adminuser.ErrUserExists) {
		t.Fatalf("second Create = %v, want adminuser.ErrUserExists", err)
	}
}
