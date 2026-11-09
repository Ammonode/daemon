// Package server manages the lifecycle of a single game server container.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/docker"
	"github.com/ammonode/daemon/internal/remote"
)

const (
	StateOffline  = "offline"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
)

var ErrSuspended = errors.New("server is suspended")
var ErrInstalling = errors.New("server is installing")

type Stats struct {
	MemoryBytes      uint64  `json:"memory_bytes"`
	MemoryLimitBytes uint64  `json:"memory_limit_bytes"`
	CPUAbsolute      float64 `json:"cpu_absolute"`
	Network          struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"network"`
	Uptime    int64  `json:"uptime"`
	State     string `json:"state"`
	DiskBytes int64  `json:"disk_bytes"`
}

type Server struct {
	mu     sync.RWMutex
	cfg    remote.ServerConfiguration
	state  string
	Events *Bus
	Logs   *RingBuffer

	installing  bool
	stdin       io.WriteCloser
	attachClose func()
	startedAt   time.Time
	lastCrash   time.Time
	stopping    bool // user asked to stop, so an exit is not a crash
	stats       Stats
	powerLock   sync.Mutex
	diskUsage   int64

	panel *remote.Client
	log   *slog.Logger
}

func New(cfg remote.ServerConfiguration, panel *remote.Client) *Server {
	return &Server{
		cfg:    cfg,
		state:  StateOffline,
		Events: NewBus(),
		Logs:   NewRingBuffer(150),
		panel:  panel,
		log:    slog.With("server", cfg.UUID),
	}
}

func (s *Server) ID() string { return s.cfg.UUID }

func (s *Server) Config() remote.ServerConfiguration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) SetConfig(cfg remote.ServerConfiguration) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

func (s *Server) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Server) IsInstalling() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.installing
}

func (s *Server) setState(state string) {
	s.mu.Lock()
	changed := s.state != state
	s.state = state
	s.stats.State = state
	s.mu.Unlock()
	if changed {
		s.Events.Publish(EventStatus, state)
	}
}

// Dir is the server's data directory on the host.
func (s *Server) Dir() string {
	return filepath.Join(config.Get().System.Data, s.cfg.UUID)
}

func (s *Server) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.stats
	st.DiskBytes = s.diskUsage
	if s.state == StateRunning || s.state == StateStarting {
		st.Uptime = time.Since(s.startedAt).Milliseconds()
	}
	return st
}

func (s *Server) daemonMessage(msg string) {
	s.Events.Publish(EventDaemonMessage, msg)
	s.Logs.Add("[Ammonode Daemon]: " + msg)
}

func (s *Server) environment() []string {
	c := s.Config()
	cfg := config.Get()
	env := map[string]string{
		"TZ":                        cfg.System.Timezone,
		"STARTUP":                   c.Invocation,
		"SERVER_MEMORY":             strconv.FormatInt(c.Build.MemoryLimit, 10),
		"SERVER_IP":                 c.Allocations.Default.IP,
		"SERVER_PORT":               strconv.Itoa(c.Allocations.Default.Port),
		"P_SERVER_UUID":             c.UUID,
		"P_SERVER_ALLOCATION_LIMIT": "0",
	}
	for k, v := range c.Environment {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// containerName is the docker container name used for the server.
func (s *Server) containerName() string { return s.cfg.UUID }

// createContainer (re)creates the server container with the current configuration.
func (s *Server) createContainer(ctx context.Context) error {
	cli, err := docker.Client()
	if err != nil {
		return err
	}
	c := s.Config()
	cfg := config.Get()
	_, _ = cli.ContainerRemove(ctx, s.containerName(), client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})

	s.daemonMessage("Pulling Docker container image, this could take a few minutes to complete...")
	if err := docker.PullImage(ctx, c.Container.Image, nil); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	s.daemonMessage("Finished pulling Docker container image")

	exposed, bindings := docker.PortBindings(c.Allocations.Mappings)
	mounts := []mount.Mount{docker.Bind(s.Dir(), "/home/container", false)}
	for _, m := range c.Mounts {
		mounts = append(mounts, docker.Bind(m.Source, m.Target, m.ReadOnly))
	}
	labels := map[string]string{"Service": "Ammonode", "ContainerType": "server_process"}
	for k, v := range c.Labels {
		labels[k] = v
	}
	user := strconv.Itoa(cfg.System.User.UID) + ":" + strconv.Itoa(cfg.System.User.GID)

	_, err = cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: s.containerName(),
		Config: &container.Config{
			Hostname:     c.UUID,
			Domainname:   "",
			User:         user,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			Tty:          true,
			ExposedPorts: exposed,
			Image:        c.Container.Image,
			Env:          s.environment(),
			Labels:       labels,
		},
		HostConfig: &container.HostConfig{
			PortBindings:   bindings,
			Mounts:         mounts,
			Tmpfs:          map[string]string{"/tmp": "rw,exec,nosuid,size=" + strconv.Itoa(cfg.Docker.TmpfsSize) + "M"},
			Resources:      docker.Resources(cfg, c.Build.MemoryLimit, c.Build.Swap, c.Build.CPULimit, c.Build.Threads, c.Build.IOWeight, c.Build.OOMDisabled),
			DNS:            parseAddrs(cfg.Docker.DNS),
			LogConfig:      container.LogConfig{Type: "local", Config: map[string]string{"max-size": "5m", "max-file": "1", "compress": "false"}},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			CapDrop: []string{
				"setpcap", "mknod", "audit_write", "net_raw", "dac_override",
				"fowner", "fsetid", "net_bind_service", "sys_chroot", "setfcap",
			},
			NetworkMode: container.NetworkMode(cfg.Docker.Network.Name),
		},
	})
	return err
}

// Start boots the server: syncs config, rewrites config files, creates and attaches the container.
func (s *Server) Start(ctx context.Context) error {
	s.powerLock.Lock()
	defer s.powerLock.Unlock()
	if s.Config().Suspended {
		return ErrSuspended
	}
	if s.IsInstalling() {
		return ErrInstalling
	}
	if st := s.State(); st != StateOffline {
		return fmt.Errorf("server is already %s", st)
	}
	s.setState(StateStarting)
	if cfg, err := s.panel.Server(ctx, s.ID()); err == nil {
		s.SetConfig(cfg)
	} else {
		s.log.Warn("could not refresh configuration from panel", "error", err)
	}
	if err := s.checkDisk(); err != nil {
		s.setState(StateOffline)
		return err
	}
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		s.setState(StateOffline)
		return err
	}
	s.daemonMessage("Updating process configuration files...")
	if err := ApplyConfigFiles(s.Dir(), s.Config()); err != nil {
		s.daemonMessage("Failed to update a configuration file: " + err.Error())
	}
	if err := s.createContainer(ctx); err != nil {
		s.setState(StateOffline)
		return err
	}
	if err := s.attach(ctx); err != nil {
		s.setState(StateOffline)
		return err
	}
	cli, _ := docker.Client()
	if _, err := cli.ContainerStart(ctx, s.containerName(), client.ContainerStartOptions{}); err != nil {
		s.setState(StateOffline)
		return err
	}
	s.mu.Lock()
	s.startedAt = time.Now()
	s.stopping = false
	s.mu.Unlock()
	go s.pollStats()
	return nil
}

func (s *Server) attach(ctx context.Context) error {
	cli, err := docker.Client()
	if err != nil {
		return err
	}
	res, err := cli.ContainerAttach(ctx, s.containerName(), client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.stdin = res.Conn
	s.attachClose = res.Close
	s.mu.Unlock()

	c := s.Config()
	var done []*regexp.Regexp
	var doneLiteral []string
	for _, d := range c.Process.Startup.Done {
		if strings.HasPrefix(d, "regex:") {
			if re, err := regexp.Compile(strings.TrimPrefix(d, "regex:")); err == nil {
				done = append(done, re)
			}
		} else if d != "" {
			doneLiteral = append(doneLiteral, d)
		}
	}

	go func() {
		defer res.Close()
		scanner := bufio.NewScanner(res.Reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			s.Logs.Add(line)
			s.Events.Publish(EventConsoleOutput, line)
			if s.State() == StateStarting {
				for _, d := range doneLiteral {
					if strings.Contains(line, d) {
						s.setState(StateRunning)
					}
				}
				for _, re := range done {
					if re.MatchString(line) {
						s.setState(StateRunning)
					}
				}
			}
		}
		s.onExit()
	}()
	return nil
}

// onExit runs when the attached stream closes, i.e. the process stopped.
func (s *Server) onExit() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli, _ := docker.Client()
	exitCode := 0
	oom := false
	if res, err := cli.ContainerInspect(ctx, s.containerName(), client.ContainerInspectOptions{}); err == nil && res.Container.State != nil {
		exitCode = res.Container.State.ExitCode
		oom = res.Container.State.OOMKilled
	}
	s.mu.Lock()
	expected := s.stopping
	s.stdin = nil
	s.stats = Stats{State: StateOffline}
	s.mu.Unlock()
	s.setState(StateOffline)

	cfg := config.Get()
	if expected || !cfg.System.CrashDetection.Enabled || !s.Config().CrashRestart {
		return
	}
	if exitCode == 0 && !oom {
		return
	}
	s.daemonMessage("---------- Detected server process in a crashed state! ----------")
	s.daemonMessage(fmt.Sprintf("Exit code: %d", exitCode))
	s.daemonMessage(fmt.Sprintf("Out of memory: %t", oom))
	s.mu.Lock()
	recent := time.Since(s.lastCrash) < time.Duration(cfg.System.CrashDetection.Timeout)*time.Second
	s.lastCrash = time.Now()
	s.mu.Unlock()
	if recent {
		s.daemonMessage("Aborting automatic restart, last crash occurred less than " + strconv.Itoa(cfg.System.CrashDetection.Timeout) + " seconds ago.")
		return
	}
	if err := s.Start(context.Background()); err != nil {
		s.daemonMessage("Automatic restart failed: " + err.Error())
	}
}

// SendCommand writes a line to the server's stdin.
func (s *Server) SendCommand(cmd string) error {
	s.mu.RLock()
	w := s.stdin
	s.mu.RUnlock()
	if w == nil || s.State() == StateOffline {
		return errors.New("server is not running")
	}
	_, err := w.Write([]byte(cmd + "\n"))
	return err
}

// Stop asks the process to stop gracefully using the egg's stop configuration.
func (s *Server) Stop(ctx context.Context) error {
	if s.State() == StateOffline {
		return nil
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.setState(StateStopping)
	stop := s.Config().Process.Stop
	cli, err := docker.Client()
	if err != nil {
		return err
	}
	switch {
	case stop.Type == "command" || (stop.Type == "" && stop.Value != "" && stop.Value != "^C"):
		return s.SendCommand(stop.Value)
	case stop.Value == "^C" || stop.Type == "signal":
		sig := "SIGINT"
		if stop.Type == "signal" && stop.Value != "^C" {
			sig = stop.Value
		}
		_, err = cli.ContainerKill(ctx, s.containerName(), client.ContainerKillOptions{Signal: sig})
	default:
		timeout := 30
		_, err = cli.ContainerStop(ctx, s.containerName(), client.ContainerStopOptions{Timeout: &timeout})
	}
	return err
}

// WaitForStop stops the server and kills it if it has not exited after timeout.
func (s *Server) WaitForStop(ctx context.Context, timeout time.Duration, kill bool) error {
	if s.State() == StateOffline {
		return nil
	}
	if err := s.Stop(ctx); err != nil {
		return err
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if s.State() == StateOffline {
				return nil
			}
		case <-deadline:
			if kill {
				return s.Kill(ctx)
			}
			return errors.New("timed out waiting for the server to stop")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) Kill(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	cli, err := docker.Client()
	if err != nil {
		return err
	}
	_, err = cli.ContainerKill(ctx, s.containerName(), client.ContainerKillOptions{Signal: "SIGKILL"})
	if err != nil && strings.Contains(err.Error(), "is not running") {
		s.setState(StateOffline)
		return nil
	}
	return err
}

func (s *Server) Restart(ctx context.Context) error {
	if err := s.WaitForStop(ctx, 60*time.Second, true); err != nil {
		return err
	}
	return s.Start(ctx)
}

// Destroy removes the container and all files.
func (s *Server) Destroy(ctx context.Context) error {
	_ = s.Kill(ctx)
	cli, err := docker.Client()
	if err == nil {
		_, _ = cli.ContainerRemove(ctx, s.containerName(), client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}
	s.Events.Publish(EventDeleted)
	return os.RemoveAll(s.Dir())
}

func (s *Server) pollStats() {
	cli, err := docker.Client()
	if err != nil {
		return
	}
	res, err := cli.ContainerStats(context.Background(), s.containerName(), client.ContainerStatsOptions{Stream: true})
	if err != nil {
		return
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var v container.StatsResponse
		if err := dec.Decode(&v); err != nil {
			return
		}
		if s.State() == StateOffline {
			return
		}
		st := Stats{MemoryBytes: v.MemoryStats.Usage, MemoryLimitBytes: v.MemoryStats.Limit}
		if cache, ok := v.MemoryStats.Stats["inactive_file"]; ok && cache < st.MemoryBytes {
			st.MemoryBytes -= cache
		}
		cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
		sysDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
		cpus := float64(v.CPUStats.OnlineCPUs)
		if sysDelta > 0 && cpuDelta > 0 {
			st.CPUAbsolute = (cpuDelta / sysDelta) * cpus * 100
		}
		for _, n := range v.Networks {
			st.Network.RxBytes += n.RxBytes
			st.Network.TxBytes += n.TxBytes
		}
		s.mu.Lock()
		st.State = s.state
		s.stats = st
		s.mu.Unlock()
		full := s.Stats()
		b, _ := json.Marshal(full)
		s.Events.Publish(EventStats, string(b))
	}
}
