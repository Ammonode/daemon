// Package sftp runs the built-in SFTP server. Users log in as
// "<username>.<server id>" with their panel password or an SSH key from their account.
package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/remote"
	"github.com/ammonode/daemon/internal/server"
)

type Server struct {
	cfg     *config.Configuration
	manager *server.Manager
	panel   *remote.Client
}

func New(cfg *config.Configuration, m *server.Manager) *Server {
	return &Server{cfg: cfg, manager: m, panel: m.Panel()}
}

func (s *Server) hostKey() (ssh.Signer, error) {
	path := filepath.Join(s.cfg.System.RootDirectory, ".sftp", "id_ed25519")
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "ammonode sftp")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}

func (s *Server) authenticate(conn ssh.ConnMetadata, kind, secret string) (*ssh.Permissions, error) {
	ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := s.panel.ValidateSftp(ctx, remote.SftpAuthRequest{Type: kind, User: conn.User(), Pass: secret, IP: ip})
	if err != nil {
		slog.Info("sftp login rejected", "user", conn.User(), "ip", ip)
		return nil, errors.New("invalid credentials")
	}
	return &ssh.Permissions{Extensions: map[string]string{
		"server":      res.Server,
		"user":        res.User,
		"permissions": strings.Join(res.Permissions, ","),
	}}, nil
}

// Run listens until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	key, err := s.hostKey()
	if err != nil {
		return fmt.Errorf("sftp host key: %w", err)
	}
	conf := &ssh.ServerConfig{
		MaxAuthTries: 6,
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			return s.authenticate(c, "password", string(pass))
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			return s.authenticate(c, "public_key", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))))
		},
	}
	conf.AddHostKey(key)

	addr := net.JoinHostPort(s.cfg.System.SFTP.Address, strconv.Itoa(s.cfg.System.SFTP.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	slog.Info("SFTP listening", "address", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go s.handle(c, conf)
	}
}

func (s *Server) handle(c net.Conn, conf *ssh.ServerConfig) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	sconn, chans, reqs, err := ssh.NewServerConn(c, conf)
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	srv, ok := s.manager.Get(sconn.Permissions.Extensions["server"])
	if !ok {
		return
	}
	perms := strings.Split(sconn.Permissions.Extensions["permissions"], ",")

	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range requests {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}()
		if srv.Config().Suspended {
			ch.Close()
			continue
		}
		h := &handler{server: srv, permissions: perms, readOnly: s.cfg.System.SFTP.ReadOnly}
		rs := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
		go func() {
			_ = rs.Serve()
			rs.Close()
		}()
	}
}
