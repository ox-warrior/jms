package main

import (
	"fmt"
	"net/http"
)

// jmsdPermissionCodenames are the permissions the jmsd account is granted,
// verified against the JumpServer v4.10.16 source (see docs/PLAN.md §7.1).
// view_superconnectiontokensecret lets jmsd read a target account's secret
// from a ticket it minted. Only DB queries need it (jmsd runs the SQL itself,
// since Koko has no non-interactive SQL); without it every query fails at
// GetConnectTokenInfo with 403, and SSH / k8s keep working.
//
// view_userassets, add_superconnectiontoken and the secret are what jmsd uses
// at runtime; view_user is not. jmsd stopped
// needing it once the user id came out of signature verification instead of a
// lookup by username (docs/PLAN.md §5.22). It is kept because jmsd-admin's own
// findUserID needs it and this list is what the role ends up holding -- a role
// that cannot resolve a username would make rotation harder than the
// permission is worth removing.
var jmsdPermissionCodenames = []string{
	"view_userassets",
	"add_superconnectiontoken",
	"view_superconnectiontokensecret",
	"view_user",
}

// operatorPermissionCodenames are what an account needs to run every
// jmsd-admin command *other* than setup-operator-role itself (which, like
// every other command's first-ever run, needs a fully admin-capable key --
// see loadAdminEnv). Scoping the day-to-day operator down to this list
// instead of a personal admin key follows the same least-privilege logic as
// jmsd's own role.
//
// SystemRole/SystemRoleBinding are Django proxy models (rbac/models/role.py,
// rbac/models/rolebinding.py: `proxy = True`, no default_permissions
// override), so they carry their own add/view/delete_systemrole(binding)
// permission codenames -- not the base Role/RoleBinding ones.
var operatorPermissionCodenames = []string{
	"add_user", "change_user", "delete_user", "view_user",
	"view_permission",
	"view_systemrole", "add_systemrole",
	"view_systemrolebinding", "add_systemrolebinding", "delete_systemrolebinding",
}

type paginated[T any] struct {
	Results []T `json:"results"`
}

type permissionItem struct {
	ID       string `json:"id"`
	Codename string `json:"codename"`
}

type roleItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type bindingItem struct {
	ID   string `json:"id"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Role string `json:"role"`
}

// doSetupRole creates (or reuses) a system-scope role granting exactly
// permissionCodenames, and binds it to username in place of whatever
// system role(s) it currently has.
func doSetupRole(env *adminEnv, username, roleName string, permissionCodenames []string) error {
	permIDs := make([]string, 0, len(permissionCodenames))
	for _, codename := range permissionCodenames {
		id, err := findPermissionID(env, codename)
		if err != nil {
			return err
		}
		permIDs = append(permIDs, id)
	}

	roleID, err := findOrCreateRole(env, roleName, permIDs)
	if err != nil {
		return err
	}

	userID, err := findUserID(env, username)
	if err != nil {
		return err
	}

	bindings, err := listSystemRoleBindings(env, userID)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if b.Role == roleID {
			fmt.Printf("%s is already bound to role %s, nothing to do\n", username, roleName)
			return nil
		}
	}

	if err := createSystemRoleBinding(env, userID, roleID); err != nil {
		return fmt.Errorf("bind new role: %w", err)
	}
	for _, b := range bindings {
		if err := deleteSystemRoleBinding(env, b.ID); err != nil {
			return fmt.Errorf("remove old role binding %s: %w", b.ID, err)
		}
	}
	fmt.Printf("%s now bound to role %s (%d old binding(s) removed)\n", username, roleName, len(bindings))
	return nil
}

func findPermissionID(env *adminEnv, codename string) (string, error) {
	var res paginated[permissionItem]
	_, err := env.core.Call(http.MethodGet, "/api/v1/rbac/permissions/", nil, &res,
		map[string]string{"codename": codename})
	if err != nil {
		return "", fmt.Errorf("look up permission %s: %w", codename, err)
	}
	for _, p := range res.Results {
		if p.Codename == codename {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("permission %s not found", codename)
}

func findOrCreateRole(env *adminEnv, name string, permissionIDs []string) (string, error) {
	var res paginated[roleItem]
	_, err := env.core.Call(http.MethodGet, "/api/v1/rbac/system-roles/", nil, &res,
		map[string]string{"name": name})
	if err != nil {
		return "", fmt.Errorf("look up role %s: %w", name, err)
	}
	for _, r := range res.Results {
		if r.Name == name {
			return r.ID, nil
		}
	}

	body := map[string]any{
		"name":        name,
		"scope":       "system",
		"comment":     "jmsd-admin least-privilege role, see docs/PLAN.md §7.1",
		"permissions": permissionIDs,
	}
	var created roleItem
	_, err = env.core.Call(http.MethodPost, "/api/v1/rbac/system-roles/", body, &created)
	if err != nil {
		return "", fmt.Errorf("create role %s: %w", name, err)
	}
	return created.ID, nil
}

func listSystemRoleBindings(env *adminEnv, userID string) ([]bindingItem, error) {
	var res paginated[bindingItem]
	_, err := env.core.Call(http.MethodGet, "/api/v1/rbac/system-role-bindings/", nil, &res,
		map[string]string{"user": userID})
	if err != nil {
		return nil, fmt.Errorf("list role bindings: %w", err)
	}
	return res.Results, nil
}

func createSystemRoleBinding(env *adminEnv, userID, roleID string) error {
	body := map[string]string{"user": userID, "role": roleID}
	_, err := env.core.Call(http.MethodPost, "/api/v1/rbac/system-role-bindings/", body, nil)
	return err
}

func deleteSystemRoleBinding(env *adminEnv, bindingID string) error {
	// JumpServer refuses to delete a user's last remaining system role
	// (rbac/api/rolebinding.py: SystemRoleBindingViewSet.perform_destroy),
	// which is exactly why createSystemRoleBinding above must run first.
	_, err := env.core.Call(http.MethodDelete, "/api/v1/rbac/system-role-bindings/"+bindingID+"/", nil, nil)
	return err
}
