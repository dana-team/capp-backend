package desiredstate

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(id string) []byte { return bytes.Repeat([]byte(id[len(id)-1:]), keySize) }

func testSealer(t *testing.T, active string, others ...string) *Sealer {
	t.Helper()
	keys := map[string][]byte{active: testKey(active)}
	for _, id := range others {
		keys[id] = testKey(id)
	}
	sealer, err := NewSealer(active, keys)
	require.NoError(t, err)
	return sealer
}

var secretRef = Ref{Cluster: "c1", Kind: KindSecret, Namespace: "team-a", Name: "db"}

func TestSealerRoundTrip(t *testing.T) {
	data := map[string][]byte{"password": []byte("hunter2"), "empty": {}, "binary": {0, 0xff}}
	sealer := testSealer(t, "k1")
	sealed, err := sealer.Seal(secretRef, data)
	require.NoError(t, err)
	opened, err := sealer.Open(secretRef, sealed)
	require.NoError(t, err)
	assert.Equal(t, data, opened)

	again, err := sealer.Seal(secretRef, data)
	require.NoError(t, err)
	assert.NotEqual(t, sealed[0].Nonce, again[0].Nonce)
}

func TestSealerRejectsMovedValues(t *testing.T) {
	sealer := testSealer(t, "k1")
	sealed, err := sealer.Seal(secretRef, map[string][]byte{"a": []byte("one"), "b": []byte("two")})
	require.NoError(t, err)

	for name, ref := range map[string]Ref{
		"cluster":   {Cluster: "c2", Kind: KindSecret, Namespace: "team-a", Name: "db"},
		"namespace": {Cluster: "c1", Kind: KindSecret, Namespace: "team-b", Name: "db"},
		"name":      {Cluster: "c1", Kind: KindSecret, Namespace: "team-a", Name: "other"},
	} {
		_, err := sealer.Open(ref, sealed)
		require.Error(t, err, name)
	}

	sealed[0].Key, sealed[1].Key = sealed[1].Key, sealed[0].Key
	_, err = sealer.Open(secretRef, sealed)
	require.Error(t, err)
}

func TestSealerRejectsTampering(t *testing.T) {
	sealer := testSealer(t, "k1")
	sealed, err := sealer.Seal(secretRef, map[string][]byte{"a": []byte("one")})
	require.NoError(t, err)
	sealed[0].Ciphertext[0] ^= 1
	_, err = sealer.Open(secretRef, sealed)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "one")

	sealed[0].Nonce = sealed[0].Nonce[:4]
	_, err = sealer.Open(secretRef, sealed)
	require.Error(t, err)
}

func TestSealerKeyRotation(t *testing.T) {
	sealed, err := testSealer(t, "k1").Seal(secretRef, map[string][]byte{"a": []byte("one")})
	require.NoError(t, err)

	opened, err := testSealer(t, "k2", "k1").Open(secretRef, sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), opened["a"])

	_, err = testSealer(t, "k2").Open(secretRef, sealed)
	require.ErrorContains(t, err, `"k1" is not defined`)
}

func TestParseKeys(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(testKey("k1"))
	keys, err := ParseKeys(" k1=" + good + ", k2=" + good + ",")
	require.NoError(t, err)
	assert.Len(t, keys, 2)

	for name, encoded := range map[string]string{
		"no id":      good,
		"upper id":   "K1=" + good,
		"not base64": "k1=!!",
		"short":      "k1=" + base64.StdEncoding.EncodeToString([]byte("short")),
		"duplicate":  "k1=" + good + ",k1=" + good,
	} {
		_, err := ParseKeys(encoded)
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), strings.TrimRight(good, "="), name)
	}
}

func TestNewSealerNeedsTheActiveKey(t *testing.T) {
	_, err := NewSealer("missing", map[string][]byte{"k1": testKey("k1")})
	require.Error(t, err)
}
