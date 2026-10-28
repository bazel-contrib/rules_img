package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	signingAlgorithm = "AWS4-HMAC-SHA256"
	// UnsignedPayload is the payload hash for requests whose body is not signed.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptyPayloadHash is the SHA-256 of the empty string.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// SignOptions configure [Sign].
type SignOptions struct {
	Service string
	Region  string
	// Time defaults to the current time.
	Time time.Time
	// PayloadHash is the hex SHA-256 of the request body, or [UnsignedPayload].
	// It defaults to [UnsignedPayload] when presigning and is required
	// otherwise.
	PayloadHash string
	// Expires, if positive, presigns the request: the signature is placed in
	// the URL query instead of the Authorization header.
	Expires time.Duration
	// S3 selects S3 semantics: the canonical path is not double-escaped and an
	// x-amz-content-sha256 header is sent.
	S3 bool
}

// Sign signs req in place with AWS Signature Version 4. Only the host,
// content-type, content-md5 and x-amz-* headers are signed.
func Sign(req *http.Request, creds Credentials, o SignOptions) error {
	if !creds.HasKeys() {
		return errors.New("aws: cannot sign request without credentials")
	}
	t := o.Time
	if t.IsZero() {
		t = time.Now()
	}
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	scope := day + "/" + o.Region + "/" + o.Service + "/aws4_request"
	presign := o.Expires > 0

	payload := o.PayloadHash
	if payload == "" {
		if !presign {
			return errors.New("aws: payload hash is required to sign a request")
		}
		payload = UnsignedPayload
	}

	query := make([][2]string, 0, 8)
	for k, vs := range req.URL.Query() {
		for _, v := range vs {
			query = append(query, [2]string{k, v})
		}
	}

	headers := map[string]string{"host": canonicalHost(req)}
	if presign {
		query = append(query,
			[2]string{"X-Amz-Algorithm", signingAlgorithm},
			[2]string{"X-Amz-Credential", creds.AccessKeyID + "/" + scope},
			[2]string{"X-Amz-Date", amzDate},
			[2]string{"X-Amz-Expires", strconv.FormatInt(int64(o.Expires/time.Second), 10)},
			[2]string{"X-Amz-SignedHeaders", "host"},
		)
		if creds.SessionToken != "" {
			query = append(query, [2]string{"X-Amz-Security-Token", creds.SessionToken})
		}
	} else {
		if req.Header == nil {
			req.Header = http.Header{}
		}
		req.Header.Set("X-Amz-Date", amzDate)
		if creds.SessionToken != "" {
			req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
		}
		if o.S3 {
			req.Header.Set("X-Amz-Content-Sha256", payload)
		}
		for k, vs := range req.Header {
			lk := strings.ToLower(k)
			if lk == "content-type" || lk == "content-md5" || strings.HasPrefix(lk, "x-amz-") {
				trimmed := make([]string, len(vs))
				for i, v := range vs {
					trimmed[i] = strings.Join(strings.Fields(v), " ")
				}
				headers[lk] = strings.Join(trimmed, ",")
			}
		}
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	canonPath := uriEncode(path, false)
	if !o.S3 {
		canonPath = uriEncode(canonPath, false)
	}

	encoded := make([][2]string, len(query))
	for i, kv := range query {
		encoded[i] = [2]string{uriEncode(kv[0], true), uriEncode(kv[1], true)}
	}
	sort.Slice(encoded, func(i, j int) bool {
		if encoded[i][0] != encoded[j][0] {
			return encoded[i][0] < encoded[j][0]
		}
		return encoded[i][1] < encoded[j][1]
	})
	pairs := make([]string, len(encoded))
	for i, kv := range encoded {
		pairs[i] = kv[0] + "=" + kv[1]
	}
	canonQuery := strings.Join(pairs, "&")

	canonical := strings.Join([]string{
		req.Method, canonPath, canonQuery, canonHeaders.String(), signedHeaders, payload,
	}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{signingAlgorithm, amzDate, scope, hex.EncodeToString(canonicalHash[:])}, "\n")

	key := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), day)
	key = hmacSHA256(key, o.Region)
	key = hmacSHA256(key, o.Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	if presign {
		req.URL.RawQuery = canonQuery + "&X-Amz-Signature=" + signature
		req.URL.RawPath = canonPath
		if !o.S3 {
			req.URL.RawPath = uriEncode(path, false)
		}
	} else {
		req.Header.Set("Authorization", signingAlgorithm+" Credential="+creds.AccessKeyID+"/"+scope+
			", SignedHeaders="+signedHeaders+", Signature="+signature)
	}
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func canonicalHost(req *http.Request) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	switch {
	case req.URL.Scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	case req.URL.Scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	}
	return host
}

// uriEncode percent-encodes s per RFC 3986, leaving '/' alone unless
// encodeSlash is set.
func uriEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
		}
	}
	return b.String()
}
