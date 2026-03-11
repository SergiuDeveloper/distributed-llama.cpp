package shared

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

func ForClientDialBack(pemPath string) (credentials.TransportCredentials, error) {
	certPEM, keyPEM, err := loadSharedPEM(pemPath)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("failed to append CA certificate from %s", pemPath)
	}

	cfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caPool,
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("peer presented no certificates")
			}
			peerCert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse peer cert: %w", err)
			}
			opts := x509.VerifyOptions{Roots: caPool}
			if _, err := peerCert.Verify(opts); err != nil {
				return fmt.Errorf("peer cert verification failed: %w", err)
			}
			return nil
		},
		MinVersion: tls.VersionTLS13,
	}
	return credentials.NewTLS(cfg), nil
}

func loadSharedPEM(pemPath string) (certPEM, keyPEM []byte, err error) {
	raw, err := os.ReadFile(pemPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read PEM file: %w", err)
	}

	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			certPEM = pem.EncodeToMemory(block)
		case "PRIVATE KEY", "EC PRIVATE KEY", "RSA PRIVATE KEY":
			keyPEM = pem.EncodeToMemory(block)
		}
	}

	if len(certPEM) == 0 {
		return nil, nil, fmt.Errorf("no CERTIFICATE block found in %s", pemPath)
	}
	if len(keyPEM) == 0 {
		return nil, nil, fmt.Errorf("no PRIVATE KEY block found in %s", pemPath)
	}
	return certPEM, keyPEM, nil
}

func buildTLSConfig(pemPath string) (*tls.Config, error) {
	certPEM, keyPEM, err := loadSharedPEM(pemPath)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("failed to append CA certificate from %s", pemPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		RootCAs:      caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func ForServer(pemPath string) (credentials.TransportCredentials, error) {
	cfg, err := buildTLSConfig(pemPath)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

func ForClient(pemPath, serverName string) (credentials.TransportCredentials, error) {
	cfg, err := buildTLSConfig(pemPath)
	if err != nil {
		return nil, err
	}
	cfg.ServerName = serverName
	return credentials.NewTLS(cfg), nil
}
