// Package docker wraps the Docker Engine API for running game server containers.
package docker

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/ammonode/daemon/internal/config"
)

var (
	once sync.Once
	cli  *client.Client
	err  error
)

// Client returns a shared Docker client.
func Client() (*client.Client, error) {
	once.Do(func() {
		cli, err = client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	})
	return cli, err
}

// EnsureNetwork creates the bridge network servers are attached to.
func EnsureNetwork(ctx context.Context, cfg *config.Configuration) error {
	c, err := Client()
	if err != nil {
		return err
	}
	name := cfg.Docker.Network.Name
	if _, err := c.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
		return nil
	}
	subnet, err := netip.ParsePrefix(cfg.Docker.Network.Subnet)
	if err != nil {
		return fmt.Errorf("docker.network.subnet: %w", err)
	}
	gateway, err := netip.ParseAddr(cfg.Docker.Network.Gateway)
	if err != nil {
		return fmt.Errorf("docker.network.gateway: %w", err)
	}
	_, err = c.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: cfg.Docker.Network.Driver,
		IPAM:   &network.IPAM{Driver: "default", Config: []network.IPAMConfig{{Subnet: subnet, Gateway: gateway}}},
		Options: map[string]string{
			"encryption": "false",
			"com.docker.network.bridge.default_bridge":       "false",
			"com.docker.network.bridge.enable_icc":           "true",
			"com.docker.network.bridge.enable_ip_masquerade": "true",
			"com.docker.network.bridge.host_binding_ipv4":    "0.0.0.0",
			"com.docker.network.bridge.name":                 "ammonode0",
			"com.docker.network.driver.mtu":                  strconv.Itoa(cfg.Docker.Network.MTU),
		},
	})
	return err
}

// PullImage pulls an image, streaming progress lines to out (may be nil).
func PullImage(ctx context.Context, image string, out func(string)) error {
	c, err := Client()
	if err != nil {
		return err
	}
	res, err := c.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		// Fall back to a local image if the registry is unreachable.
		if _, ierr := c.ImageInspect(ctx, image); ierr == nil {
			return nil
		}
		return err
	}
	defer res.Close()
	for msg, err := range res.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if out != nil && msg.Status != "" && msg.Progress == nil {
			out(strings.TrimSpace(msg.ID + " " + msg.Status))
		}
	}
	return nil
}

// Resources converts panel limits into Docker resource settings.
func Resources(cfg *config.Configuration, memoryMiB, swapMiB, cpuPercent int64, threads string, ioWeight uint16, oomDisabled bool) container.Resources {
	mem := memoryMiB * 1024 * 1024
	limit := mem + cfg.Docker.Overhead*1024*1024
	swap := int64(-1)
	if swapMiB >= 0 {
		swap = limit + swapMiB*1024*1024
	}
	if memoryMiB == 0 {
		limit, swap = 0, 0
	}
	pids := cfg.Docker.ContainerPidLimit
	r := container.Resources{
		Memory:            limit,
		MemoryReservation: mem,
		MemorySwap:        swap,
		CpusetCpus:        threads,
		PidsLimit:         &pids,
		OomKillDisable:    &oomDisabled,
		BlkioWeight:       ioWeight,
	}
	if cpuPercent > 0 {
		r.CPUQuota = cpuPercent * 1000
		r.CPUPeriod = 100000
		r.CPUShares = 1024
	}
	return r
}

// PortBindings builds exposed ports and bindings for every allocation.
func PortBindings(mappings map[string][]int) (network.PortSet, network.PortMap) {
	exposed := network.PortSet{}
	bindings := network.PortMap{}
	for ip, ports := range mappings {
		addr, _ := netip.ParseAddr(ip)
		for _, p := range ports {
			for _, proto := range []string{"tcp", "udp"} {
				port, err := network.ParsePort(fmt.Sprintf("%d/%s", p, proto))
				if err != nil {
					continue
				}
				exposed[port] = struct{}{}
				bindings[port] = append(bindings[port], network.PortBinding{HostIP: addr, HostPort: strconv.Itoa(p)})
			}
		}
	}
	return exposed, bindings
}

// Bind returns a bind mount.
func Bind(source, target string, readOnly bool) mount.Mount {
	return mount.Mount{Type: mount.TypeBind, Source: source, Target: target, ReadOnly: readOnly}
}

// Demux reads a multiplexed docker stream (non-TTY) and writes payloads to w.
func Demux(r io.Reader, w io.Writer) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		if _, err := io.CopyN(w, r, size); err != nil {
			return err
		}
	}
}
