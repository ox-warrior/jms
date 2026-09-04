// Command jmsd-admin is a one-off operator tool for the JumpServer-side
// setup described in docs/PLAN.md §7.1: creating jmsd's dedicated JumpServer
// account, locking it down to API-only use, scoping its role to the minimum
// permission set, and cleaning up on rotation. It is not part of the
// jms/jmsd runtime and is never deployed alongside them.
//
// jmsd's identity is a plain user, not a JumpServer "service account" --
// see docs/PLAN.md §12 for why: the service-account-registrations endpoint
// has no public documentation and no internal caller anywhere in Koko or
// sdk-go, so this avoids depending on it. The tradeoff is that minting the
// Access Key can't be done by an admin on the user's behalf (JumpServer has
// no such endpoint); it needs one manual browser step, described by
// `create-user`'s output.
//
// Credentials come from the environment, never from flags, so nothing ends
// up in shell history or `ps`:
//
//	JMSADMIN_CORE_HOST           JumpServer base URL
//	JMSADMIN_ACCESS_KEY_ID       your own personal admin Access Key (SigAuth)
//	JMSADMIN_ACCESS_KEY_SECRET   -- not jmsd's -- used for every call below.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "jmsd-admin",
		Short:         "One-off setup for jmsd's JumpServer account (see docs/PLAN.md §7.1)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newCreateUserCmd(), newLockAccountCmd(), newSetupRoleCmd(), newSetupOperatorRoleCmd(), newDeleteUserCmd())
	return cmd
}

func newCreateUserCmd() *cobra.Command {
	var username, name, email, comment string
	cmd := &cobra.Command{
		Use:   "create-user",
		Short: "Step 1: create a plain user for jmsd with a one-time temporary password",
		RunE: func(_ *cobra.Command, _ []string) error {
			env, err := loadAdminEnv()
			if err != nil {
				return err
			}
			user, tempPassword, err := doCreateUser(env, username, name, email, comment)
			if err != nil {
				return err
			}
			fmt.Printf(`Created user %q (id=%s).

Next, by hand (this can't be scripted -- see the package comment in main.go):
  1. Log into %s as:
       username: %s
       password: %s
  2. Go to your profile's API Keys page and create one. Copy its id and secret.
  3. Come back and run:
       jmsd-admin lock-account --username %s
     This invalidates the password above so the account can never log in
     again -- the Access Key you just created keeps working.
  4. Run:
       jmsd-admin setup-role --username %s
     to scope it down to the minimum permission set.

Then put the Access Key you copied in step 2 into jmsd's config file
(access_key_id / access_key_secret, see docs/PLAN.md §7.2) -- default path
/etc/jmsd/config.yaml.
`, username, user.ID, env.coreHost, username, tempPassword, username, username)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "jmsd", "account username")
	cmd.Flags().StringVar(&name, "name", "jmsd", "account display name")
	cmd.Flags().StringVar(&email, "email", "", "account email (required, must be unique)")
	cmd.Flags().StringVar(&comment, "comment", "CLI control service -- API-only, see docs/PLAN.md §7.1", "account comment")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func newLockAccountCmd() *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "lock-account",
		Short: "Step 3: overwrite the account's password with an unknown random value, so it can no longer log in",
		RunE: func(_ *cobra.Command, _ []string) error {
			env, err := loadAdminEnv()
			if err != nil {
				return err
			}
			if err := doLockAccount(env, username); err != nil {
				return err
			}
			fmt.Printf("%s locked: password replaced, existing Access Keys are unaffected\n", username)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "jmsd", "account username to lock")
	return cmd
}

func newSetupRoleCmd() *cobra.Command {
	var username, roleName string
	cmd := &cobra.Command{
		Use:   "setup-role",
		Short: "Step 4: create jmsd's least-privilege system role and bind it, replacing the account's current system role(s)",
		RunE: func(_ *cobra.Command, _ []string) error {
			env, err := loadAdminEnv()
			if err != nil {
				return err
			}
			return doSetupRole(env, username, roleName, jmsdPermissionCodenames)
		},
	}
	cmd.Flags().StringVar(&username, "username", "jmsd", "account username to reassign")
	cmd.Flags().StringVar(&roleName, "role-name", "jmsd-minimal", "custom role name (created if missing, reused if present)")
	return cmd
}

// newSetupOperatorRoleCmd scopes down whichever account you'll use to run
// jmsd-admin day to day, instead of reusing a personal admin key forever.
// Bootstrapping it still needs an admin-capable key the first time (see
// loadAdminEnv) -- creating a role and binding it requires exactly the
// permissions this command is trying to hand out.
func newSetupOperatorRoleCmd() *cobra.Command {
	var username, roleName string
	cmd := &cobra.Command{
		Use:   "setup-operator-role",
		Short: "Optional hardening: scope an account down to exactly what's needed to run the other jmsd-admin commands",
		RunE: func(_ *cobra.Command, _ []string) error {
			env, err := loadAdminEnv()
			if err != nil {
				return err
			}
			return doSetupRole(env, username, roleName, operatorPermissionCodenames)
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "account username to reassign (create it first with create-user)")
	cmd.Flags().StringVar(&roleName, "role-name", "jmsd-operator-minimal", "custom role name (created if missing, reused if present)")
	_ = cmd.MarkFlagRequired("username")
	return cmd
}

func newDeleteUserCmd() *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "delete-user",
		Short: "Rotation cleanup: delete an old jmsd account after a replacement is confirmed working",
		RunE: func(_ *cobra.Command, _ []string) error {
			env, err := loadAdminEnv()
			if err != nil {
				return err
			}
			if err := doDeleteUser(env, username); err != nil {
				return err
			}
			fmt.Printf("%s deleted\n", username)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "account username to delete")
	_ = cmd.MarkFlagRequired("username")
	return cmd
}
