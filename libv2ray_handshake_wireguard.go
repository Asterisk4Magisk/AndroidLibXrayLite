package libv2ray

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

// WireGuard handshake initiation (Noise_IKpsk2), see https://www.wireguard.com/protocol/.
// Only the initiation is built; the round trip ends when the server answers with a handshake
// response or a cookie reply addressed to our sender index, so no session is established.

const (
	wireGuardMessageInitiation = 1
	wireGuardMessageResponse   = 2
	wireGuardMessageCookie     = 3

	wireGuardInitiationSize = 148
	wireGuardResponseSize   = 92
	wireGuardCookieSize     = 64

	wireGuardTAI64NBase = uint64(0x400000000000000a)
	wireGuardTimeout    = 3 * time.Second
)

var (
	wireGuardConstruction = []byte("Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s")
	wireGuardIdentifier   = []byte("WireGuard v1 zx2c4 Jason@zx2c4.com")
	wireGuardLabelMAC1    = []byte("mac1----")
)

type wireGuardHandshakeRequest struct {
	Endpoint  string `json:"endpoint"`
	SecretKey string `json:"secretKey"`
	PublicKey string `json:"publicKey"`
	Reserved  []int  `json:"reserved"`
	TimeoutMs int64  `json:"timeoutMs"`
}

type wireGuardHandshakeResult struct {
	DelayMs int64  `json:"delayMs"`
	Error   string `json:"error"`
}

type wireGuardHandshake struct {
	localPrivateKey [32]byte
	serverPublicKey [32]byte
	reserved        [3]byte
}

// MeasureWireGuardHandshake sends one WireGuard handshake initiation to the endpoint and
// returns how long the server took to answer, without establishing a tunnel.
// requestJSON: {"endpoint":"host:port","secretKey":"base64","publicKey":"base64 peer key",
// "reserved":[0,0,0],"timeoutMs":3000}. Returns JSON: {"delayMs":n,"error":""}; delayMs is
// -1 when error is set.
func MeasureWireGuardHandshake(requestJSON string) string {
	delay, err := measureWireGuardHandshake(requestJSON)
	if err != nil {
		return marshalWireGuardHandshakeResult(wireGuardHandshakeResult{DelayMs: -1, Error: err.Error()})
	}
	return marshalWireGuardHandshakeResult(wireGuardHandshakeResult{DelayMs: delay})
}

func measureWireGuardHandshake(requestJSON string) (int64, error) {
	var request wireGuardHandshakeRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return -1, err
	}
	handshake, err := newWireGuardHandshake(request)
	if err != nil {
		return -1, err
	}
	timeout := time.Duration(request.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = wireGuardTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", request.Endpoint)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	var senderIndexBytes [4]byte
	if _, err := rand.Read(senderIndexBytes[:]); err != nil {
		return -1, err
	}
	senderIndex := binary.LittleEndian.Uint32(senderIndexBytes[:])
	initiation, err := handshake.initiation(senderIndex, time.Now())
	if err != nil {
		return -1, err
	}

	start := time.Now()
	if _, err := conn.Write(initiation); err != nil {
		return -1, fmt.Errorf("failed to send WireGuard handshake: %w", err)
	}
	buffer := make([]byte, 2048)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			return -1, fmt.Errorf("no WireGuard handshake response: %w", err)
		}
		if isWireGuardHandshakeReply(buffer[:n], senderIndex) {
			return time.Since(start).Milliseconds(), nil
		}
	}
}

func newWireGuardHandshake(request wireGuardHandshakeRequest) (*wireGuardHandshake, error) {
	if request.Endpoint == "" {
		return nil, errors.New("endpoint is empty")
	}
	handshake := &wireGuardHandshake{}
	if err := decodeWireGuardKey(request.SecretKey, &handshake.localPrivateKey); err != nil {
		return nil, fmt.Errorf("invalid WireGuard secret key: %w", err)
	}
	if err := decodeWireGuardKey(request.PublicKey, &handshake.serverPublicKey); err != nil {
		return nil, fmt.Errorf("invalid WireGuard public key: %w", err)
	}
	switch len(request.Reserved) {
	case 0:
	case len(handshake.reserved):
		for i, value := range request.Reserved {
			if value < 0 || value > 255 {
				return nil, errors.New("WireGuard reserved values must be 0-255")
			}
			handshake.reserved[i] = byte(value)
		}
	default:
		return nil, errors.New("WireGuard reserved must be 3 bytes")
	}
	return handshake, nil
}

func decodeWireGuardKey(value string, key *[32]byte) error {
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(value), "="))
	if err != nil {
		return err
	}
	if len(decoded) != len(key) {
		return fmt.Errorf("key must be %d bytes", len(key))
	}
	copy(key[:], decoded)
	return nil
}

func (h *wireGuardHandshake) initiation(senderIndex uint32, now time.Time) ([]byte, error) {
	var ephemeralPrivate [32]byte
	if _, err := rand.Read(ephemeralPrivate[:]); err != nil {
		return nil, err
	}
	ephemeralPublic, err := curve25519.X25519(ephemeralPrivate[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	localPublic, err := curve25519.X25519(h.localPrivateKey[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}

	chainKey := wireGuardHash(wireGuardConstruction)
	handshakeHash := wireGuardHash(chainKey[:], wireGuardIdentifier)
	handshakeHash = wireGuardHash(handshakeHash[:], h.serverPublicKey[:])

	chainKey = wireGuardKDF1(chainKey[:], ephemeralPublic)
	handshakeHash = wireGuardHash(handshakeHash[:], ephemeralPublic)

	ephemeralShared, err := curve25519.X25519(ephemeralPrivate[:], h.serverPublicKey[:])
	if err != nil {
		return nil, err
	}
	var key [32]byte
	chainKey, key = wireGuardKDF2(chainKey[:], ephemeralShared)
	encryptedStatic, err := wireGuardSeal(key, localPublic, handshakeHash[:])
	if err != nil {
		return nil, err
	}
	handshakeHash = wireGuardHash(handshakeHash[:], encryptedStatic)

	staticShared, err := curve25519.X25519(h.localPrivateKey[:], h.serverPublicKey[:])
	if err != nil {
		return nil, err
	}
	_, key = wireGuardKDF2(chainKey[:], staticShared)
	encryptedTimestamp, err := wireGuardSeal(key, wireGuardTAI64N(now), handshakeHash[:])
	if err != nil {
		return nil, err
	}

	message := make([]byte, wireGuardInitiationSize)
	message[0] = wireGuardMessageInitiation
	binary.LittleEndian.PutUint32(message[4:8], senderIndex)
	copy(message[8:40], ephemeralPublic)
	copy(message[40:88], encryptedStatic)
	copy(message[88:116], encryptedTimestamp)

	mac1Key := wireGuardHash(wireGuardLabelMAC1, h.serverPublicKey[:])
	mac1, err := blake2s.New128(mac1Key[:])
	if err != nil {
		return nil, err
	}
	mac1.Write(message[:116])
	copy(message[116:132], mac1.Sum(nil))

	// Like Xray's WireGuard client, reserved bytes are written after MAC1 is computed.
	copy(message[1:4], h.reserved[:])
	return message, nil
}

func isWireGuardHandshakeReply(packet []byte, senderIndex uint32) bool {
	if len(packet) < 8 {
		return false
	}
	switch packet[0] {
	case wireGuardMessageResponse:
		return len(packet) == wireGuardResponseSize && binary.LittleEndian.Uint32(packet[8:12]) == senderIndex
	case wireGuardMessageCookie:
		return len(packet) == wireGuardCookieSize && binary.LittleEndian.Uint32(packet[4:8]) == senderIndex
	default:
		return false
	}
}

func wireGuardTAI64N(now time.Time) []byte {
	timestamp := make([]byte, 12)
	binary.BigEndian.PutUint64(timestamp[:8], wireGuardTAI64NBase+uint64(now.Unix()))
	binary.BigEndian.PutUint32(timestamp[8:], uint32(now.Nanosecond()))
	return timestamp
}

func wireGuardSeal(key [32]byte, plaintext []byte, additionalData []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, err
	}
	var nonce [chacha20poly1305.NonceSize]byte
	return aead.Seal(nil, nonce[:], plaintext, additionalData), nil
}

func wireGuardHash(parts ...[]byte) [32]byte {
	digest, _ := blake2s.New256(nil)
	for _, part := range parts {
		digest.Write(part)
	}
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return sum
}

func wireGuardHMAC(key []byte, parts ...[]byte) [32]byte {
	mac := hmac.New(func() hash.Hash {
		digest, _ := blake2s.New256(nil)
		return digest
	}, key)
	for _, part := range parts {
		mac.Write(part)
	}
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

func wireGuardKDF1(key []byte, input []byte) [32]byte {
	pseudoRandomKey := wireGuardHMAC(key, input)
	return wireGuardHMAC(pseudoRandomKey[:], []byte{1})
}

func wireGuardKDF2(key []byte, input []byte) ([32]byte, [32]byte) {
	pseudoRandomKey := wireGuardHMAC(key, input)
	first := wireGuardHMAC(pseudoRandomKey[:], []byte{1})
	second := wireGuardHMAC(pseudoRandomKey[:], first[:], []byte{2})
	return first, second
}

func marshalWireGuardHandshakeResult(result wireGuardHandshakeResult) string {
	data, err := json.Marshal(result)
	if err != nil {
		return `{"delayMs":-1,"error":"failed to marshal result"}`
	}
	return string(data)
}
