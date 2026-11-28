// Package cache wraps the Valkey client used for the readiness check.
package cache

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	valkey "github.com/valkey-io/valkey-go"
)

// Options says how to reach Valkey.
type Options struct {
	Addr string
	// Password is sent as AUTH for the default user; empty sends none.
	Password string
	// TLS connects over TLS when non-nil; nil means plaintext.
	TLS *tls.Config
}

// TLSConfig returns a TLS configuration for addr that verifies the server
// against the system roots under the host name in addr.
func TLSConfig(addr string) *tls.Config {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
}

// Open builds a Valkey client for o.Addr. Open attempts one dial; with
// ForceSingleClient a failed dial is tolerated and the client reconnects on
// first use, so Open fails only for an address it cannot use.
func Open(o Options) (valkey.Client, error) {
	addr := o.Addr
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("cache: invalid Valkey address %q: %w", addr, err)
	}
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
		Password:          o.Password,
		TLSConfig:         o.TLS,
	})
	// With ForceSingleClient the library returns a usable client alongside the
	// initial dial error, so only a nil client is a real failure.
	if client == nil {
		return nil, fmt.Errorf("cache: create Valkey client: %w", err)
	}
	return client, nil
}

// Ping sends PING and returns nil only when Valkey answers PONG.
func Ping(ctx context.Context, c valkey.Client) error {
	reply, err := c.Do(ctx, c.B().Ping().Build()).ToString()
	if err != nil {
		return err
	}
	if reply != "PONG" {
		return fmt.Errorf("cache: unexpected PING reply %q", reply)
	}
	return nil
}
