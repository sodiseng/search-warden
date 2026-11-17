package admin

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
)

// Raw files page (ARCHITECTURE.md §14.6): edit config.yaml, egresses.yaml,
// keys/*.yaml and tokens.yaml directly, with the same validation and conflict
// detection as every other edit; export them as a zip.

type fileData struct {
	Status  confstore.FileStatus
	Name    string
	Content string
	Version string
}

func filesTitle(l lang) string { return l.tr("配置文件", "Config files") }

func (p *Panel) filesPage(w http.ResponseWriter, r *http.Request) {
	p.render(w, r, http.StatusOK, "files", page{Title: filesTitle(langOf(r)), Nav: "files", Data: p.conf.Status()})
}

func (p *Panel) fileStatus(name string) confstore.FileStatus {
	for _, s := range p.conf.Status() {
		if s.File == name {
			return s
		}
	}
	return confstore.FileStatus{File: name}
}

func (p *Panel) filePage(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("f")
	if !slices.Contains(confstore.AllFiles(), name) {
		http.NotFound(w, r)
		return
	}
	data, ver, err := p.conf.ReadRaw(name)
	if err != nil {
		l := langOf(r)
		p.render(w, r, http.StatusInternalServerError, "files", page{Title: filesTitle(l), Nav: "files",
			Error: l.tr("读取失败：", "Could not read the file: ") + err.Error(), Data: p.conf.Status()})
		return
	}
	pg := page{Title: name, Nav: "files",
		Data: fileData{Status: p.fileStatus(name), Name: name, Content: string(data), Version: ver}}
	p.dialog(w, r, http.StatusOK, "file", pg)
}

func (p *Panel) fileSave(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("f")
	if !slices.Contains(confstore.AllFiles(), name) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	content := r.PostFormValue("content")
	version := r.PostFormValue("version")
	// Browsers submit textarea line breaks as CRLF; keep the file's own style.
	if cur, _, err := p.conf.ReadRaw(name); err == nil && !bytes.Contains(cur, []byte("\r\n")) {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	if _, err := p.conf.SaveRaw(name, version, []byte(content)); err != nil {
		pg := page{Title: name, Nav: "files", Error: saveError(langOf(r), err),
			Data: fileData{Status: p.fileStatus(name), Name: name, Content: content, Version: version}}
		p.dialog(w, r, http.StatusBadRequest, "file", pg)
		return
	}
	p.done(w, r, "files?ok=saved")
}

// filesExport downloads every config file as a zip. It holds plaintext upstream
// keys and proxy passwords, so it is audited and never cached (§14.6).
func (p *Panel) filesExport(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range confstore.AllFiles() {
		data, _, err := p.conf.ReadRaw(name)
		if err != nil {
			continue
		}
		f, err := zw.Create(name)
		if err == nil {
			_, err = f.Write(data)
		}
		if err != nil {
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
	}
	if err := zw.Close(); err != nil {
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	p.audit(r, "config.export", strings.Join(confstore.AllFiles(), ","))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="warden-config-%s.zip"`, p.now().Format("20060102-150405")))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}
