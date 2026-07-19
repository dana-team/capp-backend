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

func TestBuildTLSTransport_ValidCert(t *testing.T) {
	pemBytes := selfSignedCACertPEM(t)
	b64 := base64.StdEncoding.EncodeToString(pemBytes)

	transport, err := buildTLSTransport(b64)
	require.NoError(t, err)
	require.NotNil(t, transport)
	require.NotNil(t, transport.TLSClientConfig)
	require.NotNil(t, transport.TLSClientConfig.RootCAs)
}

func TestBuildTLSTransport_InvalidBase64(t *testing.T) {
	_, err := buildTLSTransport("not-valid-base64!!!")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding CACert")
}

func TestBuildTLSTransport_InvalidPEM(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("this is not a PEM certificate"))
	_, err := buildTLSTransport(b64)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no valid PEM")
}

func TestBuildTLSTransport_HasMinTLSVersion(t *testing.T) {
	pemBytes := selfSignedCACertPEM(t)
	b64 := base64.StdEncoding.EncodeToString(pemBytes)

	transport, err := buildTLSTransport(b64)
	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
}
