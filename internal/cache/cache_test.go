package cache

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

const serverPassword = "fake-valkey-password" //nolint:gosec // test value

// fakeValkey speaks just enough RESP for valkey-go's handshake and PING. It
// requires serverPassword, supplied through HELLO ... AUTH or AUTH.
type fakeValkey struct {
	addr string
}

func startFakeValkey(t *testing.T, wrap func(net.Listener) net.Listener) *fakeValkey {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if wrap != nil {
		ln = wrap(ln)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeValkey(conn)
		}
	}()
	return &fakeValkey{addr: ln.Addr().String()}
}

func serveFakeValkey(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	authed := false
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		var reply string
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			reply = "-NOAUTH Authentication required.\r\n"
			for i := 1; i+2 < len(args); i++ {
				if strings.EqualFold(args[i], "AUTH") {
					if args[i+2] == serverPassword {
						authed = true
					} else {
						reply = "-WRONGPASS invalid username-password pair\r\n"
					}
				}
			}
			if authed {
				reply = "%7\r\n$6\r\nserver\r\n$6\r\nvalkey\r\n$7\r\nversion\r\n$5\r\n8.0.0\r\n" +
					"$5\r\nproto\r\n:3\r\n$2\r\nid\r\n:1\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n" +
					"$4\r\nrole\r\n$6\r\nmaster\r\n$7\r\nmodules\r\n*0\r\n"
			}
		case "AUTH":
			if len(args) > 1 && args[len(args)-1] == serverPassword {
				authed = true
				reply = "+OK\r\n"
			} else {
				reply = "-WRONGPASS invalid username-password pair\r\n"
			}
		case "PING":
			if authed {
				reply = "+PONG\r\n"
			} else {
				reply = "-NOAUTH Authentication required.\r\n"
			}
		default:
			if authed {
				reply = "+OK\r\n"
			} else {
				reply = "-NOAUTH Authentication required.\r\n"
			}
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

// readCommand reads one RESP array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected frame %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "$")))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func pingWithin2s(t *testing.T, o Options) error {
	t.Helper()
	client, err := Open(o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return Ping(ctx, client)
}

func TestOpenAuthenticatesWithPassword(t *testing.T) {
	t.Parallel()
	srv := startFakeValkey(t, nil)

	if err := pingWithin2s(t, Options{Addr: srv.addr, Password: serverPassword}); err != nil {
		t.Errorf("Ping with the right password: %v", err)
	}
	if err := pingWithin2s(t, Options{Addr: srv.addr, Password: "wrong"}); err == nil {
		t.Error("Ping with a wrong password succeeded")
	}
	if err := pingWithin2s(t, Options{Addr: srv.addr}); err == nil {
		t.Error("Ping with no password succeeded against a server that requires one")
	}
}

// selfSignedCert returns a certificate for 127.0.0.1 and a pool trusting it.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-valkey"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestOpenUsesTLSWhenConfigured(t *testing.T) {
	t.Parallel()
	cert, pool := selfSignedCert(t)
	tlsServer := startFakeValkey(t, func(ln net.Listener) net.Listener {
		return tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	})
	plainServer := startFakeValkey(t, nil)
	clientTLS := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	if err := pingWithin2s(t, Options{Addr: tlsServer.addr, Password: serverPassword, TLS: clientTLS}); err != nil {
		t.Errorf("Ping over TLS: %v", err)
	}
	if err := pingWithin2s(t, Options{Addr: tlsServer.addr, Password: serverPassword}); err == nil {
		t.Error("plaintext Ping against a TLS server succeeded")
	}
	if err := pingWithin2s(t, Options{Addr: plainServer.addr, Password: serverPassword, TLS: clientTLS}); err == nil {
		t.Error("TLS Ping against a plaintext server succeeded")
	}
}

func TestTLSConfigSetsServerNameAndMinimumVersion(t *testing.T) {
	t.Parallel()
	cfg := TLSConfig("cache.example.com:6380")
	if cfg.ServerName != "cache.example.com" {
		t.Errorf("ServerName = %q, want cache.example.com", cfg.ServerName)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}
}
