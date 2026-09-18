package admin

import (
	"context"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Seed creates an initial admin account with a hashed password and returns
// its ID. It returns ErrDuplicateEmail if the email is already registered.
//
// Seed is the function a one-off command or an application's own bootstrap
// path calls; it does not read flags, environment variables, or a database
// connection string itself. See admin/cmd/seed for a runnable command built
// on it.
func Seed(ctx context.Context, store AdminStore, email, password, name, role string) (id string, err error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("admin: hash password: %w", err)
	}
	a := &Admin{Email: email, PasswordHash: string(hash), Name: name, Role: role}
	if err := store.Create(ctx, a); err != nil {
		return "", err
	}
	return a.ID, nil
}
