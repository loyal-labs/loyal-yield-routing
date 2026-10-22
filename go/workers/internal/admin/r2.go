package admin

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type R2Config struct {
	Endpoint        *url.URL
	Bucket          string
	Prefix          string
	CDNBase         *url.URL
	AccessKeyID     string
	SecretAccessKey string
}

const r2CacheControl = "public, max-age=31536000, immutable"

func awsEscape(segment string) string {
	var out strings.Builder
	for _, b := range []byte(segment) {
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '_', b == '.', b == '~':
			out.WriteByte(b)
		default:
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String()
}

func escapeObjectPath(key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = awsEscape(segment)
	}
	return strings.Join(segments, "/")
}

func (c *R2Config) objectKey(name string) string {
	if c.Prefix == "" {
		return name
	}
	return c.Prefix + "/" + name
}

func (c *R2Config) objectRequest(key string, body []byte, contentType string) (*http.Request, error) {
	target := strings.TrimRight(c.Endpoint.String(), "/") + "/" + awsEscape(c.Bucket) + "/" + escapeObjectPath(key)
	request, err := http.NewRequest(http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Cache-Control", r2CacheControl)
	sum := sha256.Sum256(body)
	signV4(request, hex.EncodeToString(sum[:]), c.AccessKeyID, c.SecretAccessKey, "auto", "s3", time.Now().UTC())
	return request, nil
}

func (c *R2Config) publicURL(key string) string {
	base := strings.TrimRight(c.CDNBase.String(), "/")
	return base + "/" + escapeObjectPath(key)
}

func newObjectID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func signV4(request *http.Request, payloadHash, accessKeyID, secretAccessKey, region, service string, now time.Time) {
	stamp := now.Format("20060102T150405Z")
	day := stamp[:8]
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	request.Header.Set("X-Amz-Date", stamp)

	headers := map[string]string{"host": request.URL.Host}
	for name, values := range request.Header {
		headers[strings.ToLower(name)] = strings.TrimSpace(strings.Join(values, ","))
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		request.Method,
		request.URL.EscapedPath(),
		request.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	canonicalSum := sha256.Sum256([]byte(canonical))
	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(canonicalSum[:])

	key := hmacSHA256([]byte("AWS4"+secretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))

	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}
