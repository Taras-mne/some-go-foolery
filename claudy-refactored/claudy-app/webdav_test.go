package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDynamicDAV_NotConfigured(t *testing.T) {
	d := &dynamicDAV{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dav/u/file.txt", nil)
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestDynamicDAV_GetFile(t *testing.T) {
	dir := t.TempDir()
	const content = "hello"
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	d := newDynamicDAV("/dav/u", dir)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dav/u/file.txt", nil)
	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != content {
		t.Errorf("body = %q, want %q", got, content)
	}
}

func TestDynamicDAV_SetDirSwitchesFileSystem(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	_ = os.WriteFile(filepath.Join(a, "x.txt"), []byte("A"), 0644)
	_ = os.WriteFile(filepath.Join(b, "x.txt"), []byte("B"), 0644)

	d := newDynamicDAV("/dav/u", a)

	req := httptest.NewRequest(http.MethodGet, "/dav/u/x.txt", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if got := strings.TrimSpace(rec.Body.String()); got != "A" {
		t.Fatalf("before switch = %q", got)
	}

	d.SetDir("/dav/u", b)

	req = httptest.NewRequest(http.MethodGet, "/dav/u/x.txt", nil)
	rec = httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if got := strings.TrimSpace(rec.Body.String()); got != "B" {
		t.Fatalf("after switch = %q", got)
	}
}

func TestDynamicDAV_Put(t *testing.T) {
	dir := t.TempDir()
	d := newDynamicDAV("/dav/u", dir)

	body := strings.NewReader("new content")
	req := httptest.NewRequest(http.MethodPut, "/dav/u/upload.txt", body)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status = %d", rec.Code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "upload.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "new content" {
		t.Errorf("content = %q", got)
	}
}
