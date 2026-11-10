package core

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- primitives

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pbkdf2SHA256 is RFC 8018 PBKDF2 with HMAC-SHA256 (stdlib has none before Go 1.24).
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		prf.Write(buf[:])
		dk = prf.Sum(dk)
		t := dk[len(dk)-hashLen:]
		copy(u, t)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = u[:0]
			u = prf.Sum(u)
			for x := range u {
				t[x] ^= u[x]
			}
		}
	}
	return dk[:keyLen]
}

// ---------------------------------------------------------------- passwords

const passwordIter = 210000

func hashPassword(pw string) string {
	salt := randBytes(16)
	dk := pbkdf2SHA256([]byte(pw), salt, passwordIter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk))
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------------------------------------------------------------- small secrets at rest

// seal encrypts a short value (project passwords, function secrets) with the
// master key. Output: base64(nonce || ciphertext).
func seal(key []byte, plaintext string) string {
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := randBytes(gcm.NonceSize())
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...))
}

func unseal(key []byte, enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("cannot decrypt (wrong master key?)")
	}
	return string(pt), nil
}

// ---------------------------------------------------------------- recovery key

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newRecoveryKey returns 160 random bits as XXXX-XXXX-... (32 base32 chars).
func newRecoveryKey() string {
	s := b32.EncodeToString(randBytes(20))
	var groups []string
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return strings.Join(groups, "-")
}

// normalizeRecoveryKey makes the key insensitive to case, spaces and dashes.
func normalizeRecoveryKey(k string) string {
	k = strings.ToUpper(k)
	k = strings.NewReplacer("-", "", " ", "", "\n", "", "\t", "").Replace(k)
	return k
}

// ---------------------------------------------------------------- bundle encryption
//
// Stream format (all bundles leaving the process are encrypted):
//
//	magic "IVBK1\n" | salt[16] | iter uint32 | noncePrefix[8]
//	repeated: flag[1] | len uint32 | ciphertext[len]   (flag 1 = final chunk)
//
// Each chunk is AES-256-GCM sealed with nonce = noncePrefix || counter and the
// flag byte as additional data, so reordering, truncation and tampering are all
// detected. The key is PBKDF2(recovery key, salt).

const (
	bundleMagic = "IVBK1\n"
	chunkSize   = 64 * 1024
	bundleIter  = 100000
)

type encWriter struct {
	w       io.Writer
	gcm     cipher.AEAD
	prefix  []byte
	counter uint32
	buf     []byte
	closed  bool
}

func newEncWriter(w io.Writer, recoveryKey string) (*encWriter, error) {
	salt := randBytes(16)
	prefix := randBytes(8)
	key := pbkdf2SHA256([]byte(normalizeRecoveryKey(recoveryKey)), salt, bundleIter, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 0, len(bundleMagic)+16+4+8)
	hdr = append(hdr, bundleMagic...)
	hdr = append(hdr, salt...)
	hdr = binary.BigEndian.AppendUint32(hdr, bundleIter)
	hdr = append(hdr, prefix...)
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &encWriter{w: w, gcm: gcm, prefix: prefix, buf: make([]byte, 0, chunkSize)}, nil
}

func (e *encWriter) nonce() []byte {
	n := make([]byte, 12)
	copy(n, e.prefix)
	binary.BigEndian.PutUint32(n[8:], e.counter)
	e.counter++
	return n
}

func (e *encWriter) flush(final bool) error {
	flag := byte(0)
	if final {
		flag = 1
	}
	ct := e.gcm.Seal(nil, e.nonce(), e.buf, []byte{flag})
	hdr := []byte{flag, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(ct)))
	if _, err := e.w.Write(hdr); err != nil {
		return err
	}
	_, err := e.w.Write(ct)
	e.buf = e.buf[:0]
	return err
}

func (e *encWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("write after close")
	}
	n := 0
	for len(p) > 0 {
		room := chunkSize - len(e.buf)
		take := min(room, len(p))
		e.buf = append(e.buf, p[:take]...)
		p = p[take:]
		n += take
		if len(e.buf) == chunkSize {
			if err := e.flush(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *encWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	return e.flush(true)
}

type decReader struct {
	r       *bufio.Reader
	gcm     cipher.AEAD
	prefix  []byte
	counter uint32
	buf     []byte
	done    bool
}

var errBadKey = &APIError{Status: 400, Code: "bad_recovery_key", Message: "cannot decrypt backup: wrong recovery key or corrupted file",
	Hint: "use the recovery key printed when this installation (or the old one) was initialized; it is in <data>/secrets/recovery.key"}

func newDecReader(r io.Reader, recoveryKey string) (*decReader, error) {
	br := bufio.NewReaderSize(r, 256*1024)
	hdr := make([]byte, len(bundleMagic)+16+4+8)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, fmt.Errorf("read backup header: %w", err)
	}
	if string(hdr[:len(bundleMagic)]) != bundleMagic {
		return nil, errors.New("not an IvaraBase backup (bad magic)")
	}
	off := len(bundleMagic)
	salt := hdr[off : off+16]
	iter := int(binary.BigEndian.Uint32(hdr[off+16 : off+20]))
	prefix := append([]byte(nil), hdr[off+20:off+28]...)
	if iter < 1000 || iter > 10_000_000 {
		return nil, errors.New("backup header has an invalid iteration count")
	}
	key := pbkdf2SHA256([]byte(normalizeRecoveryKey(recoveryKey)), salt, iter, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &decReader{r: br, gcm: gcm, prefix: prefix}, nil
}

func (d *decReader) next() error {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(d.r, hdr); err != nil {
		return errors.New("backup is truncated (no final chunk)")
	}
	flag := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > chunkSize+64 || flag > 1 {
		return errBadKey
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		return errors.New("backup is truncated")
	}
	nonce := make([]byte, 12)
	copy(nonce, d.prefix)
	binary.BigEndian.PutUint32(nonce[8:], d.counter)
	d.counter++
	pt, err := d.gcm.Open(nil, nonce, ct, []byte{flag})
	if err != nil {
		return errBadKey
	}
	d.buf = pt
	if flag == 1 {
		d.done = true
		// anything after the final chunk is tampering
		if _, err := d.r.Peek(1); err == nil {
			return errors.New("backup has trailing data after the final chunk")
		}
	}
	return nil
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}
