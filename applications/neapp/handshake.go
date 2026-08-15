package main

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

// A deliberately small TLS-shaped handshake: exchange identity + an ephemeral
// X25519 public key, ECDH to a shared secret, then HKDF that into a session
// key. It is NOT TLS — there are no certificates and no peer authentication.
// What it demonstrates is that the kernel-supplied device ID reaches the key
// schedule, mirroring Atom<->Atom session negotiation on the control plane.

const (
	handshakePort = 7777
	// wire format: 8-byte big-endian device ID + 32-byte X25519 public key
	deviceIDLen = 8
	pubKeyLen   = 32
	helloLen    = deviceIDLen + pubKeyLen

	sessionKeyLen = 32
	hkdfLabel     = "plasma-ne session key v1"
)

type hello struct {
	deviceID uint64
	pubKey   []byte
}

func (h hello) marshal() []byte {
	buf := make([]byte, helloLen)
	binary.BigEndian.PutUint64(buf[:deviceIDLen], h.deviceID)
	copy(buf[deviceIDLen:], h.pubKey)
	return buf
}

func parseHello(buf []byte) (hello, error) {
	if len(buf) != helloLen {
		return hello{}, fmt.Errorf("short hello: %d bytes, want %d", len(buf), helloLen)
	}
	pub := make([]byte, pubKeyLen)
	copy(pub, buf[deviceIDLen:])
	return hello{
		deviceID: binary.BigEndian.Uint64(buf[:deviceIDLen]),
		pubKey:   pub,
	}, nil
}

// result is what both peers must agree on for the test to have passed.
type result struct {
	localID    uint64
	peerID     uint64
	sessionKey []byte
}

// exchange runs the handshake over an established connection. It is symmetric,
// so the listening and dialing sides run exactly the same code.
func exchange(conn net.Conn, localID uint64) (result, error) {
	curve := ecdh.X25519()

	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return result{}, fmt.Errorf("generate ephemeral key: %w", err)
	}

	out := hello{deviceID: localID, pubKey: priv.PublicKey().Bytes()}
	if _, err := conn.Write(out.marshal()); err != nil {
		return result{}, fmt.Errorf("send hello: %w", err)
	}

	buf := make([]byte, helloLen)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return result{}, fmt.Errorf("read peer hello: %w", err)
	}
	in, err := parseHello(buf)
	if err != nil {
		return result{}, err
	}

	peerPub, err := curve.NewPublicKey(in.pubKey)
	if err != nil {
		return result{}, fmt.Errorf("bad peer public key: %w", err)
	}
	shared, err := priv.ECDH(peerPub)
	if err != nil {
		return result{}, fmt.Errorf("ECDH: %w", err)
	}

	key, err := hkdf.Key(sha256.New, shared, nil, bindingInfo(localID, in.deviceID), sessionKeyLen)
	if err != nil {
		return result{}, fmt.Errorf("derive session key: %w", err)
	}

	return result{localID: localID, peerID: in.deviceID, sessionKey: key}, nil
}

// bindingInfo mixes both device IDs into the key schedule. Sorting them makes
// the result independent of who dialled, so the two peers derive the same key
// while still binding it to this specific pair of devices.
func bindingInfo(a, b uint64) string {
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	info := make([]byte, 0, len(hkdfLabel)+16)
	info = append(info, hkdfLabel...)
	info = binary.BigEndian.AppendUint64(info, lo)
	info = binary.BigEndian.AppendUint64(info, hi)
	return string(info)
}
