// Package adminuser creates an administrator: the core of
// `yumdrake-orchestrator create-admin`, shared with the demo setup.
package adminuser

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/audit"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// GroupName is the admin group Create puts the new user in.
const GroupName = "Administrators"

// ErrUserExists is returned when a user with that email already exists.
var ErrUserExists = errors.New("a user with that email already exists")

// Create makes the user, the admin group when needed, the membership and their
// audit entries (made by the system: nobody is signed in yet). via names the
// caller in the audit detail. Everything happens in tx; the caller commits.
func Create(ctx context.Context, tx pgx.Tx, email, name, passwordHash string, at time.Time, via string) error {
	user, err := store.CreateLocalUser(ctx, tx, email, name, "local", passwordHash)
	if errors.Is(err, store.ErrDuplicate) {
		return ErrUserExists
	}
	if err != nil {
		return err
	}
	userID := strconv.FormatInt(user.ID, 10)
	if err := audit.Record(ctx, tx, nil, at, audit.Event{Action: "user.create", TargetType: "user", TargetID: userID,
		Detail: map[string]any{"email": user.Email, "source": "local", "via": via}}); err != nil {
		return err
	}

	groups, err := store.ListGroups(ctx, tx)
	if err != nil {
		return err
	}
	var group *store.Group
	for i := range groups {
		if strings.EqualFold(groups[i].Name, GroupName) {
			group = &groups[i]
		}
	}
	switch {
	case group == nil:
		created, err := store.CreateGroup(ctx, tx, GroupName, true)
		if err != nil {
			return err
		}
		group = &created
		if err := audit.Record(ctx, tx, nil, at, audit.Event{Action: "group.create", TargetType: "group",
			TargetID: strconv.FormatInt(group.ID, 10), Detail: map[string]any{"name": group.Name, "admin": true}}); err != nil {
			return err
		}
	case !group.Admin:
		// Never promote an existing group silently: it may hold people who are not admins.
		return fmt.Errorf("a group named %q exists but is not an admin group; fix or rename it first", GroupName)
	}
	if err := store.SetManualMembership(ctx, tx, user.ID, group.ID, true); err != nil {
		return err
	}
	return audit.Record(ctx, tx, nil, at, audit.Event{Action: "group.member_add", TargetType: "group",
		TargetID: strconv.FormatInt(group.ID, 10), Detail: map[string]any{"user_id": user.ID, "group": group.Name}})
}
