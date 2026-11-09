package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/docker"
)

// Install runs the egg installation script in a temporary container, exactly
// like Pterodactyl: the server files are mounted at /mnt/server and the script
// at /mnt/install/install.sh.
func (s *Server) Install(ctx context.Context, reinstall bool) error {
	s.mu.Lock()
	if s.installing {
		s.mu.Unlock()
		return ErrInstalling
	}
	s.installing = true
	ctx, cancel := context.WithCancel(ctx)
	s.cancelInstall = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.installing = false
		s.cancelInstall = nil
		s.mu.Unlock()
	}()

	if reinstall {
		if err := s.WaitForStop(ctx, 60*time.Second, true); err != nil {
			return err
		}
	}

	err := s.runInstall(ctx)
	if ctx.Err() != nil {
		// Aborted because the server was deleted: nothing to report.
		return ctx.Err()
	}
	s.Events.Publish(EventInstallDone)
	if rerr := s.panel.SetInstallStatus(context.Background(), s.ID(), err == nil, reinstall); rerr != nil {
		s.log.Error("failed to report install status", "error", rerr)
	}
	if err != nil {
		s.daemonMessage("Installation failed: " + err.Error())
	}
	return err
}

func (s *Server) runInstall(ctx context.Context) error {
	cfg := config.Get()
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		return err
	}
	_ = os.Chown(s.Dir(), cfg.System.User.UID, cfg.System.User.GID)
	if s.Config().SkipScripts {
		return nil
	}
	script, err := s.panel.InstallScript(ctx, s.ID())
	if err != nil {
		return fmt.Errorf("fetch install script: %w", err)
	}
	if strings.TrimSpace(script.Script) == "" {
		return nil
	}
	s.Events.Publish(EventInstallStarted)

	tmp := filepath.Join(cfg.System.TmpDirectory, s.ID())
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	body := strings.ReplaceAll(script.Script, "\r\n", "\n")
	if err := os.WriteFile(filepath.Join(tmp, "install.sh"), []byte(body), 0o644); err != nil {
		return err
	}

	cli, err := docker.Client()
	if err != nil {
		return err
	}
	out := func(line string) {
		s.Logs.Add(line)
		s.Events.Publish(EventInstallOutput, line)
	}
	out("[Ammonode Daemon]: Pulling installation container image " + script.ContainerImage)
	if err := docker.PullImage(ctx, script.ContainerImage, out); err != nil {
		return fmt.Errorf("pull installer image: %w", err)
	}

	name := s.ID() + "_installer"
	_, _ = cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	entry := script.Entrypoint
	if entry == "" {
		entry = "ash"
	}
	c := s.Config()
	mem := cfg.Docker.InstallerLimits.Memory
	if c.Build.MemoryLimit > mem {
		mem = c.Build.MemoryLimit
	}
	cpu := cfg.Docker.InstallerLimits.CPU
	if c.Build.CPULimit > cpu {
		cpu = c.Build.CPULimit
	}
	_, err = cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Hostname:     "installer",
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
			Image:        script.ContainerImage,
			Env:          s.environment(),
			Cmd:          []string{entry, "/mnt/install/install.sh"},
			Labels:       map[string]string{"Service": "Ammonode", "ContainerType": "server_installer"},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{
				docker.Bind(s.Dir(), "/mnt/server", false),
				docker.Bind(tmp, "/mnt/install", false),
			},
			Tmpfs:       map[string]string{"/tmp": "rw,exec,nosuid,size=" + strconv.Itoa(cfg.Docker.TmpfsSize) + "M"},
			Resources:   docker.Resources(cfg, mem, -1, cpu, "", 500, false),
			DNS:         parseAddrs(cfg.Docker.DNS),
			NetworkMode: container.NetworkMode(cfg.Docker.Network.Name),
		},
	})
	if err != nil {
		return fmt.Errorf("create installer: %w", err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()

	attach, err := cli.ContainerAttach(ctx, name, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	defer attach.Close()
	if _, err := cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start installer: %w", err)
	}

	logPath := filepath.Join(cfg.System.LogDirectory, "install", s.ID()+".log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, _ := os.Create(logPath)
	if logFile != nil {
		defer logFile.Close()
	}
	sc := bufio.NewScanner(attach.Reader)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		out(line)
		if logFile != nil {
			fmt.Fprintln(logFile, line)
		}
	}

	wait := cli.ContainerWait(ctx, name, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return fmt.Errorf("install script exited with code %d", res.StatusCode)
		}
	case err := <-wait.Error:
		return err
	}
	// Give the files to the unprivileged user the server runs as.
	_ = filepath.Walk(s.Dir(), func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Lchown(p, cfg.System.User.UID, cfg.System.User.GID)
		}
		return nil
	})
	s.UpdateDiskUsage()
	return nil
}
