package main

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"

	"github.com/ammonode/daemon/internal/config"
)

// ensureUser creates the unprivileged system user that owns server files.
func ensureUser(cfg *config.Configuration) error {
	u, err := user.Lookup(cfg.System.Username)
	if err != nil {
		if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", cfg.System.Username).CombinedOutput(); err != nil {
			return fmt.Errorf("create user %s: %v: %s", cfg.System.Username, err, out)
		}
		if u, err = user.Lookup(cfg.System.Username); err != nil {
			return err
		}
	}
	cfg.System.User.UID, _ = strconv.Atoi(u.Uid)
	cfg.System.User.GID, _ = strconv.Atoi(u.Gid)
	return nil
}
