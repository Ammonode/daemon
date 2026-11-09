// Package config loads the daemon configuration file written by
// `ammonode-daemon configure` (normally /etc/ammonode/config.yml).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "/etc/ammonode/config.yml"

type Configuration struct {
	Debug   bool   `yaml:"debug"`
	UUID    string `yaml:"uuid"`
	TokenID string `yaml:"token_id"`
	Token   string `yaml:"token"`
	// Base URL of the panel, e.g. https://panel.example.com
	Remote string `yaml:"remote"`

	API struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		SSL  struct {
			Enabled bool   `yaml:"enabled"`
			Cert    string `yaml:"cert"`
			Key     string `yaml:"key"`
		} `yaml:"ssl"`
		// Maximum upload size in MiB.
		UploadLimit int `yaml:"upload_limit"`
		// Origins allowed to open websocket connections, besides the panel.
		TrustedOrigins []string `yaml:"trusted_origins"`
	} `yaml:"api"`

	System struct {
		RootDirectory string `yaml:"root_directory"`
		LogDirectory  string `yaml:"log_directory"`
		Data          string `yaml:"data"`
		ArchiveDir    string `yaml:"archive_directory"`
		BackupDir     string `yaml:"backup_directory"`
		TmpDirectory  string `yaml:"tmp_directory"`
		Username      string `yaml:"username"`
		Timezone      string `yaml:"timezone"`
		User          struct {
			UID int `yaml:"uid"`
			GID int `yaml:"gid"`
		} `yaml:"user"`
		DiskCheckInterval int `yaml:"disk_check_interval"`
		SFTP              struct {
			Address  string `yaml:"bind_address"`
			Port     int    `yaml:"bind_port"`
			ReadOnly bool   `yaml:"read_only"`
		} `yaml:"sftp"`
		// Restart a server automatically when it exits unexpectedly.
		CrashDetection struct {
			Enabled bool `yaml:"enabled"`
			// Do not restart again if the last crash was less than this many seconds ago.
			Timeout int `yaml:"timeout"`
		} `yaml:"crash_detection"`
	} `yaml:"system"`

	Docker struct {
		Network struct {
			Name      string `yaml:"name"`
			Interface string `yaml:"interface"`
			Subnet    string `yaml:"subnet"`
			Gateway   string `yaml:"gateway"`
			Driver    string `yaml:"driver"`
			MTU       int    `yaml:"mtu"`
		} `yaml:"network"`
		DNS               []string `yaml:"dns"`
		TmpfsSize         int      `yaml:"tmpfs_size"`
		ContainerPidLimit int64    `yaml:"container_pid_limit"`
		InstallerLimits   struct {
			Memory int64 `yaml:"memory"`
			CPU    int64 `yaml:"cpu"`
		} `yaml:"installer_limits"`
		// Extra memory (in MiB) granted on top of the server limit to avoid OOM kills on small servers.
		Overhead int64 `yaml:"overhead"`
	} `yaml:"docker"`

	path string
}

var (
	mu      sync.RWMutex
	current *Configuration
)

func defaults() *Configuration {
	c := &Configuration{}
	c.API.Host = "0.0.0.0"
	c.API.Port = 8080
	c.API.UploadLimit = 100
	c.System.RootDirectory = "/var/lib/ammonode"
	c.System.LogDirectory = "/var/log/ammonode"
	c.System.Data = "/var/lib/ammonode/volumes"
	c.System.ArchiveDir = "/var/lib/ammonode/archives"
	c.System.BackupDir = "/var/lib/ammonode/backups"
	c.System.TmpDirectory = "/tmp/ammonode"
	c.System.Username = "ammonode"
	c.System.Timezone = "UTC"
	c.System.DiskCheckInterval = 150
	c.System.SFTP.Address = "0.0.0.0"
	c.System.SFTP.Port = 2022
	c.System.CrashDetection.Enabled = true
	c.System.CrashDetection.Timeout = 60
	c.Docker.Network.Name = "ammonode_nw"
	c.Docker.Network.Interface = "172.18.0.1"
	c.Docker.Network.Subnet = "172.18.0.0/16"
	c.Docker.Network.Gateway = "172.18.0.1"
	c.Docker.Network.Driver = "bridge"
	c.Docker.Network.MTU = 1500
	c.Docker.DNS = []string{"1.1.1.1", "1.0.0.1"}
	c.Docker.TmpfsSize = 100
	c.Docker.ContainerPidLimit = 512
	c.Docker.InstallerLimits.Memory = 1024
	c.Docker.InstallerLimits.CPU = 100
	return c
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Configuration, error) {
	c := defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("configuration file %s not found, run `ammonode-daemon configure` first", path)
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.UUID == "" || c.Token == "" || c.TokenID == "" || c.Remote == "" {
		return nil, fmt.Errorf("%s is missing uuid, token_id, token or remote", path)
	}
	c.path = path
	Set(c)
	return c, nil
}

// Save writes the configuration back to disk with 0600 permissions.
func (c *Configuration) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// New returns a configuration populated with defaults.
func New() *Configuration { return defaults() }

func Set(c *Configuration) {
	mu.Lock()
	current = c
	mu.Unlock()
}

func Get() *Configuration {
	mu.RLock()
	defer mu.RUnlock()
	return current
}
