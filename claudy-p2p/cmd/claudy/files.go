// Browser-side file management API: list, download, upload, mkdir,
// delete inside the CONNECT-side mount. Proxies through to the local
// dav-client WebDAV listener and translates WebDAV XML into the JSON
// shape the UI wants.
//
// Why this exists: the OS-mounted drive (Finder / Explorer / Files) is
// great for power users who already think in folders, but a sizeable
// chunk of testers preferred a "Drive-like" page in the browser —
// drag-drop upload, click to download, no mount-related Windows
// quirks. So we expose the same data twice: as a real mount and as a
// web file browser. Both back onto the same dav-client local proxy.
package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// FileEntry is one row in the file browser. Field set is what the UI
// renders — anything richer (etag, owner, permissions) is not exposed
// because dav-owner's webdav.Handler doesn't fill those out anyway.
type FileEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"` // ISO 8601 (RFC3339)
	IsDir    bool   `json:"is_dir"`
}

// FilesList is the JSON payload of GET /api/files. Path is canonical
// (always begins with "/", never has trailing slash except for root).
type FilesList struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// connectURL builds the absolute URL on the local dav-client proxy for
// the given path. Returns an error if no Connect session is live.
func (a *appState) connectURL(p string) (string, error) {
	a.mu.Lock()
	sp := a.connect
	a.mu.Unlock()
	if sp == nil {
		return "", errors.New("not connected — start Connect first")
	}
	if sp.localAddr == "" {
		return "", errors.New("local proxy not yet listening")
	}
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// Encode each segment but keep slashes as separators.
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return "http://" + sp.localAddr + strings.Join(parts, "/"), nil
}

// multistatus is the XML shape webdav.Handler returns from PROPFIND.
// We only care about a tiny slice of properties (displayname,
// resourcetype, getcontentlength, getlastmodified) so we accept the
// noise of unknown fields silently.
type multistatus struct {
	XMLName   xml.Name        `xml:"DAV: multistatus"`
	Responses []davResponse   `xml:"response"`
}

type davResponse struct {
	Href     string         `xml:"href"`
	Propstat []davPropstat  `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	DisplayName     string         `xml:"displayname"`
	ResourceType    davResType     `xml:"resourcetype"`
	ContentLength   string         `xml:"getcontentlength"`
	LastModified    string         `xml:"getlastmodified"`
}

type davResType struct {
	Collection *struct{} `xml:"collection"`
}

// listFiles issues a Depth:1 PROPFIND on path and renders the
// children. The response always omits the path itself (which webdav
// always includes as the first <response>) — we only want children.
func (a *appState) listFiles(p string) (*FilesList, error) {
	u, err := a.connectURL(p)
	if err != nil {
		return nil, err
	}
	body := strings.NewReader(`<?xml version="1.0" encoding="utf-8"?>` +
		`<D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`)
	req, err := http.NewRequest("PROPFIND", u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PROPFIND: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PROPFIND returned %s", resp.Status)
	}
	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode multistatus: %w", err)
	}

	// Canonical path for display + comparisons. Trim duplicate slashes
	// and the trailing slash (except for root).
	canon := path.Clean("/" + strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/"))
	if canon == "" {
		canon = "/"
	}
	out := &FilesList{Path: canon, Entries: []FileEntry{}}

	for _, r := range ms.Responses {
		// href is URL-escaped; decode then drop the prefix that
		// matches the request path so we can detect the "self"
		// response (which webdav always returns first).
		href, err := url.PathUnescape(r.Href)
		if err != nil {
			href = r.Href
		}
		hrefClean := path.Clean("/" + strings.TrimSuffix(strings.TrimPrefix(href, "/"), "/"))
		if hrefClean == canon {
			continue
		}
		// Pick the first 200-status propstat block.
		var pr davProp
		for _, ps := range r.Propstat {
			if strings.Contains(ps.Status, "200") {
				pr = ps.Prop
				break
			}
		}
		name := pr.DisplayName
		if name == "" {
			name = path.Base(hrefClean)
		}
		size, _ := strconv.ParseInt(pr.ContentLength, 10, 64)
		modified := ""
		if pr.LastModified != "" {
			// webdav serves RFC1123 timestamps; reformat to RFC3339
			// for the JS Date() constructor's happy path.
			if t, err := time.Parse(time.RFC1123, pr.LastModified); err == nil {
				modified = t.UTC().Format(time.RFC3339)
			} else if t, err := time.Parse(time.RFC1123Z, pr.LastModified); err == nil {
				modified = t.UTC().Format(time.RFC3339)
			} else {
				modified = pr.LastModified
			}
		}
		out.Entries = append(out.Entries, FileEntry{
			Name:     name,
			Size:     size,
			Modified: modified,
			IsDir:    pr.ResourceType.Collection != nil,
		})
	}
	return out, nil
}

// downloadFile streams the file at p out to w. We don't read the body
// into memory — io.Copy handles arbitrary sizes.
func (a *appState) downloadFile(w http.ResponseWriter, p string) error {
	u, err := a.connectURL(p)
	if err != nil {
		return err
	}
	resp, err := http.Get(u)
	if err != nil {
		return fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", p, resp.Status)
	}
	// Mime + filename hints so the browser triggers Save-As.
	filename := path.Base(p)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filename))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(filename)))
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// uploadFile PUTs body at p (replacing if exists). Caller is responsible
// for size limits — we don't buffer in memory, just stream.
func (a *appState) uploadFile(p string, body io.Reader, size int64) error {
	u, err := a.connectURL(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", u, body)
	if err != nil {
		return err
	}
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s: %s", p, resp.Status)
	}
	return nil
}

// mkdir creates a directory at p (MKCOL). Idempotent: existing dir
// returns 405 from webdav; we map that to nil.
func (a *appState) mkdir(p string) error {
	u, err := a.connectURL(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("MKCOL", u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("MKCOL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return nil // already exists — fine
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("MKCOL %s: %s", p, resp.Status)
	}
	return nil
}

// rm deletes the file or directory at p (DELETE). webdav returns 404
// for missing — we surface that so the UI can refresh.
func (a *appState) rm(p string) error {
	u, err := a.connectURL(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("DELETE", u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("DELETE %s: %s", p, resp.Status)
	}
	return nil
}
