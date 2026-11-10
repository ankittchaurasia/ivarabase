package core

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func roundTrip(t *testing.T, size int) []byte {
	t.Helper()
	data := randBytes(size)
	var buf bytes.Buffer
	w, err := newEncWriter(&buf, "KEY1-KEY2")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := newDecReader(bytes.NewReader(buf.Bytes()), "key1 key2") // case/space/dash-insensitive
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("size %d: %v", size, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("size %d: round trip mismatch", size)
	}
	return buf.Bytes()
}

func TestBundleEncryptionRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		roundTrip(t, n)
	}
}

func TestBundleEncryptionDetectsTampering(t *testing.T) {
	enc := roundTrip(t, 2*chunkSize+100)
	readAll := func(b []byte, key string) error {
		r, err := newDecReader(bytes.NewReader(b), key)
		if err != nil {
			return err
		}
		_, err = io.ReadAll(r)
		return err
	}
	if err := readAll(enc, "WRONG"); !errors.Is(err, errBadKey) {
		t.Fatalf("wrong key: got %v", err)
	}
	flipped := append([]byte(nil), enc...)
	flipped[len(flipped)/2] ^= 1
	if readAll(flipped, "KEY1KEY2") == nil {
		t.Fatal("bit flip not detected")
	}
	if readAll(enc[:len(enc)-20], "KEY1KEY2") == nil {
		t.Fatal("truncation not detected")
	}
	// dropping the final chunk entirely must also fail
	if readAll(enc[:len(bundleMagic)+28+5+chunkSize+16], "KEY1KEY2") == nil {
		t.Fatal("missing final chunk not detected")
	}
	if readAll(append(append([]byte(nil), enc...), 0), "KEY1KEY2") == nil {
		t.Fatal("trailing data not detected")
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse")
	if !checkPassword(h, "correct horse") || checkPassword(h, "wrong") {
		t.Fatal("password check failed")
	}
}

func TestSQLDestructiveHeuristic(t *testing.T) {
	cases := map[string]bool{
		"select 1":                               false,
		"insert into t values (1)":               false,
		"delete from t where id = 1":             false,
		"delete from t":                          true,
		"DROP TABLE t":                           true,
		"truncate t":                             true,
		"alter table t drop column c":            true,
		"alter table t add column c int":         false,
		"update t set a=1":                       true,
		"update t set a=1 where id=2":            false,
		"select 1; -- drop table t\nselect 2":    false,
		"delete from a where x=1; delete from b": true,
	}
	for q, want := range cases {
		if got := sqlIsDestructive(q); got != want {
			t.Errorf("%q: got %v want %v", q, got, want)
		}
	}
}

func TestSlugValidation(t *testing.T) {
	for _, s := range []string{"shop", "my_app2", "abc"} {
		if validSlug(s) != nil {
			t.Errorf("%s should be valid", s)
		}
	}
	for _, s := range []string{"ab", "Shop", "1app", "my-app", "postgres", "pg_x", "ivb_x", "a;drop"} {
		if validSlug(s) == nil {
			t.Errorf("%s should be invalid", s)
		}
	}
}
