package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ammonode/daemon/internal/remote"
)

// Manager holds every server running on this node.
type Manager struct {
	mu      sync.RWMutex
	servers map[string]*Server
	panel   *remote.Client
	stop    chan struct{}
}

func NewManager(panel *remote.Client) *Manager {
	return &Manager{servers: map[string]*Server{}, panel: panel, stop: make(chan struct{})}
}

func (m *Manager) Panel() *remote.Client { return m.panel }

// Boot loads all servers assigned to this node from the panel.
func (m *Manager) Boot(ctx context.Context) error {
	list, err := m.panel.Servers(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range list {
		s := m.Add(cfg)
		if err := s.Reattach(ctx); err != nil {
			slog.Warn("could not reattach to server", "server", cfg.UUID, "error", err)
		}
	}
	slog.Info("loaded servers from panel", "count", len(list))
	return nil
}

func (m *Manager) Add(cfg remote.ServerConfiguration) *Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.servers[cfg.UUID]; ok {
		s.SetConfig(cfg)
		return s
	}
	s := New(cfg, m.panel)
	m.servers[cfg.UUID] = s
	go s.UpdateDiskUsage()
	go s.DiskLoop(150*time.Second, m.stop)
	return s
}

func (m *Manager) Get(uuid string) (*Server, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.servers[uuid]
	return s, ok
}

func (m *Manager) Remove(uuid string) {
	m.mu.Lock()
	delete(m.servers, uuid)
	m.mu.Unlock()
}

func (m *Manager) All() []*Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s)
	}
	return out
}

// Shutdown stops background loops; containers keep running.
func (m *Manager) Shutdown() { close(m.stop) }
