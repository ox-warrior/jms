package main

import (
	"fmt"
	"net/http"
)

// There is no admin-facing API that mints an Access Key on another user's
// behalf: authentication/api/access_key.go's create action always creates a
// key for self.request.user, and the only other call site of
// User.create_access_key() is the (undocumented, unused-by-JumpServer-itself)
// service-account-registrations endpoint. So getting a usable Access Key for
// any account created here unavoidably means: create a normal user with a
// temporary password, have a human log in as it once and self-service an
// Access Key from their own profile page, then run lock-account to kill the
// password so the account can never be used to log in again -- the Access
// Key created in between keeps working (AccessKey.is_valid only checks
// user.is_valid, i.e. is_active + not expired, never the password).

type createdUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

func doCreateUser(env *adminEnv, username, name, email, comment string) (*createdUser, string, error) {
	tempPassword, err := randomSecret(24)
	if err != nil {
		return nil, "", err
	}
	body := map[string]any{
		"username":          username,
		"name":              name,
		"email":             email,
		"comment":           comment,
		"password_strategy": "custom",
		"password":          tempPassword,
	}
	var created createdUser
	_, err = env.core.Call(http.MethodPost, "/api/v1/users/users/", body, &created)
	if err != nil {
		return nil, "", fmt.Errorf("create user %s: %w", username, err)
	}
	return &created, tempPassword, nil
}

func doLockAccount(env *adminEnv, username string) error {
	userID, err := findUserID(env, username)
	if err != nil {
		return err
	}
	newPassword, err := randomSecret(32)
	if err != nil {
		return err
	}
	body := map[string]any{"password_strategy": "custom", "password": newPassword}
	_, err = env.core.Call(http.MethodPatch, "/api/v1/users/users/"+userID+"/", body, nil)
	if err != nil {
		return fmt.Errorf("lock account %s: %w", username, err)
	}
	return nil
}

// Unlike service accounts (whose queryset is filtered out of the standard
// Users API entirely, see docs/PLAN.md §12), a plain user created by
// create-user is a normal row in that queryset, so the standard DELETE verb
// works. This is what makes rotation actually deletable end to end.
func doDeleteUser(env *adminEnv, username string) error {
	userID, err := findUserID(env, username)
	if err != nil {
		return err
	}
	_, err = env.core.Call(http.MethodDelete, "/api/v1/users/users/"+userID+"/", nil, nil)
	if err != nil {
		return fmt.Errorf("delete user %s: %w", username, err)
	}
	return nil
}

func findUserID(env *adminEnv, username string) (string, error) {
	candidates, err := env.core.GetShareUserInfo(username)
	if err != nil {
		return "", fmt.Errorf("look up user %s: %w", username, err)
	}
	for _, c := range candidates {
		if c.Username == username {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("user %s not found", username)
}
