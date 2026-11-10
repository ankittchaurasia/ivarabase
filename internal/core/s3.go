package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3Config targets any S3-compatible store (AWS, Cloudflare R2, Backblaze B2,
// MinIO, Hetzner, Wasabi). Requests are signed with SigV4; no SDK needed.
type S3Config struct {
	Endpoint  string `json:"endpoint"` // https://s3.eu-central-1.amazonaws.com, https://<acct>.r2.cloudflarestorage.com
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

func (s S3Config) configured() bool {
	return s.Endpoint != "" && s.Bucket != "" && s.AccessKey != "" && s.SecretKey != ""
}

func (s S3Config) key(name string) string {
	p := strings.Trim(s.Prefix, "/")
	if p == "" {
		return name
	}
	return p + "/" + name
}

var s3Client = &http.Client{Timeout: 0}

func s3Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func (s S3Config) request(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64) (*http.Response, error) {
	return s.requestWith(ctx, method, key, query, body, size, nil)
}

func (s S3Config) requestWith(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, extra map[string]string) (*http.Response, error) {
	region := s.Region
	if region == "" {
		region = "us-east-1"
	}
	ep, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil || ep.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", s.Endpoint)
	}
	segs := []string{s3Escape(s.Bucket)}
	if key != "" {
		for _, part := range strings.Split(key, "/") {
			segs = append(segs, s3Escape(part))
		}
	}
	canonURI := "/" + strings.Join(segs, "/")
	var qparts []string
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range query[k] {
			qparts = append(qparts, s3Escape(k)+"="+s3Escape(v))
		}
	}
	canonQuery := strings.Join(qparts, "&")
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	payload := "UNSIGNED-PAYLOAD"
	if body == nil {
		payload = hex.EncodeToString(sha256.New().Sum(nil))
	}
	host := ep.Host
	canonHeaders := "host:" + host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amzDate + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonReq := strings.Join([]string{method, canonURI, canonQuery, canonHeaders, signed, payload}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(canonReq))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	k := hmacSHA256([]byte("AWS4"+s.SecretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))

	u := ep.Scheme + "://" + host + canonURI
	if canonQuery != "" {
		u += "?" + canonQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, v := range extra {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payload)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", s.AccessKey, scope, signed, sig))
	resp, err := s3Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("S3 %s %s: %s %s", method, key, resp.Status, s3ErrMessage(b))
	}
	return resp, nil
}

func s3ErrMessage(b []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(b))
}

func (s S3Config) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	resp, err := s.request(ctx, "PUT", s.key(key), nil, f, st.Size())
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s S3Config) PutBytes(ctx context.Context, key string, b []byte) error {
	resp, err := s.request(ctx, "PUT", s.key(key), nil, strings.NewReader(string(b)), int64(len(b)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s S3Config) GetToFile(ctx context.Context, key, path string) error {
	resp, err := s.request(ctx, "GET", s.key(key), nil, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s S3Config) Delete(ctx context.Context, key string) error {
	resp, err := s.request(ctx, "DELETE", s.key(key), nil, nil, 0)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type s3Object struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

func (s S3Config) List(ctx context.Context) ([]s3Object, error) {
	var out []s3Object
	token := ""
	prefix := strings.Trim(s.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	for {
		q := url.Values{"list-type": {"2"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.request(ctx, "GET", "", q, nil, 0)
		if err != nil {
			return nil, err
		}
		var res struct {
			Contents    []s3Object `xml:"Contents"`
			IsTruncated bool       `xml:"IsTruncated"`
			NextToken   string     `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, o := range res.Contents {
			o.Key = strings.TrimPrefix(o.Key, prefix)
			out = append(out, o)
		}
		if !res.IsTruncated || res.NextToken == "" {
			return out, nil
		}
		token = res.NextToken
	}
}

// Files above s3MultipartMin go up in parts: S3 refuses a single PUT over
// 5 GB, and a failed part is retried alone instead of resending everything.
const (
	s3MultipartMin = 100 << 20
	s3PartSize     = 16 << 20
)

type s3Part struct {
	N    int    `xml:"PartNumber"`
	ETag string `xml:"ETag"`
}

// putMultipart uploads size bytes of r to the full key (prefix applied).
func (s S3Config) putMultipart(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	part := int64(s3PartSize)
	for (size+part-1)/part > 10000 { // S3 allows at most 10,000 parts
		part *= 2
	}
	resp, err := s.request(ctx, "POST", key, url.Values{"uploads": {""}}, nil, 0)
	if err != nil {
		return err
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadID == "" {
		return fmt.Errorf("S3 multipart %s: no upload id (%v)", key, err)
	}
	id := init.UploadID
	abort := func() {
		actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if resp, err := s.request(actx, "DELETE", key, url.Values{"uploadId": {id}}, nil, 0); err == nil {
			resp.Body.Close()
		}
	}
	var parts []s3Part
	for n, off := 1, int64(0); off < size; n, off = n+1, off+part {
		ln := min(part, size-off)
		for try := 0; ; try++ {
			resp, err := s.request(ctx, "PUT", key, url.Values{"partNumber": {fmt.Sprint(n)}, "uploadId": {id}}, io.NewSectionReader(r, off, ln), ln)
			if err == nil {
				parts = append(parts, s3Part{n, resp.Header.Get("ETag")})
				resp.Body.Close()
				break
			}
			if try == 2 || ctx.Err() != nil {
				abort()
				return err
			}
			time.Sleep(time.Second << try)
		}
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []s3Part `xml:"Part"`
	}{Parts: parts})
	resp, err = s.request(ctx, "POST", key, url.Values{"uploadId": {id}}, strings.NewReader(string(body)), int64(len(body)))
	if err != nil {
		abort()
		return err
	}
	defer resp.Body.Close()
	// S3 can answer 200 and still report a failed completion in the body
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if strings.Contains(string(b), "<Error>") {
		abort()
		return fmt.Errorf("S3 multipart %s: %s", key, s3ErrMessage(b))
	}
	return nil
}
