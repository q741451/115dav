package pan115

// Codec for the /app/chrome/downurl endpoint.
//
// The endpoint does not speak TLS-only JSON: the request body and the response
// payload are each wrapped in a two-layer envelope. Both layers must be
// reproduced byte for byte or the server rejects the call, so the constants
// below are protocol parameters rather than tunables.
//
// Request  (client -> server): mask -> reverse -> mask -> RSA(public) -> base64
// Response (server -> client): base64 -> RSA(public) -> mask -> reverse -> mask
//
// The response direction applies the *public* exponent as well: the server
// wraps its payload with the private key, so raising it to e recovers the
// plaintext block. That makes the two directions non-inverse, which is why
// there is no local round trip through RSA.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
)

// keySize is the length of the per-request session key that prefixes every
// request envelope and every response envelope.
const keySize = 16

// sessionKey is generated fresh for each call and shared between the request
// envelope and the response envelope of that same call.
type sessionKey [keySize]byte

func newSessionKey() (sessionKey, error) {
	var k sessionKey
	if _, err := io.ReadFull(rand.Reader, k[:]); err != nil {
		return k, fmt.Errorf("draw session key: %w", err)
	}
	return k, nil
}

// maskTable is the byte pool that mask keys are derived from.
var maskTable = [...]byte{
	0xf0, 0xe5, 0x69, 0xae, 0xbf, 0xdc, 0xbf, 0x8a,
	0x1a, 0x45, 0xe8, 0xbe, 0x7d, 0xa6, 0x73, 0xb8,
	0xde, 0x8f, 0xe7, 0xc4, 0x45, 0xda, 0x86, 0xc4,
	0x9b, 0x64, 0x8b, 0x14, 0x6a, 0xb4, 0xf1, 0xaa,
	0x38, 0x01, 0x35, 0x9e, 0x26, 0x69, 0x2c, 0x86,
	0x00, 0x6b, 0x4f, 0xa5, 0x36, 0x34, 0x62, 0xa6,
	0x2a, 0x96, 0x68, 0x18, 0xf2, 0x4a, 0xfd, 0xbd,
	0x6b, 0x97, 0x8f, 0x4d, 0x8f, 0x89, 0x13, 0xb7,
	0x6c, 0x8e, 0x93, 0xed, 0x0e, 0x0d, 0x48, 0x3e,
	0xd7, 0x2f, 0x88, 0xd8, 0xfe, 0xfe, 0x7e, 0x86,
	0x50, 0x95, 0x4f, 0xd1, 0xeb, 0x83, 0x26, 0x34,
	0xdb, 0x66, 0x7b, 0x9c, 0x7e, 0x9d, 0x7a, 0x81,
	0x32, 0xea, 0xb6, 0x33, 0xde, 0x3a, 0xa9, 0x59,
	0x34, 0x66, 0x3b, 0xaa, 0xba, 0x81, 0x60, 0x48,
	0xb9, 0xd5, 0x81, 0x9c, 0xf8, 0x6c, 0x84, 0x77,
	0xff, 0x54, 0x78, 0x26, 0x5f, 0xbe, 0xe8, 0x1e,
	0x36, 0x9f, 0x34, 0x80, 0x5c, 0x45, 0x2c, 0x9b,
	0x76, 0xd5, 0x1b, 0x8f, 0xcc, 0xc3, 0xb8, 0xf5,
}

// clientMask is the fixed second-stage mask on the request side. The response
// side derives its equivalent from the header the server prepends instead.
var clientMask = [...]byte{
	0x78, 0x06, 0xad, 0x4c, 0x33, 0x86, 0x5d, 0x18,
	0x4c, 0x01, 0x3f, 0x46,
}

// deriveMask expands the first n bytes of seed into an n-byte mask by mixing
// them with maskTable entries taken from both ends of an n-strided walk.
func deriveMask(seed []byte, n int) []byte {
	if len(seed) < n || n*(n-1) >= len(maskTable) {
		panic("pan115: mask derivation out of range")
	}
	mask := make([]byte, n)
	for i := range n {
		mask[i] = (seed[i] + maskTable[n*i]) ^ maskTable[n*(n-i-1)]
	}
	return mask
}

// applyMask XORs data with a repeating mask, in place.
//
// The stream is not a plain repeating XOR: the leading len(data)%4 bytes
// consume the mask once, and the mask index then restarts at zero for the
// remainder. Reproducing that quirk exactly is what makes the envelope valid.
func applyMask(data, mask []byte) {
	head := len(data) % 4
	for i := range head {
		data[i] ^= mask[i%len(mask)]
	}
	for i := head; i < len(data); i++ {
		data[i] ^= mask[(i-head)%len(mask)]
	}
}

func reverse(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

// RSA parameters. Only the public half exists client side; see the note at the
// top of the file for why both directions exponentiate by e.
var (
	rsaModulus, _ = new(big.Int).SetString(
		"8686980c0f5a24c4b9d43020cd2c22703ff3f450756529058b1cf88f09b86021"+
			"36477198a6e2683149659bd122c33592fdb5ad47944ad1ea4d36c6b172aad633"+
			"8c3bb6ac6227502d010993ac967d1aef00f0c8e038de2e4d3bc2ec368af2e9f1"+
			"0a6f1eda4f7262f136420c07c331b871bf139f74f3010e3c4fe57df3afb71683", 16)
	rsaExponent = big.NewInt(0x10001)
)

// blockSize is the RSA modulus size in bytes; payloadSize is what fits in one
// block once the 3 framing bytes and at least 8 bytes of padding are removed.
var (
	blockSize   = rsaModulus.BitLen() / 8
	payloadSize = blockSize - 11
)

// sealRequest wraps plaintext into the envelope the downurl endpoint expects.
func sealRequest(plaintext []byte, key sessionKey) (string, error) {
	// The session key travels in the clear ahead of the masked body; the server
	// needs it to derive the mask it answers with.
	buf := make([]byte, keySize+len(plaintext))
	copy(buf, key[:])
	body := buf[keySize:]
	copy(body, plaintext)

	applyMask(body, deriveMask(key[:], 4))
	reverse(body)
	applyMask(body, clientMask[:])

	sealed, err := rsaSeal(buf)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openResponse unwraps a downurl response payload using the session key that
// was used to seal the matching request.
func openResponse(payload string, key sessionKey) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("decode response payload: %w", err)
	}
	unwrapped, err := rsaOpen(raw)
	if err != nil {
		return nil, err
	}
	if len(unwrapped) < keySize {
		return nil, fmt.Errorf("response envelope truncated: %d bytes", len(unwrapped))
	}

	// Mirror image of sealRequest: the server prepends its own 16-byte header,
	// which seeds the first mask.
	plaintext := bytes.Clone(unwrapped[keySize:])
	applyMask(plaintext, deriveMask(unwrapped[:keySize], 12))
	reverse(plaintext)
	applyMask(plaintext, deriveMask(key[:], 4))
	return plaintext, nil
}

// rsaSeal splits input into payload-sized chunks and exponentiates each one
// under PKCS#1 v1.5 type-2 framing.
func rsaSeal(input []byte) ([]byte, error) {
	out := make([]byte, 0, (len(input)/payloadSize+1)*blockSize)
	for len(input) > 0 {
		chunk := min(len(input), payloadSize)
		block, err := sealBlock(input[:chunk])
		if err != nil {
			return nil, err
		}
		out = append(out, block...)
		input = input[chunk:]
	}
	return out, nil
}

func sealBlock(chunk []byte) ([]byte, error) {
	// 0x00 0x02 <nonzero padding> 0x00 <chunk>
	block := make([]byte, blockSize)
	padding := block[2 : blockSize-len(chunk)-1]
	if _, err := io.ReadFull(rand.Reader, padding); err != nil {
		return nil, fmt.Errorf("draw padding: %w", err)
	}
	block[1] = 0x02
	for i, b := range padding {
		// PKCS#1 forbids zero padding bytes; fold the range into 1..255.
		padding[i] = b%0xff + 0x01
	}
	copy(block[blockSize-len(chunk):], chunk)

	sealed := new(big.Int).Exp(new(big.Int).SetBytes(block), rsaExponent, rsaModulus).Bytes()
	// Exp drops leading zero bytes; the wire format is fixed width.
	if pad := blockSize - len(sealed); pad > 0 {
		sealed = append(make([]byte, pad), sealed...)
	}
	return sealed, nil
}

var errNoPadding = errors.New("pan115: response block has no padding terminator")

// rsaOpen exponentiates each block and strips the framing added by the server.
func rsaOpen(input []byte) ([]byte, error) {
	var out []byte
	for len(input) > 0 {
		chunk := min(len(input), blockSize)
		block := new(big.Int).Exp(new(big.Int).SetBytes(input[:chunk]), rsaExponent, rsaModulus).Bytes()
		body, err := stripPadding(block)
		if err != nil {
			return nil, err
		}
		out = append(out, body...)
		input = input[chunk:]
	}
	return out, nil
}

// stripPadding drops the framing prefix, which ends at the first zero byte
// that is not the (already elided) leading one.
func stripPadding(block []byte) ([]byte, error) {
	for i, b := range block {
		if b == 0 && i != 0 {
			return block[i+1:], nil
		}
	}
	return nil, errNoPadding
}
