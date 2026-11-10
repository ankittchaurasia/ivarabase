package core

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestTusMetadata(t *testing.T) {
	m, err := parseTusMetadata("bucketName ZG9jcw==, objectName YS9iLnR4dA,empty")
	if err != nil || m["bucketName"] != "docs" || m["objectName"] != "a/b.txt" || m["empty"] != "" {
		t.Fatalf("metadata: %v %v", m, err)
	}
	if _, err := parseTusMetadata("bucketName ***"); err == nil {
		t.Fatal("invalid base64 must be refused")
	}
}

func TestTusSweep(t *testing.T) {
	a := &App{Cfg: &Config{DataDir: t.TempDir()}}
	now := time.Now()
	mk := func(slug string, expires time.Time) *tusUpload {
		u := &tusUpload{Version: randUUID(), Project: slug, Expires: expires}
		if err := a.tusSave(u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	live := mk("p1", now.Add(time.Hour))
	old := mk("p1", now.Add(-time.Minute))
	gone := mk("deleted", now.Add(time.Hour))
	orphan := filepath.Join(a.tusDir("p1"), randUUID()+".bin")
	os.WriteFile(orphan, []byte("x"), 0o600)
	os.Chtimes(orphan, now.Add(-2*time.Hour), now.Add(-2*time.Hour))

	if n := a.tusSweep(now, func(s string) bool { return s != "deleted" }); n != 1 {
		t.Fatalf("expected 1 expired upload removed, got %d", n)
	}
	exists := func(u *tusUpload, ext string) bool {
		_, err := os.Stat(a.tusFile(u.Project, u.Version, ext))
		return err == nil
	}
	if !exists(live, ".json") || !exists(live, ".bin") {
		t.Fatal("a live upload was removed")
	}
	if exists(old, ".json") || exists(old, ".bin") {
		t.Fatal("an expired upload was kept")
	}
	if _, err := os.Stat(a.tusDir(gone.Project)); err == nil {
		t.Fatal("uploads of a deleted project were kept")
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("an old data file without state was kept")
	}
}

func TestTusLoadChecksProject(t *testing.T) {
	a := &App{Cfg: &Config{DataDir: t.TempDir()}}
	v := randUUID()
	u := &tusUpload{ID: "x", Version: v, Project: "p1", Expires: time.Now().Add(time.Hour)}
	u.ID = b64url.EncodeToString([]byte("b/o/" + v))
	if err := a.tusSave(u); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tusLoad("p1", u.ID); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := a.tusLoad("p2", u.ID); err == nil {
		t.Fatal("an upload id must not resolve in another project")
	}
	if _, err := a.tusLoad("p1", b64url.EncodeToString([]byte("b/o/../../x"))); err == nil {
		t.Fatal("a non-uuid version must be refused")
	}
}

// a minimal S3 multipart server: checks the request sequence and reassembles parts
func TestS3Multipart(t *testing.T) {
	var mu sync.Mutex
	parts := map[int][]byte{}
	var final []byte
	aborted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>U1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT" && q.Get("uploadId") == "U1":
			n, _ := strconv.Atoi(q.Get("partNumber"))
			b, _ := io.ReadAll(r.Body)
			parts[n] = b
			w.Header().Set("ETag", `"e`+strconv.Itoa(n)+`"`)
		case r.Method == "POST" && q.Get("uploadId") == "U1":
			var c struct {
				Parts []s3Part `xml:"Part"`
			}
			xml.NewDecoder(r.Body).Decode(&c)
			nums := []int{}
			for _, p := range c.Parts {
				if p.ETag != `"e`+strconv.Itoa(p.N)+`"` {
					w.WriteHeader(400)
					return
				}
				nums = append(nums, p.N)
			}
			sort.Ints(nums)
			for _, n := range nums {
				final = append(final, parts[n]...)
			}
			io.WriteString(w, `<CompleteMultipartUploadResult></CompleteMultipartUploadResult>`)
		case r.Method == "DELETE":
			aborted = true
		default:
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	s := S3Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"}
	data := bytes.Repeat([]byte("0123456789abcdef"), (s3PartSize*2+12345)/16)
	if err := s.putMultipart(context.Background(), "storage/p/b/id", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || !bytes.Equal(final, data) || aborted {
		t.Fatalf("parts %d, assembled %d of %d bytes, aborted %v", len(parts), len(final), len(data), aborted)
	}
}
