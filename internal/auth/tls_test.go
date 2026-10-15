package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTLSTransport(t *testing.T) {
	caCertB64 := base64.StdEncoding.EncodeToString(selfSignedCACertPEM(t))
	cases := []struct {
		name      string
		caCertB64 string
		wantError string
	}{
		{name: "custom CA", caCertB64: caCertB64},
		{name: "invalid base64", caCertB64: "not-valid-base64!!!", wantError: "decoding CACert"},
		{
			name:      "invalid PEM",
			caCertB64: base64.StdEncoding.EncodeToString([]byte("this is not a PEM certificate")),
			wantError: "no valid PEM",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport, err := buildTLSTransport(tc.caCertB64)
			if tc.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantError)
				assert.Nil(t, transport)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, transport)
			require.NotNil(t, transport.TLSClientConfig)
			assert.NotNil(t, transport.TLSClientConfig.RootCAs)
			assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
			assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)
		})
	}
}

func TestBuildTLSConfig(t *testing.T) {
	tlsConfig, err := buildTLSConfig("")
	require.NoError(t, err)
	require.NotNil(t, tlsConfig)
	assert.Nil(t, tlsConfig.RootCAs)
	assert.Equal(t, uint16(tls.VersionTLS12), tlsConfig.MinVersion)
	assert.False(t, tlsConfig.InsecureSkipVerify)
}

func selfSignedCACertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(1 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}
