// Package bfd implements Bidirectional Forwarding Detection (RFC 5880,
// asynchronous mode; RFC 5881 single-hop and RFC 5883 multihop over UDP)
// for the routing protocols (reference 5.12).
//
// The session state machine is pure (driven by received packets and a
// clock); the UDP transport is a thin layer on top.
package bfd

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

// State is a session state (RFC 5880 §4.1).
type State uint8

const (
	AdminDown State = 0
	Down      State = 1
	Init      State = 2
	Up        State = 3
)

func (s State) String() string {
	return [...]string{"AdminDown", "Down", "Init", "Up"}[s&3]
}

// Diag is a diagnostic code (RFC 5880 §4.1).
type Diag uint8

const (
	DiagNone              Diag = 0
	DiagTimeExpired       Diag = 1
	DiagEchoFailed        Diag = 2
	DiagNeighborDown      Diag = 3
	DiagForwardingReset   Diag = 4
	DiagPathDown          Diag = 5
	DiagConcatPathDown    Diag = 6
	DiagAdminDown         Diag = 7
	DiagReverseConcatDown Diag = 8
)

var diagNames = [...]string{"No Diagnostic", "Control Detection Time Expired", "Echo Function Failed",
	"Neighbor Signaled Session Down", "Forwarding Plane Reset", "Path Down", "Concatenated Path Down",
	"Administratively Down", "Reverse Concatenated Path Down"}

func (d Diag) String() string {
	if int(d) < len(diagNames) {
		return diagNames[d]
	}
	return fmt.Sprintf("diag %d", d)
}

// Authentication types (RFC 5880 §4.2).
const (
	AuthNone            = 0
	AuthSimple          = 1
	AuthKeyedMD5        = 2
	AuthMeticulousMD5   = 3
	AuthKeyedSHA1       = 4
	AuthMeticulousSHA1  = 5
	headerLen           = 24
	md5AuthLen          = 24
	sha1AuthLen         = 28
	flagPoll            = 0x20
	flagFinal           = 0x10
	flagControlPlaneInd = 0x08
	flagAuth            = 0x04
	flagDemand          = 0x02
	flagMultipoint      = 0x01
)

// Packet is a BFD control packet.
type Packet struct {
	Diag              Diag
	State             State
	Poll, Final       bool
	CPI               bool // control plane independent
	Demand            bool
	DetectMult        uint8
	MyDisc, YourDisc  uint32
	DesiredMinTx      uint32 // microseconds
	RequiredMinRx     uint32
	RequiredMinEchoRx uint32
	// Authentication (AuthType 0: none).
	AuthType  uint8
	AuthKeyID uint8
	AuthSeq   uint32
	authData  []byte // digest as received
}

// Errors of Decode.
var (
	ErrShort   = errors.New("bfd: packet too short")
	ErrVersion = errors.New("bfd: not version 1")
	ErrInvalid = errors.New("bfd: invalid packet")
)

// Decode parses a control packet and checks the rules of RFC 5880
// §6.8.6 that do not depend on session state. The authentication digest
// is checked by Verify.
func Decode(b []byte) (*Packet, error) {
	if len(b) < headerLen {
		return nil, ErrShort
	}
	if b[0]>>5 != 1 {
		return nil, ErrVersion
	}
	length := int(b[3])
	if length < headerLen || length > len(b) {
		return nil, ErrInvalid
	}
	p := &Packet{
		Diag:              Diag(b[0] & 0x1f),
		State:             State(b[1] >> 6),
		Poll:              b[1]&flagPoll != 0,
		Final:             b[1]&flagFinal != 0,
		CPI:               b[1]&flagControlPlaneInd != 0,
		Demand:            b[1]&flagDemand != 0,
		DetectMult:        b[2],
		MyDisc:            binary.BigEndian.Uint32(b[4:]),
		YourDisc:          binary.BigEndian.Uint32(b[8:]),
		DesiredMinTx:      binary.BigEndian.Uint32(b[12:]),
		RequiredMinRx:     binary.BigEndian.Uint32(b[16:]),
		RequiredMinEchoRx: binary.BigEndian.Uint32(b[20:]),
	}
	switch {
	case p.DetectMult == 0, b[1]&flagMultipoint != 0, p.MyDisc == 0, p.Poll && p.Final:
		return nil, ErrInvalid
	case p.YourDisc == 0 && p.State != Down && p.State != AdminDown:
		return nil, ErrInvalid
	}
	auth := b[1]&flagAuth != 0
	if !auth {
		if length != headerLen {
			// Trailing bytes beyond the header without authentication.
			return nil, ErrInvalid
		}
		return p, nil
	}
	a := b[headerLen:length]
	if len(a) < 2 || int(a[1]) != len(a) {
		return nil, ErrInvalid
	}
	p.AuthType = a[0]
	switch p.AuthType {
	case AuthKeyedMD5, AuthMeticulousMD5:
		if len(a) != md5AuthLen {
			return nil, ErrInvalid
		}
	case AuthKeyedSHA1, AuthMeticulousSHA1:
		if len(a) != sha1AuthLen {
			return nil, ErrInvalid
		}
	default:
		return nil, fmt.Errorf("bfd: authentication type %d not supported", p.AuthType)
	}
	p.AuthKeyID = a[2]
	p.AuthSeq = binary.BigEndian.Uint32(a[4:])
	p.authData = append([]byte(nil), a[8:]...)
	return p, nil
}

// Auth is a session's authentication (nil: none).
type Auth struct {
	Type  uint8 // AuthKeyedMD5 or AuthKeyedSHA1
	KeyID uint8
	Key   []byte
}

// Encode serialises the packet; with auth the digest is computed (the
// sequence number is p.AuthSeq).
func (p *Packet) Encode(auth *Auth) []byte {
	length := headerLen
	if auth != nil {
		switch auth.Type {
		case AuthKeyedMD5, AuthMeticulousMD5:
			length += md5AuthLen
		default:
			length += sha1AuthLen
		}
	}
	b := make([]byte, length)
	b[0] = 1<<5 | byte(p.Diag&0x1f)
	b[1] = byte(p.State) << 6
	if p.Poll {
		b[1] |= flagPoll
	}
	if p.Final {
		b[1] |= flagFinal
	}
	if p.CPI {
		b[1] |= flagControlPlaneInd
	}
	if p.Demand {
		b[1] |= flagDemand
	}
	b[2] = p.DetectMult
	b[3] = byte(length)
	binary.BigEndian.PutUint32(b[4:], p.MyDisc)
	binary.BigEndian.PutUint32(b[8:], p.YourDisc)
	binary.BigEndian.PutUint32(b[12:], p.DesiredMinTx)
	binary.BigEndian.PutUint32(b[16:], p.RequiredMinRx)
	binary.BigEndian.PutUint32(b[20:], p.RequiredMinEchoRx)
	if auth == nil {
		return b
	}
	b[1] |= flagAuth
	a := b[headerLen:]
	a[0], a[1], a[2] = auth.Type, byte(len(a)), auth.KeyID
	binary.BigEndian.PutUint32(a[4:], p.AuthSeq)
	digest(b, auth)
	return b
}

// digest writes the digest of an encoded packet: the key goes into the
// digest field, the hash of the whole packet replaces it (RFC 5880 §6.7.3,
// §6.7.4).
func digest(b []byte, auth *Auth) {
	field := b[headerLen+8:]
	clear(field)
	copy(field, auth.Key)
	switch auth.Type {
	case AuthKeyedMD5, AuthMeticulousMD5:
		h := md5.Sum(b)
		copy(field, h[:])
	default:
		h := sha1.Sum(b)
		copy(field, h[:])
	}
}

// Verify checks the digest of a received packet (raw bytes) against the
// session's key; the sequence number rules are checked by the session.
func Verify(raw []byte, p *Packet, auth *Auth) bool {
	if auth == nil {
		return p.AuthType == AuthNone
	}
	if p.AuthType != auth.Type || p.AuthKeyID != auth.KeyID {
		return false
	}
	b := append([]byte(nil), raw[:raw[3]]...)
	digest(b, auth)
	return subtle.ConstantTimeCompare(b[headerLen+8:], p.authData) == 1
}
