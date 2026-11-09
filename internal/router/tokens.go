package router

import (
	"crypto/subtle"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// WebsocketClaims is the JWT the panel hands to the browser for console access.
type WebsocketClaims struct {
	jwt.RegisteredClaims
	ServerUUID  string   `json:"server_uuid"`
	UserUUID    string   `json:"user_uuid"`
	Permissions []string `json:"permissions"`
}

func (c *WebsocketClaims) Can(p string) bool {
	for _, x := range c.Permissions {
		if x == "*" || x == p {
			return true
		}
	}
	return false
}

// FileClaims authorises a single file download.
type FileClaims struct {
	jwt.RegisteredClaims
	ServerUUID string `json:"server_uuid"`
	FilePath   string `json:"file_path"`
}

func parseToken[T jwt.Claims](secret, raw string, claims T) (T, error) {
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithLeeway(5*time.Second), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid {
		return claims, errors.New("invalid or expired token")
	}
	return claims, nil
}
