package auth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
)

// buildTLSTransport returns an *http.Transport that trusts the PEM CA bundle
// provided as a base64-encoded string. Used by dexManager when DexConfig.CACert
// is set.
func buildTLSTransport(caCertB64 string) (*http.Transport, error) {
	pemBytes, err := base64.StdEncoding.DecodeString(caCertB64)
	if err != nil {
		return nil, fmt.Errorf("decoding CACert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("CACert: no valid PEM certificates found")
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}, nil
}
