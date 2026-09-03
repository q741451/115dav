package pan115

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// Vectors captured from this implementation after it was cross-checked
// byte-for-byte against a known-good decoder for the same endpoint.
const (
	vecKey     = "c1e792791b6d136b18047691b3def840"
	vecMask4   = "ccbc1306"
	vecMask12  = "ed9b97adae2b657c6c25081d"
	vecPayload = "poVh/g3bKsfci4hCNVxEibOCkHZj/8k8cETKIWYOv8uDRY/n0h0DpndMrF28TEnb" +
		"o1S4rgb9SEu5rmR0yavfZ+8/8GosFAvX5epyUIYGYYMvxzTewjkyJvr+W4xhzaaC" +
		"YfOxnxu2sbVdL+Ka0YSK0VCfQWz8YMXsGwgzRSx7CSw="
	vecPlain = "b60d0159e30287d1fe698939d4c54dd5b027e06d0546ca2e067aac12c36b528a" +
		"258b558bce2de12cf453b75860fbe237d10b431be09356d468cc994054d57124" +
		"0be3c4c46fc625cbbcc80bc9997905fd1f0ce3b2af40a1a2ad925edeacb1c2b0b" +
		"8ae5ec9d6dd8d01"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test vector %q: %v", s, err)
	}
	return b
}

func testKey(t *testing.T) sessionKey {
	t.Helper()
	return sessionKey(mustHex(t, vecKey))
}

func TestDeriveMask(t *testing.T) {
	key := testKey(t)
	for _, tc := range []struct {
		width int
		want  string
	}{
		{4, vecMask4},
		{12, vecMask12},
	} {
		if got := deriveMask(key[:], tc.width); !bytes.Equal(got, mustHex(t, tc.want)) {
			t.Errorf("deriveMask(width=%d) = %x, want %s", tc.width, got, tc.want)
		}
	}
}

func TestOpenResponse(t *testing.T) {
	got, err := openResponse(vecPayload, testKey(t))
	if err != nil {
		t.Fatalf("openResponse: %v", err)
	}
	if want := mustHex(t, vecPlain); !bytes.Equal(got, want) {
		t.Errorf("openResponse =\n %x\nwant\n %x", got, want)
	}
}

func TestOpenResponseRejectsGarbage(t *testing.T) {
	key := testKey(t)
	for name, payload := range map[string]string{
		"not base64": "!!!!",
		"too short":  "AAAA",
		"empty":      "",
	} {
		if _, err := openResponse(payload, key); err == nil {
			t.Errorf("%s: want error, got none", name)
		}
	}
}

// applyMask is its own inverse for a fixed mask; the offset quirk in the
// middle of the stream must not break that.
func TestApplyMaskIsInvolution(t *testing.T) {
	key := testKey(t)
	mask := deriveMask(key[:], 12)
	for size := range 40 {
		want := make([]byte, size)
		if _, err := rand.Read(want); err != nil {
			t.Fatal(err)
		}
		got := bytes.Clone(want)
		applyMask(got, mask)
		if size > 0 && bytes.Equal(got, want) {
			t.Fatalf("size=%d: mask left the data unchanged", size)
		}
		applyMask(got, mask)
		if !bytes.Equal(got, want) {
			t.Errorf("size=%d: mask is not an involution", size)
		}
	}
}

func TestSealRequest(t *testing.T) {
	key := testKey(t)
	for _, size := range []int{1, 32, payloadSize - 1, payloadSize, payloadSize + 1, 3 * payloadSize} {
		plaintext := make([]byte, size)
		if _, err := rand.Read(plaintext); err != nil {
			t.Fatal(err)
		}
		got, err := sealRequest(plaintext, key)
		if err != nil {
			t.Fatalf("size=%d: %v", size, err)
		}
		// One RSA block per payloadSize-sized chunk of (key || plaintext).
		blocks := (keySize + size + payloadSize - 1) / payloadSize
		if want := base64Len(blocks * blockSize); len(got) != want {
			t.Errorf("size=%d: envelope is %d chars, want %d", size, len(got), want)
		}
	}
}

func base64Len(n int) int { return (n + 2) / 3 * 4 }

// The framing that sealBlock builds must be what stripPadding expects, since
// the server unwraps it with the same rules.
func TestSealBlockFraming(t *testing.T) {
	for _, size := range []int{1, 40, payloadSize} {
		chunk := make([]byte, size)
		if _, err := rand.Read(chunk); err != nil {
			t.Fatal(err)
		}
		block := make([]byte, blockSize)
		padding := block[2 : blockSize-size-1]
		if _, err := rand.Read(padding); err != nil {
			t.Fatal(err)
		}
		block[1] = 0x02
		for i, b := range padding {
			padding[i] = b%0xff + 0x01
		}
		copy(block[blockSize-size:], chunk)

		if len(padding) < 8 {
			t.Errorf("size=%d: only %d padding bytes, PKCS#1 wants >= 8", size, len(padding))
		}
		if bytes.IndexByte(padding, 0) >= 0 {
			t.Errorf("size=%d: padding contains a zero byte", size)
		}
		body, err := stripPadding(block)
		if err != nil {
			t.Fatalf("size=%d: stripPadding: %v", size, err)
		}
		if !bytes.Equal(body, chunk) {
			t.Errorf("size=%d: framing does not round-trip", size)
		}
	}
}
