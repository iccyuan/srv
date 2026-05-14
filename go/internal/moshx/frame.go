// Package moshx implements srv's mosh-style UDP transport: a reliable,
// encrypted, roaming-tolerant datagram protocol used by `srv mosh` to
// keep an interactive remote shell alive across network changes.
//
// Wire format (per UDP datagram):
//
//	[12 bytes nonce] [ encrypted(payload) ] [16 bytes GCM tag]
//
// The 12-byte nonce is a 64-bit per-direction counter prefixed with a
// 4-byte direction tag ("CTOS" or "STOC"), so nonces never collide
// between client→server and server→client even when seq numbers
// realign after a roam.
//
// Decrypted payload:
//
//	uint64 seq      this frame's sequence number (monotonic per direction)
//	uint64 ack      highest seq we've delivered in-order from the peer
//	uint8  kind     0=data 1=ack-only 2=hello 3=bye 4=winsize
//	...kind-specific body...
//
// Sequence numbers are 64-bit so the counter never wraps in a
// realistic session. The ACK piggybacks on every data frame so an
// idle direction still informs the peer's retransmit decisions
// without a dedicated ACK stream.
package moshx

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	nonceSize    = 12
	tagSize      = 16
	headerSize   = 17 // seq(8) + ack(8) + kind(1)
	maxFrameSize = 65535 - 28
)

// FrameKind enumerates the small set of distinct frame payloads.
type FrameKind byte

const (
	KindData    FrameKind = 0
	KindAckOnly FrameKind = 1
	KindHello   FrameKind = 2
	KindBye     FrameKind = 3
	KindWinsize FrameKind = 4
)

// Direction prefixes nonces so the client/server share one 64-bit
// counter space without their nonces ever colliding -- otherwise a
// roam (server adopting client's new addr) could reuse a nonce that
// had decrypted under the old addr.
type Direction [4]byte

var (
	ClientToServer = Direction{'C', 'T', 'O', 'S'}
	ServerToClient = Direction{'S', 'T', 'O', 'C'}
)

// Frame is the decrypted payload. Body's contents depend on Kind:
//   - KindData / KindHello: arbitrary bytes (user data / hello banner).
//   - KindAckOnly:          empty.
//   - KindBye:              optional reason string.
//   - KindWinsize:          4 bytes -- rows(uint16 BE), cols(uint16 BE).
type Frame struct {
	Seq  uint64
	Ack  uint64
	Kind FrameKind
	Body []byte
}

// Codec wraps an AES-GCM cipher and a per-direction nonce counter.
// Two Codecs sit on either end of one connection: one in each
// direction. Both peers know the same shared secret but use distinct
// Direction tags so their nonces can't collide.
type Codec struct {
	gcm     cipher.AEAD
	dirTag  Direction
	counter uint64 // next nonce-counter to use when sealing
}

// NewCodec builds an AES-GCM codec from a 32-byte shared secret.
// The same secret feeds both Codecs; the Direction differs.
func NewCodec(secret []byte, dir Direction) (*Codec, error) {
	if len(secret) != 32 {
		return nil, fmt.Errorf("shared secret must be 32 bytes, got %d", len(secret))
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, nonceSize)
	if err != nil {
		return nil, err
	}
	return &Codec{gcm: gcm, dirTag: dir}, nil
}

// Seal encrypts the given Frame into wire bytes. The nonce is freshly
// generated from the codec's monotonic counter so two consecutive
// Seal calls never reuse a nonce. Output layout: [nonce || ct || tag].
func (c *Codec) Seal(f *Frame) ([]byte, error) {
	if len(f.Body)+headerSize > maxFrameSize {
		return nil, fmt.Errorf("frame too large (%d)", len(f.Body)+headerSize)
	}
	plain := make([]byte, headerSize+len(f.Body))
	binary.BigEndian.PutUint64(plain[0:8], f.Seq)
	binary.BigEndian.PutUint64(plain[8:16], f.Ack)
	plain[16] = byte(f.Kind)
	copy(plain[headerSize:], f.Body)

	nonce := make([]byte, nonceSize)
	copy(nonce[:4], c.dirTag[:])
	binary.BigEndian.PutUint64(nonce[4:], c.counter)
	c.counter++

	out := make([]byte, 0, nonceSize+len(plain)+tagSize)
	out = append(out, nonce...)
	out = c.gcm.Seal(out, nonce, plain, nil)
	return out, nil
}

// Open decrypts wire bytes back into a Frame. The nonce is read from
// the leading 12 bytes; the direction-tag check ensures we don't
// accidentally accept a frame from our own outbound stream looped
// back. Returns a stable error so the caller can drop the packet
// without surfacing detail to remote attackers.
var ErrBadFrame = errors.New("moshx: frame decrypt failed")

// Open verifies + decrypts buf and returns the Frame. `expectDir` is
// the direction tag we EXPECT the peer to be sending with (i.e. the
// other side's direction). Anything else (replay attempt, our own
// echo) returns ErrBadFrame.
func (c *Codec) Open(buf []byte, expectDir Direction) (*Frame, error) {
	if len(buf) < nonceSize+tagSize+headerSize {
		return nil, ErrBadFrame
	}
	nonce := buf[:nonceSize]
	for i, b := range expectDir {
		if nonce[i] != b {
			return nil, ErrBadFrame
		}
	}
	ct := buf[nonceSize:]
	plain, err := c.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrBadFrame
	}
	if len(plain) < headerSize {
		return nil, ErrBadFrame
	}
	f := &Frame{
		Seq:  binary.BigEndian.Uint64(plain[0:8]),
		Ack:  binary.BigEndian.Uint64(plain[8:16]),
		Kind: FrameKind(plain[16]),
		Body: append([]byte(nil), plain[headerSize:]...),
	}
	return f, nil
}
