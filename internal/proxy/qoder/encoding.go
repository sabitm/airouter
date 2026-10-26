package qoder

import (
	"encoding/base64"
)

// WAF-bypass body encoding from 9router encoding.js / qoder2api:
// standard base64 → rearrange thirds as [tail][mid][head] → custom alphabet.

const (
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
)

var s2c [128]byte

func init() {
	for i := range s2c {
		s2c[i] = 0xff
	}
	for i := 0; i < 64; i++ {
		s2c[stdAlphabet[i]] = customAlphabet[i]
	}
	s2c['='] = '$'
}

// EncodeBody applies Qoder's WAF-bypass transform. The result must be posted
// with Encode=1 on the URL and is the exact byte sequence COSY signs.
func EncodeBody(plaintext []byte) []byte {
	if len(plaintext) == 0 {
		return nil
	}
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	// [tail][mid][head]
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c < 128 && s2c[c] != 0xff {
			out[i] = s2c[c]
		} else {
			out[i] = c
		}
	}
	return out
}

// DecodeWireBody reverses EncodeBody. Tests use it to inspect the plaintext
// behind a captured wire body; it exists so assertions can verify identity
// fields without duplicating the alphabet transform.
func DecodeWireBody(wire []byte) ([]byte, error) {
	if len(wire) == 0 {
		return nil, nil
	}
	var c2s [128]byte
	for i := range c2s {
		c2s[i] = 0xff
	}
	for i := 0; i < 64; i++ {
		c2s[customAlphabet[i]] = stdAlphabet[i]
	}
	c2s['$'] = '='
	std := make([]byte, len(wire))
	for i, c := range wire {
		if c < 128 && c2s[c] != 0xff {
			std[i] = c2s[c]
		} else {
			std[i] = c
		}
	}
	n := len(std)
	a := n / 3
	// original = head + mid + tail where wire = tail + mid + head
	reordered := string(std[n-a:]) + string(std[a:n-a]) + string(std[:a])
	return base64.StdEncoding.DecodeString(reordered)
}
