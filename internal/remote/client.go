package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	base    string
	tokenID string
	token   string
	http    *http.Client
}

func New(base, tokenID, token string) *Client {
	return &Client{
		base:    strings.TrimRight(base, "/") + "/api/remote",
		tokenID: tokenID,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Error is returned when the panel responds with a non 2xx status.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("panel returned %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body, out interface{}) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.tokenID+"."+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ammonode-Daemon")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	var res *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		res, err = c.http.Do(req)
		if err == nil && res.StatusCode < 500 {
			break
		}
		if res != nil {
			res.Body.Close()
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
			if body != nil {
				b, _ := json.Marshal(body)
				req.Body = io.NopCloser(bytes.NewReader(b))
			}
		}
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		return &Error{Status: res.StatusCode, Message: e.Error.Message}
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// Servers returns the configuration of every server assigned to this node.
func (c *Client) Servers(ctx context.Context) ([]ServerConfiguration, error) {
	var out struct {
		Data []ServerConfiguration `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/servers", nil, &out)
}

func (c *Client) Server(ctx context.Context, uuid string) (ServerConfiguration, error) {
	var out struct {
		Data ServerConfiguration `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/servers/"+uuid, nil, &out)
}

func (c *Client) InstallScript(ctx context.Context, uuid string) (InstallationScript, error) {
	var out InstallationScript
	return out, c.do(ctx, http.MethodGet, "/servers/"+uuid+"/install", nil, &out)
}

func (c *Client) SetInstallStatus(ctx context.Context, uuid string, successful, reinstall bool) error {
	return c.do(ctx, http.MethodPost, "/servers/"+uuid+"/install", map[string]bool{"successful": successful, "reinstall": reinstall}, nil)
}

func (c *Client) ValidateSftp(ctx context.Context, req SftpAuthRequest) (SftpAuthResponse, error) {
	var out SftpAuthResponse
	return out, c.do(ctx, http.MethodPost, "/sftp/auth", req, &out)
}

func (c *Client) SendActivity(ctx context.Context, events []ActivityEvent) error {
	return c.do(ctx, http.MethodPost, "/activity", map[string]interface{}{"data": events}, nil)
}

func (c *Client) Heartbeat(ctx context.Context, info interface{}) error {
	return c.do(ctx, http.MethodPost, "/heartbeat", info, nil)
}

func (c *Client) SetBackupStatus(ctx context.Context, uuid string, data interface{}) error {
	return c.do(ctx, http.MethodPost, "/backups/"+uuid, data, nil)
}

// SetTransferStatus reports the outcome of a server transfer to the panel.
func (c *Client) SetTransferStatus(ctx context.Context, uuid string, successful bool, message string) error {
	return c.do(ctx, http.MethodPost, "/servers/"+uuid+"/transfer", map[string]interface{}{"successful": successful, "error": message}, nil)
}
