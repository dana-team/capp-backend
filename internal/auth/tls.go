package auth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
)

// buildTLSTransport returns an *http.Transport that trusts the PEM CA bundle
// provided as a base64-encoded string. Used by openshift auth manager when
// OpenShiftConfig.CACert is set.
func buildTLSTransport(caCertB64 string) (*http.Transport, error) {
	tlsConfig, err := buildTLSConfig(caCertB64)
	if err != nil {
		return nil, err
	}
	return &http.Transport{TLSClientConfig: tlsConfig}, nil
}

// buildTLSConfig trusts the supplied base64 PEM CA bundle, or system roots
// when the bundle is empty.
func buildTLSConfig(caCertB64 string) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caCertB64 == "" {
		return tlsConfig, nil
	}

	pemBytes, err := base64.StdEncoding.DecodeString(caCertB64)
	if err != nil {
		return nil, fmt.Errorf("decoding CACert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("CACert: no valid PEM certificates found")
	}
	tlsConfig.RootCAs = pool
	return tlsConfig, nil
}
