package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"google.golang.org/grpc/credentials"
)

const tokenBindingLabel = "EXPORTER-llamesh-token-binding"

// Whatever the collector dials sees its proof and can guess at the token offline, so a short token is
// refused. Length is all that can be checked; the Ansible role's tokens are 48 random characters.
const minTokenTLSLen = 32

type tokenCredentials struct {
	inner credentials.TransportCredentials
	token string
}

func tokenTLSServerCredentials(token string) (credentials.TransportCredentials, error) {
	cert, err := tokenTLSCertificate()
	if err != nil {
		return nil, err
	}
	inner := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2"},
	})
	return &tokenCredentials{inner: inner, token: token}, nil
}

func tokenTLSClientCredentials(token string) credentials.TransportCredentials {
	inner := credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true, // The token proof authenticates the peer.
	})
	return &tokenCredentials{inner: inner, token: token}
}

func tokenTLSCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func (c *tokenCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.inner.ClientHandshake(ctx, authority, raw)
	if err != nil {
		return nil, nil, err
	}
	if err := c.prove(conn, info, true); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, info, nil
}

func (c *tokenCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.inner.ServerHandshake(raw)
	if err != nil {
		return nil, nil, err
	}
	if err := c.prove(conn, info, false); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, info, nil
}

// prove exchanges token proofs bound to the TLS session. The client proves first, so a host that only
// connects is sent nothing to guess the token from.
func (c *tokenCredentials) prove(conn net.Conn, info credentials.AuthInfo, client bool) error {
	tlsInfo, ok := info.(credentials.TLSInfo)
	if !ok || tlsInfo.State.Version != tls.VersionTLS13 {
		return errors.New("token binding requires TLS 1.3")
	}
	ekm, err := tlsInfo.State.ExportKeyingMaterial(tokenBindingLabel, nil, 32)
	if err != nil {
		return fmt.Errorf("export token binding: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	mine, theirs, peer := tokenProof(c.token, "llamesh server", ekm), tokenProof(c.token, "llamesh client", ekm), "client"
	if client {
		mine, theirs, peer = theirs, mine, "server"
		if _, err := conn.Write(mine); err != nil {
			return fmt.Errorf("write client token proof: %w", err)
		}
	}
	got := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("read %s token proof: %w", peer, err)
	}
	if !hmac.Equal(got, theirs) {
		return fmt.Errorf("bad %s token proof", peer)
	}
	if !client {
		if _, err := conn.Write(mine); err != nil {
			return fmt.Errorf("write server token proof: %w", err)
		}
	}
	return conn.SetDeadline(time.Time{})
}

func tokenProof(token, direction string, ekm []byte) []byte {
	h := hmac.New(sha256.New, []byte(token))
	h.Write([]byte(direction))
	h.Write(ekm)
	return h.Sum(nil)
}

func (c *tokenCredentials) Info() credentials.ProtocolInfo {
	return c.inner.Info()
}

func (c *tokenCredentials) Clone() credentials.TransportCredentials {
	return &tokenCredentials{inner: c.inner.Clone(), token: c.token}
}

// OverrideServerName is required by the interface but unused by gRPC, and the name is not checked here.
func (c *tokenCredentials) OverrideServerName(string) error {
	return nil
}
