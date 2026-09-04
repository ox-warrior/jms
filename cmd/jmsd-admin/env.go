package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"

	"github.com/jumpserver-dev/sdk-go/service"
)

// adminEnv is the Core client every subcommand calls through, authenticated
// as whichever admin identity the operator points it at via env vars.
type adminEnv struct {
	core     *service.JMService
	coreHost string
}

func loadAdminEnv() (*adminEnv, error) {
	coreHost := os.Getenv("JMSADMIN_CORE_HOST")
	akID := os.Getenv("JMSADMIN_ACCESS_KEY_ID")
	akSecret := os.Getenv("JMSADMIN_ACCESS_KEY_SECRET")
	if coreHost == "" || akID == "" || akSecret == "" {
		return nil, errors.New("JMSADMIN_CORE_HOST, JMSADMIN_ACCESS_KEY_ID and JMSADMIN_ACCESS_KEY_SECRET are required " +
			"(an admin-capable Access Key -- see docs/PLAN.md §7.1 for why every command here needs is_admin()-level " +
			"access the first time, even setup-operator-role)")
	}
	core, err := service.NewAuthJMService(
		service.JMSCoreHost(coreHost),
		service.JMSAccessKey(akID, akSecret),
	)
	if err != nil {
		return nil, err
	}
	return &adminEnv{core: core, coreHost: coreHost}, nil
}

func randomSecret(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
