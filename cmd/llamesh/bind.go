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
	tlsInfo, ok := info.(credentials.TLSInfo)
	if !ok || tlsInfo.State.Version != tls.VersionTLS13 {
		conn.Close()
		return nil, nil, errors.New("token binding requires TLS 1.3")
	}
	ekm, err := tlsInfo.State.ExportKeyingMaterial(tokenBindingLabel, nil, 32)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("export token binding: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	clientProof := tokenProof(c.token, "llamesh client", ekm)
	if n, err := conn.Write(clientProof); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write client token proof: %w", err)
	} else if n != len(clientProof) {
		conn.Close()
		return nil, nil, io.ErrShortWrite
	}
	serverProof := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, serverProof); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read server token proof: %w", err)
	}
	if !hmac.Equal(serverProof, tokenProof(c.token, "llamesh server", ekm)) {
		conn.Close()
		return nil, nil, errors.New("bad server token proof")
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, info, nil
}

func (c *tokenCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.inner.ServerHandshake(raw)
	if err != nil {
		return nil, nil, err
	}
	tlsInfo, ok := info.(credentials.TLSInfo)
	if !ok || tlsInfo.State.Version != tls.VersionTLS13 {
		conn.Close()
		return nil, nil, errors.New("token binding requires TLS 1.3")
	}
	ekm, err := tlsInfo.State.ExportKeyingMaterial(tokenBindingLabel, nil, 32)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("export token binding: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	// The client proves first, so a host that only connects is sent nothing to guess the token from.
	clientProof := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, clientProof); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read client token proof: %w", err)
	}
	if !hmac.Equal(clientProof, tokenProof(c.token, "llamesh client", ekm)) {
		conn.Close()
		return nil, nil, errors.New("bad client token proof")
	}
	serverProof := tokenProof(c.token, "llamesh server", ekm)
	if n, err := conn.Write(serverProof); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write server token proof: %w", err)
	} else if n != len(serverProof) {
		conn.Close()
		return nil, nil, io.ErrShortWrite
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, info, nil
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

func (c *tokenCredentials) OverrideServerName(name string) error {
	return c.inner.OverrideServerName(name)
}
