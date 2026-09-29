// Package access manages login accounts (reference 5.1 system login) and
// password hashing.
package access

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"strconv"
	"strings"
)

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// HashPassword returns a SHA-512 crypt ($6$) hash of password with a random
// 16-character salt and the default 5000 rounds.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	rnd := make([]byte, 16)
	if _, err := rand.Read(rnd); err != nil {
		return "", err
	}
	for i := range salt {
		salt[i] = cryptAlphabet[int(rnd[i])%len(cryptAlphabet)]
	}
	return SHA512Crypt(password, "$6$"+string(salt))
}

// SHA512Crypt implements Ulrich Drepper's SHA-512 crypt. setting is
// "$6$[rounds=N$]salt[$…]".
func SHA512Crypt(password, setting string) (string, error) {
	if !strings.HasPrefix(setting, "$6$") {
		return "", errors.New("not a $6$ setting")
	}
	rest := setting[3:]
	rounds, custom := 5000, false
	if strings.HasPrefix(rest, "rounds=") {
		n, after, ok := strings.Cut(rest[7:], "$")
		if !ok {
			return "", errors.New("malformed rounds")
		}
		r, err := strconv.Atoi(n)
		if err != nil {
			return "", errors.New("malformed rounds")
		}
		rounds, custom, rest = min(max(r, 1000), 999999999), true, after
	}
	salt, _, _ := strings.Cut(rest, "$")
	if len(salt) > 16 {
		salt = salt[:16]
	}
	pw, sl := []byte(password), []byte(salt)

	b := sha512.New()
	b.Write(pw)
	b.Write(sl)
	b.Write(pw)
	sumB := b.Sum(nil)

	a := sha512.New()
	a.Write(pw)
	a.Write(sl)
	for i := len(pw); i > 0; i -= 64 {
		a.Write(sumB[:min(i, 64)])
	}
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			a.Write(sumB)
		} else {
			a.Write(pw)
		}
	}
	sumA := a.Sum(nil)

	dp := sha512.New()
	for range pw {
		dp.Write(pw)
	}
	sumDP := dp.Sum(nil)
	p := make([]byte, 0, len(pw))
	for len(p) < len(pw) {
		p = append(p, sumDP[:min(64, len(pw)-len(p))]...)
	}

	ds := sha512.New()
	for i := 0; i < 16+int(sumA[0]); i++ {
		ds.Write(sl)
	}
	sumDS := ds.Sum(nil)
	s := sumDS[:len(sl)]

	c := sumA
	for i := 0; i < rounds; i++ {
		h := sha512.New()
		if i&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	if custom {
		out.WriteString("rounds=" + strconv.Itoa(rounds) + "$")
	}
	out.WriteString(salt)
	out.WriteByte('$')
	enc := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for ; n > 0; n-- {
			out.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	order := [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
		{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41},
	}
	for _, o := range order {
		enc(c[o[0]], c[o[1]], c[o[2]], 4)
	}
	enc(0, 0, c[63], 2)
	return out.String(), nil
}

// CheckPassword reports whether password matches a $6$ hash.
func CheckPassword(password, hash string) bool {
	got, err := SHA512Crypt(password, hash)
	return err == nil && got == hash
}
