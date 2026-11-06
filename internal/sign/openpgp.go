package sign

import (
	"bufio"
	"bytes"
	"crypto/sha1" //nolint:gosec // The v4 fingerprint is SHA-1 by definition (RFC 4880 12.2).
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The OpenPGP packet tags that repoctl reads (RFC 4880 4.3).
const (
	tagSignature     = 2
	tagSecretKey     = 5
	tagPublicKey     = 6
	tagSecretSubkey  = 7
	tagUserID        = 13
	tagPublicSubkey  = 14
	tagUserAttribute = 17
)

// The public key algorithms (RFC 4880 9.1, RFC 9580 9.1).
const (
	AlgoRSA        = 1
	AlgoRSASign    = 3
	AlgoElGamal    = 16
	AlgoDSA        = 17
	AlgoECDH       = 18
	AlgoECDSA      = 19
	AlgoEdDSA      = 22 // EdDSA in the legacy form of RFC 4880bis
	AlgoX25519     = 25
	AlgoX448       = 26
	AlgoEd25519    = 27
	AlgoEd448      = 28
	hashSHA1       = 2
	sigTypeKeyRev  = 0x20
	sigTypeSubRev  = 0x28
	sigTypeDirect  = 0x1f
	sigTypeSubBind = 0x18
)

// oidEd25519 is the curve OID of the legacy EdDSA Ed25519 key.
var oidEd25519 = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01}

// Cert is the facts of one OpenPGP certificate: the primary public key, its
// user ids, its self-signatures and its subkeys.
type Cert struct {
	// Fingerprint is the full fingerprint, upper-case hex, no spaces.
	Fingerprint string
	// KeyID is the last 16 hex digits of the fingerprint (v4).
	KeyID   string
	Version int
	Algo    int
	// Bits is the size of the RSA modulus, or 255 for Ed25519.
	Bits    int
	Curve   string
	Created time.Time
	// Expires is the end of the key, from its newest self-signature; zero
	// for a key without an expiry.
	Expires time.Time
	// UID is the primary user id, for example Dennis Klein <d.klein@gsi.de>.
	UID  string
	UIDs []string
	// SelfSigHashes holds the hash algorithm of each effective
	// self-signature: the newest certification of each user id, the newest
	// binding of each subkey and the newest direct-key signature.
	SelfSigHashes []int
	Revoked       bool
	Subkeys       []Subkey
	// Raw is the binary packets of the certificate.
	Raw []byte
}

// Subkey is one subkey of a certificate.
type Subkey struct {
	Fingerprint string
	KeyID       string
	Algo        int
	Bits        int
	Curve       string
	Created     time.Time
	Expires     time.Time
	CanSign     bool
	Revoked     bool
}

// Signature is the facts of one signature packet.
type Signature struct {
	Version int
	Type    int
	Algo    int
	Hash    int
	Created time.Time
	// IssuerKeyID is the 16 hex digits of the issuer subpacket.
	IssuerKeyID string
	// IssuerFingerprint is the issuer fingerprint subpacket, if any.
	IssuerFingerprint string
	// KeyExpiry is the key expiration time subpacket: the time after the
	// creation of the key; 0 for none.
	KeyExpiry time.Duration
	Primary   bool
	KeyFlags  int
	hasFlags  bool
}

// Issuer gives the issuer fingerprint, else the issuer key id.
func (s *Signature) Issuer() string {
	if s.IssuerFingerprint != "" {
		return s.IssuerFingerprint
	}
	return s.IssuerKeyID
}

// Short gives the short form of a fingerprint: …41D8 0B6E.
func (c *Cert) Short() string {
	if len(c.Fingerprint) < 8 {
		return c.Fingerprint
	}
	t := c.Fingerprint[len(c.Fingerprint)-8:]
	return "…" + t[:4] + " " + t[4:]
}

// AlgorithmName gives the algorithm in the form of gpg: rsa4096, ed25519.
func (c *Cert) AlgorithmName() string { return algoName(c.Algo, c.Bits, c.Curve) }

func algoName(algo, bits int, curve string) string {
	switch algo {
	case AlgoRSA, AlgoRSASign:
		return fmt.Sprintf("rsa%d", bits)
	case AlgoDSA:
		return fmt.Sprintf("dsa%d", bits)
	case AlgoElGamal:
		return fmt.Sprintf("elg%d", bits)
	case AlgoEdDSA, AlgoECDSA, AlgoECDH:
		if curve != "" {
			return curve
		}
		return fmt.Sprintf("algo%d", algo)
	case AlgoEd25519:
		return "ed25519"
	case AlgoEd448:
		return "ed448"
	case AlgoX25519:
		return "cv25519"
	case AlgoX448:
		return "x448"
	}
	return fmt.Sprintf("algo%d", algo)
}

// HasKey reports whether the fingerprint or key id names the primary key or
// a subkey of the certificate.
func (c *Cert) HasKey(id string) bool {
	id = NormalizeFingerprint(id)
	if id == "" {
		return false
	}
	match := func(fpr, kid string) bool {
		return fpr == id || kid == id || (len(id) == 8 && strings.HasSuffix(kid, id))
	}
	if match(c.Fingerprint, c.KeyID) {
		return true
	}
	for _, s := range c.Subkeys {
		if match(s.Fingerprint, s.KeyID) {
			return true
		}
	}
	return false
}

// RPMKeyID gives the id of the gpg-pubkey entry that rpm makes for the
// key: the last 8 hex digits of the key id, lower case (rpm -e
// gpg-pubkey-ID).
func (c *Cert) RPMKeyID() string {
	if len(c.KeyID) < 8 {
		return strings.ToLower(c.KeyID)
	}
	return strings.ToLower(c.KeyID[len(c.KeyID)-8:])
}

// ExpiresIn reports whether the key expires before now+d.
func (c *Cert) ExpiresIn(now time.Time, d time.Duration) bool {
	return !c.Expires.IsZero() && c.Expires.Before(now.Add(d))
}

// CheckFormat checks gate 3: an OpenPGP v4 key, RSA 2048 or more, or
// Ed25519, and no SHA-1 self-signature. It gives the reason, or "".
func (c *Cert) CheckFormat() string {
	if c.Version != 4 {
		return fmt.Sprintf("an OpenPGP v%d key; the clients need v4", c.Version)
	}
	switch {
	case (c.Algo == AlgoRSA || c.Algo == AlgoRSASign) && c.Bits >= 2048:
	case c.Algo == AlgoEdDSA && c.Curve == "ed25519", c.Algo == AlgoEd25519:
	default:
		return fmt.Sprintf("a %s key; allowed are RSA 2048 or more and Ed25519", c.AlgorithmName())
	}
	for _, h := range c.SelfSigHashes {
		if h == hashSHA1 {
			return "a SHA-1 self-signature; make the self-signatures again with SHA-256 (gpg --quick-set-expire, or a new key)"
		}
	}
	if c.Revoked {
		return "a revoked key"
	}
	return ""
}

// NormalizeFingerprint gives a fingerprint or key id in upper-case hex
// without spaces and without a 0x prefix or a trailing !.
func NormalizeFingerprint(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "!")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToUpper(s)
}

// IsFingerprint reports a full v4 fingerprint: 40 hex digits.
func IsFingerprint(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// GroupFingerprint gives the fingerprint in groups of 4, with two spaces in
// the middle, as gpg shows it.
func GroupFingerprint(f string) string {
	if len(f) != 40 {
		return f
	}
	var b strings.Builder
	for i := 0; i < 40; i += 4 {
		if i == 20 {
			b.WriteString(" ")
		}
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(f[i : i+4])
	}
	return b.String()
}

// AbbrevFingerprint gives the first and the last 4 digits: C2F9…52A7.
func AbbrevFingerprint(f string) string {
	if len(f) < 8 {
		return f
	}
	return f[:4] + "…" + f[len(f)-4:]
}

// Dearmor gives the binary packets of data: each armored block in it, one
// after the other, or data itself when it is binary.
func Dearmor(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0]&0x80 != 0 {
		return data, nil
	}
	var out []byte
	found := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	inBlock, inBody := false, false
	var body strings.Builder
	var crc string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		switch {
		case !inBlock:
			if strings.HasPrefix(line, "-----BEGIN PGP ") && strings.HasSuffix(line, "-----") {
				inBlock, inBody = true, false
				body.Reset()
				crc = ""
			}
		case strings.HasPrefix(line, "-----END PGP "):
			b, err := base64.StdEncoding.DecodeString(body.String())
			if err != nil {
				return nil, fmt.Errorf("armor: %w", err)
			}
			if crc != "" {
				c, err := base64.StdEncoding.DecodeString(crc)
				if err != nil || len(c) != 3 {
					return nil, errors.New("armor: bad checksum line")
				}
				if want := uint32(c[0])<<16 | uint32(c[1])<<8 | uint32(c[2]); crc24(b) != want {
					return nil, errors.New("armor: checksum mismatch")
				}
			}
			out = append(out, b...)
			found = true
			inBlock = false
		case !inBody:
			// Armor headers end at the first empty line. A block without
			// headers can start with data at once.
			if line == "" {
				inBody = true
			} else if !strings.Contains(line, ": ") {
				inBody = true
				body.WriteString(line)
			}
		case strings.HasPrefix(line, "=") && len(line) == 5:
			crc = line[1:]
		default:
			body.WriteString(line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if inBlock {
		return nil, errors.New("armor: the block has no END line")
	}
	if !found {
		return nil, errors.New("no OpenPGP data: want an armored block or binary packets")
	}
	return out, nil
}

// Armor gives data as one armored block of the type, for example "PUBLIC
// KEY BLOCK", with 64 columns and a checksum.
func Armor(data []byte, blockType string) []byte {
	var b bytes.Buffer
	b.WriteString("-----BEGIN PGP " + blockType + "-----\n\n")
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	if enc != "" {
		b.WriteString(enc + "\n")
	}
	c := crc24(data)
	b.WriteString("=" + base64.StdEncoding.EncodeToString([]byte{byte(c >> 16), byte(c >> 8), byte(c)}) + "\n")
	b.WriteString("-----END PGP " + blockType + "-----\n")
	return b.Bytes()
}

func crc24(data []byte) uint32 {
	const init, poly = 0xB704CE, 0x1864CFB
	crc := uint32(init)
	for _, x := range data {
		crc ^= uint32(x) << 16
		for range 8 {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= poly
			}
		}
	}
	return crc & 0xFFFFFF
}

// packet is one OpenPGP packet.
type packet struct {
	tag  int
	body []byte
	raw  []byte // header and body
}

// readPackets splits binary data into packets.
func readPackets(data []byte) ([]packet, error) {
	var out []packet
	for len(data) > 0 {
		start := data
		h := data[0]
		if h&0x80 == 0 {
			return nil, fmt.Errorf("packet: bad header byte 0x%02x", h)
		}
		var tag int
		var body []byte
		if h&0x40 != 0 {
			tag = int(h & 0x3f)
			data = data[1:]
			var chunks []byte
			for {
				n, partial, rest, err := newLength(data)
				if err != nil {
					return nil, err
				}
				if n > len(rest) {
					return nil, errors.New("packet: truncated")
				}
				chunks = append(chunks, rest[:n]...)
				data = rest[n:]
				if !partial {
					break
				}
			}
			body = chunks
		} else {
			tag = int(h>>2) & 0xf
			lt := h & 3
			data = data[1:]
			var n int
			switch lt {
			case 0:
				if len(data) < 1 {
					return nil, errors.New("packet: truncated")
				}
				n, data = int(data[0]), data[1:]
			case 1:
				if len(data) < 2 {
					return nil, errors.New("packet: truncated")
				}
				n, data = int(binary.BigEndian.Uint16(data)), data[2:]
			case 2:
				if len(data) < 4 {
					return nil, errors.New("packet: truncated")
				}
				n, data = int(binary.BigEndian.Uint32(data)), data[4:]
			default:
				n = len(data)
			}
			if n > len(data) {
				return nil, errors.New("packet: truncated")
			}
			body, data = data[:n], data[n:]
		}
		out = append(out, packet{tag: tag, body: body, raw: start[:len(start)-len(data)]})
	}
	return out, nil
}

func newLength(data []byte) (n int, partial bool, rest []byte, err error) {
	if len(data) < 1 {
		return 0, false, nil, errors.New("packet: truncated length")
	}
	o := int(data[0])
	switch {
	case o < 192:
		return o, false, data[1:], nil
	case o < 224:
		if len(data) < 2 {
			return 0, false, nil, errors.New("packet: truncated length")
		}
		return (o-192)<<8 + int(data[1]) + 192, false, data[2:], nil
	case o == 255:
		if len(data) < 5 {
			return 0, false, nil, errors.New("packet: truncated length")
		}
		return int(binary.BigEndian.Uint32(data[1:5])), false, data[5:], nil
	default:
		return 1 << (o & 0x1f), true, data[1:], nil
	}
}

// keyMaterial is a parsed public key packet.
type keyMaterial struct {
	version     int
	created     time.Time
	algo        int
	bits        int
	curve       string
	fingerprint string
	keyID       string
}

func parseKeyPacket(body []byte) (*keyMaterial, error) {
	if len(body) < 6 {
		return nil, errors.New("public key packet: too short")
	}
	k := &keyMaterial{version: int(body[0])}
	switch k.version {
	case 4:
		k.created = time.Unix(int64(binary.BigEndian.Uint32(body[1:5])), 0).UTC()
		k.algo = int(body[5])
		k.parseMaterial(body[6:])
		sum := sha1.Sum(append([]byte{0x99, byte(len(body) >> 8), byte(len(body))}, body...)) //nolint:gosec // v4 fingerprint
		k.fingerprint = strings.ToUpper(hex.EncodeToString(sum[:]))
		k.keyID = k.fingerprint[24:]
	case 5, 6:
		if len(body) < 10 {
			return nil, errors.New("public key packet: too short")
		}
		k.created = time.Unix(int64(binary.BigEndian.Uint32(body[1:5])), 0).UTC()
		k.algo = int(body[5])
		k.parseMaterial(body[10:])
		prefix := byte(0x9a)
		if k.version == 6 {
			prefix = 0x9b
		}
		l := len(body)
		sum := sha256.Sum256(append([]byte{prefix, byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)}, body...))
		k.fingerprint = strings.ToUpper(hex.EncodeToString(sum[:]))
		k.keyID = k.fingerprint[:16]
	case 2, 3:
		if len(body) < 8 {
			return nil, errors.New("public key packet: too short")
		}
		k.created = time.Unix(int64(binary.BigEndian.Uint32(body[1:5])), 0).UTC()
		k.algo = int(body[7])
		k.parseMaterial(body[8:])
	default:
		return nil, fmt.Errorf("public key packet: version %d", k.version)
	}
	return k, nil
}

// parseMaterial reads the size and the curve of the key material.
func (k *keyMaterial) parseMaterial(m []byte) {
	switch k.algo {
	case AlgoRSA, AlgoRSASign, AlgoDSA, AlgoElGamal:
		if len(m) >= 2 {
			k.bits = int(binary.BigEndian.Uint16(m))
		}
	case AlgoEdDSA, AlgoECDSA, AlgoECDH:
		if len(m) >= 1 && len(m) >= 1+int(m[0]) {
			oid := m[1 : 1+int(m[0])]
			k.curve = curveName(oid)
			if k.curve == "ed25519" || k.curve == "cv25519" {
				k.bits = 255
			}
		}
	case AlgoEd25519, AlgoX25519:
		k.bits = 255
		if k.algo == AlgoEd25519 {
			k.curve = "ed25519"
		} else {
			k.curve = "cv25519"
		}
	case AlgoEd448, AlgoX448:
		k.bits = 448
	}
}

func curveName(oid []byte) string {
	switch hex.EncodeToString(oid) {
	case hex.EncodeToString(oidEd25519):
		return "ed25519"
	case "2b060104019755010501":
		return "cv25519"
	case "2a8648ce3d030107":
		return "nistp256"
	case "2b81040022":
		return "nistp384"
	case "2b81040023":
		return "nistp521"
	}
	return "oid" + hex.EncodeToString(oid)
}

// ParseSignature reads one signature packet (the body without its header,
// or with it).
func ParseSignature(data []byte) (*Signature, error) {
	if len(data) > 0 && data[0]&0x80 != 0 {
		pkts, err := readPackets(data)
		if err != nil {
			return nil, err
		}
		for _, p := range pkts {
			if p.tag == tagSignature {
				return parseSigPacket(p.body)
			}
		}
		return nil, errors.New("no signature packet")
	}
	return parseSigPacket(data)
}

func parseSigPacket(b []byte) (*Signature, error) {
	if len(b) < 1 {
		return nil, errors.New("signature packet: empty")
	}
	s := &Signature{Version: int(b[0])}
	switch s.Version {
	case 3:
		if len(b) < 19 || b[1] != 5 {
			return nil, errors.New("signature packet v3: too short")
		}
		s.Type = int(b[2])
		s.Created = time.Unix(int64(binary.BigEndian.Uint32(b[3:7])), 0).UTC()
		s.IssuerKeyID = strings.ToUpper(hex.EncodeToString(b[7:15]))
		s.Algo = int(b[15])
		s.Hash = int(b[16])
		return s, nil
	case 4, 5, 6:
		if len(b) < 6 {
			return nil, errors.New("signature packet: too short")
		}
		s.Type, s.Algo, s.Hash = int(b[1]), int(b[2]), int(b[3])
		rest := b[4:]
		lenSize := 2
		if s.Version == 6 {
			lenSize = 4
		}
		for i := range 2 {
			if len(rest) < lenSize {
				return nil, errors.New("signature packet: truncated subpackets")
			}
			var n int
			if lenSize == 2 {
				n = int(binary.BigEndian.Uint16(rest))
			} else {
				n = int(binary.BigEndian.Uint32(rest))
			}
			rest = rest[lenSize:]
			if n > len(rest) {
				return nil, errors.New("signature packet: truncated subpackets")
			}
			if err := s.parseSubpackets(rest[:n], i == 0); err != nil {
				return nil, err
			}
			rest = rest[n:]
		}
		return s, nil
	}
	return nil, fmt.Errorf("signature packet: version %d", s.Version)
}

func (s *Signature) parseSubpackets(b []byte, hashed bool) error {
	for len(b) > 0 {
		n, _, rest, err := newLength(b)
		if err != nil || n < 1 || n > len(rest) {
			return errors.New("signature packet: bad subpacket")
		}
		typ := int(rest[0] & 0x7f)
		data := rest[1:n]
		b = rest[n:]
		switch typ {
		case 2:
			if hashed && len(data) == 4 {
				s.Created = time.Unix(int64(binary.BigEndian.Uint32(data)), 0).UTC()
			}
		case 9:
			if hashed && len(data) == 4 {
				s.KeyExpiry = time.Duration(binary.BigEndian.Uint32(data)) * time.Second
			}
		case 16:
			if len(data) == 8 {
				s.IssuerKeyID = strings.ToUpper(hex.EncodeToString(data))
			}
		case 25:
			if hashed && len(data) == 1 {
				s.Primary = data[0] != 0
			}
		case 27:
			if hashed && len(data) >= 1 {
				s.KeyFlags = int(data[0])
				s.hasFlags = true
			}
		case 33:
			if len(data) >= 21 {
				s.IssuerFingerprint = strings.ToUpper(hex.EncodeToString(data[1:]))
			}
		}
	}
	return nil
}

// ParseCerts reads the certificates of an armored or binary key file. A
// file with a secret key is an error: a key file holds public keys only.
func ParseCerts(data []byte) ([]*Cert, error) {
	bin, err := Dearmor(data)
	if err != nil {
		return nil, err
	}
	pkts, err := readPackets(bin)
	if err != nil {
		return nil, err
	}
	var out []*Cert
	var cur *certBuilder
	for _, p := range pkts {
		switch p.tag {
		case tagSecretKey, tagSecretSubkey:
			return nil, errors.New("the file holds a secret key; give the public key (gpg --export --armor FPR)")
		case tagPublicKey:
			if cur != nil {
				out = append(out, cur.finish())
			}
			k, err := parseKeyPacket(p.body)
			if err != nil {
				return nil, err
			}
			cur = &certBuilder{cert: &Cert{
				Fingerprint: k.fingerprint, KeyID: k.keyID, Version: k.version, Algo: k.algo,
				Bits: k.bits, Curve: k.curve, Created: k.created,
			}}
			cur.where = whereKey
		case tagUserID:
			if cur == nil {
				return nil, errors.New("a user id packet before the public key")
			}
			cur.uids = append(cur.uids, uidSigs{uid: string(p.body)})
			cur.where = whereUID
		case tagUserAttribute:
			if cur != nil {
				cur.where = whereAttr
			}
		case tagPublicSubkey:
			if cur == nil {
				return nil, errors.New("a subkey packet before the public key")
			}
			k, err := parseKeyPacket(p.body)
			if err != nil {
				return nil, err
			}
			cur.subs = append(cur.subs, subSigs{sub: Subkey{Fingerprint: k.fingerprint, KeyID: k.keyID, Algo: k.algo, Bits: k.bits, Curve: k.curve, Created: k.created}})
			cur.where = whereSub
		case tagSignature:
			if cur == nil {
				return nil, errors.New("a signature packet before the public key")
			}
			sig, err := parseSigPacket(p.body)
			if err != nil {
				return nil, err
			}
			cur.add(sig)
		default:
			// Trust packets and others carry nothing that repoctl needs.
			continue
		}
		if cur != nil {
			cur.raw = append(cur.raw, p.raw...)
		}
	}
	if cur != nil {
		out = append(out, cur.finish())
	}
	if len(out) == 0 {
		return nil, errors.New("no public key in the file")
	}
	return out, nil
}

// ParseCert reads a key file that holds exactly one certificate.
func ParseCert(data []byte) (*Cert, error) {
	certs, err := ParseCerts(data)
	if err != nil {
		return nil, err
	}
	if len(certs) != 1 {
		return nil, fmt.Errorf("the file holds %d keys; want one", len(certs))
	}
	return certs[0], nil
}

// CountSignatures gives the number of signature packets of an armored or
// binary signature file, for example repomd.xml.asc.
func CountSignatures(data []byte) (int, error) {
	bin, err := Dearmor(data)
	if err != nil {
		return 0, err
	}
	pkts, err := readPackets(bin)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pkts {
		if p.tag == tagSignature {
			n++
		}
	}
	return n, nil
}

type where int

const (
	whereKey where = iota
	whereUID
	whereAttr
	whereSub
)

type uidSigs struct {
	uid    string
	newest *Signature
}

type subSigs struct {
	sub    Subkey
	newest *Signature
}

type certBuilder struct {
	cert   *Cert
	where  where
	uids   []uidSigs
	subs   []subSigs
	direct *Signature
	raw    []byte
}

// self reports a signature that the primary key made.
func (b *certBuilder) self(s *Signature) bool {
	c := b.cert
	if s.IssuerFingerprint != "" {
		return s.IssuerFingerprint == c.Fingerprint
	}
	return s.IssuerKeyID == "" || s.IssuerKeyID == c.KeyID
}

func newer(old, s *Signature) bool { return old == nil || !s.Created.Before(old.Created) }

func (b *certBuilder) add(s *Signature) {
	if !b.self(s) {
		return
	}
	switch b.where {
	case whereKey:
		switch s.Type {
		case sigTypeKeyRev:
			b.cert.Revoked = true
		case sigTypeDirect:
			if newer(b.direct, s) {
				b.direct = s
			}
		}
	case whereUID:
		if s.Type >= 0x10 && s.Type <= 0x13 {
			u := &b.uids[len(b.uids)-1]
			if newer(u.newest, s) {
				u.newest = s
			}
		}
	case whereSub:
		sb := &b.subs[len(b.subs)-1]
		switch s.Type {
		case sigTypeSubBind:
			if newer(sb.newest, s) {
				sb.newest = s
			}
		case sigTypeSubRev:
			sb.sub.Revoked = true
		}
	case whereAttr:
	}
}

func (b *certBuilder) finish() *Cert {
	c := b.cert
	c.Raw = b.raw
	var primary *Signature
	for i, u := range b.uids {
		c.UIDs = append(c.UIDs, u.uid)
		if u.newest == nil {
			continue
		}
		c.SelfSigHashes = append(c.SelfSigHashes, u.newest.Hash)
		if primary == nil || (u.newest.Primary && !primary.Primary) {
			primary = u.newest
			c.UID = b.uids[i].uid
		}
	}
	if c.UID == "" && len(c.UIDs) > 0 {
		c.UID = c.UIDs[0]
	}
	if b.direct != nil {
		c.SelfSigHashes = append(c.SelfSigHashes, b.direct.Hash)
		if primary == nil {
			primary = b.direct
		}
	}
	if primary != nil && primary.KeyExpiry > 0 {
		c.Expires = c.Created.Add(primary.KeyExpiry)
	}
	for _, s := range b.subs {
		sk := s.sub
		if s.newest != nil {
			c.SelfSigHashes = append(c.SelfSigHashes, s.newest.Hash)
			if s.newest.KeyExpiry > 0 {
				sk.Expires = sk.Created.Add(s.newest.KeyExpiry)
			}
			sk.CanSign = s.newest.hasFlags && s.newest.KeyFlags&0x02 != 0
		}
		c.Subkeys = append(c.Subkeys, sk)
	}
	return c
}
