// Package system reports information about the host machine.
package system

import (
	"context"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/moby/moby/client"

	"github.com/ammonode/daemon/internal/docker"
)

// Version is set at build time with -ldflags "-X .../system.Version=1.0.0".
var Version = "develop"

type Information struct {
	Version       string `json:"version"`
	KernelVersion string `json:"kernel_version"`
	Architecture  string `json:"architecture"`
	OS            string `json:"os"`
	CPUCount      int    `json:"cpu_count"`
	MemoryBytes   uint64 `json:"memory_bytes"`
	DockerVersion string `json:"docker_version"`
	Containers    int    `json:"containers"`
}

func Info(ctx context.Context) (Information, error) {
	info := Information{Version: Version, Architecture: runtime.GOARCH, OS: runtime.GOOS, CPUCount: runtime.NumCPU()}
	var u syscall.Utsname
	if syscall.Uname(&u) == nil {
		var b strings.Builder
		for _, c := range u.Release {
			if c == 0 {
				break
			}
			b.WriteByte(byte(c))
		}
		info.KernelVersion = b.String()
	}
	var si syscall.Sysinfo_t
	if syscall.Sysinfo(&si) == nil {
		info.MemoryBytes = uint64(si.Totalram) * uint64(si.Unit)
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				info.OS = strings.Trim(v, `"`)
			}
		}
	}
	if cli, err := docker.Client(); err == nil {
		if v, err := cli.ServerVersion(ctx, client.ServerVersionOptions{}); err == nil {
			info.DockerVersion = v.Version
		}
		if res, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true}); err == nil {
			info.Containers = len(res.Items)
		}
	}
	return info, nil
}
