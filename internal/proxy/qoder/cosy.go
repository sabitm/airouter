package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Creds carries the identity fields COSY signing needs.
type Creds struct {
	UserID    string
	AuthToken string
	Name      string
	Email     string
	MachineID string
}

var (
	rsaPubOnce sync.Once
	rsaPub     *rsa.PublicKey
	rsaPubErr  error
)

func loadRSAPublicKey() (*rsa.PublicKey, error) {
	rsaPubOnce.Do(func() {
		block, _ := pem.Decode([]byte(rsaPublicKeyPEM))
		if block == nil {
			rsaPubErr = fmt.Errorf("qoder: failed to decode RSA public key PEM")
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			rsaPubErr = fmt.Errorf("qoder: parse RSA public key: %w", err)
			return
		}
		var ok bool
		rsaPub, ok = pub.(*rsa.PublicKey)
		if !ok {
			rsaPubErr = fmt.Errorf("qoder: not an RSA public key")
		}
	})
	return rsaPub, rsaPubErr
}

// BuildCosyHeaders produces the Cosy-* / Authorization header set for body.
// body must be the exact bytes that will be sent on the wire.
func BuildCosyHeaders(body []byte, requestURL string, creds Creds) (map[string]string, error) {
	if strings.TrimSpace(creds.UserID) == "" {
		return nil, fmt.Errorf("qoder: cosy user id is empty")
	}
	if strings.TrimSpace(creds.AuthToken) == "" {
		return nil, fmt.Errorf("qoder: cosy auth token is empty")
	}
	if body == nil {
		body = []byte{}
	}

	cosyKey, info, err := encryptUserInfo(map[string]string{
		"uid":                   creds.UserID,
		"security_oauth_token":  creds.AuthToken,
		"name":                  creds.Name,
		"aid":                   "",
		"email":                 creds.Email,
	})
	if err != nil {
		return nil, err
	}

	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	requestID := newUUID()

	payloadJSON, err := json.Marshal(map[string]any{
		"version":     "v1",
		"requestId":   requestID,
		"info":        info,
		"cosyVersion": IDEVersion,
		"ideVersion":  "",
	})
	if err != nil {
		return nil, err
	}
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)

	sigPath := computeSigPath(requestURL)
	// MD5 over latin1 concatenation — body bytes are already 0-255 wire bytes.
	sigInput := make([]byte, 0, len(payloadB64)+len(cosyKey)+len(timestamp)+len(body)+len(sigPath)+4)
	sigInput = append(sigInput, payloadB64...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, cosyKey...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, timestamp...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, body...)
	sigInput = append(sigInput, '\n')
	sigInput = append(sigInput, sigPath...)
	sig := md5Hex(sigInput)

	machineID := creds.MachineID
	if machineID == "" {
		machineID = newUUID()
	}
	bodyHash := md5Hex(body)
	bodyLength := fmt.Sprintf("%d", len(body))

	return map[string]string{
		"Authorization":           "Bearer COSY." + payloadB64 + "." + sig,
		"Cosy-Key":                cosyKey,
		"Cosy-User":               creds.UserID,
		"Cosy-Date":               timestamp,
		"Cosy-Version":            IDEVersion,
		"Cosy-Machineid":          machineID,
		"Cosy-Machinetoken":       machineID,
		"Cosy-Machinetype":        MachineType,
		"Cosy-Machineos":          MachineOS,
		"Cosy-Clienttype":         ClientType,
		"Cosy-Clientip":           "127.0.0.1",
		"Cosy-Bodyhash":           bodyHash,
		"Cosy-Bodylength":         bodyLength,
		"Cosy-Sigpath":            sigPath,
		"Cosy-Data-Policy":        DataPolicy,
		"Cosy-Organization-Id":    "",
		"Cosy-Organization-Tags":  "",
		"Login-Version":           LoginVersion,
		"X-Request-Id":            newUUID(),
	}, nil
}

func encryptUserInfo(userInfo map[string]string) (cosyKeyB64, infoB64 string, err error) {
	aesKey := generateAESKey()
	plain, err := json.Marshal(userInfo)
	if err != nil {
		return "", "", err
	}
	infoB64, err = aesEncryptCBCBase64(plain, aesKey)
	if err != nil {
		return "", "", err
	}
	cosyKeyB64, err = rsaEncryptBase64(aesKey)
	if err != nil {
		return "", "", err
	}
	return cosyKeyB64, infoB64, nil
}

// generateAESKey matches qodercli: first 16 chars of a UUID string (hyphens included).
func generateAESKey() string {
	return newUUID()[:16]
}

func aesEncryptCBCBase64(plaintext []byte, keyStr string) (string, error) {
	key := []byte(keyStr)
	if len(key) != 16 {
		return "", fmt.Errorf("qoder: aes key must be 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	// IV reuses the key bytes (qodercli/Veria).
	iv := key[:aes.BlockSize]
	mode := cipher.NewCBCEncrypter(block, iv)
	out := make([]byte, len(padded))
	mode.CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func rsaEncryptBase64(data string) (string, error) {
	pub, err := loadRSAPublicKey()
	if err != nil {
		return "", err
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(data))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - (len(data) % blockSize)
	out := make([]byte, len(data)+padding)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(padding)
	}
	return out
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// computeSigPath strips the leading /algo prefix from the request path.
func computeSigPath(requestURL string) string {
	u, err := url.Parse(requestURL)
	if err != nil {
		return ""
	}
	path := u.Path
	if strings.HasPrefix(path, "/algo") {
		return path[len("/algo"):]
	}
	return path
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
