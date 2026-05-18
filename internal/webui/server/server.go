// Package server hosts the html/template engine and embedded static-asset
// handler used by the disruptoor webui. The split mirrors spamoor's webui/server
// layout: pagedata + template loading live here, while route registration and
// per-page handlers live one package up.
package server

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// LayoutTemplateFiles is the set of layout-level templates every page loads.
// Keep in the same order as spamoor for parity; page-specific templates are
// appended by the handler.
var LayoutTemplateFiles = []string{
	"_layout/layout.html",
	"_layout/header.html",
	"_layout/footer.html",
}

// Config holds the runtime knobs the server needs. Mirrors spamoor's
// FrontendConfig at a high level but with disruptoor's smaller surface.
type Config struct {
	SiteName  string
	Version   string
	BuildTime string
	Debug     bool // disables template caching; reloads from embed.FS each request
}

// Frontend serves static assets from the embedded FS and exposes a 404 hook.
type Frontend struct {
	logger          *slog.Logger
	defaultHandler  http.Handler
	rootFileSys     http.FileSystem
	NotFoundHandler http.HandlerFunc
}

// PageData is the top-level template context every page receives. The
// page-specific payload lives under Data.
type PageData struct {
	Active    string
	Meta      *Meta
	Data      any
	Version   string
	BuildTime string
	Year      int
	Title     string
	Lang      string
	Debug     bool
}

// Meta carries SEO/canonical bits used by the layout template head.
type Meta struct {
	Title       string
	Description string
	Domain      string
	Path        string
}

// ErrorPageData is the payload for the 500.html template.
type ErrorPageData struct {
	CallTime time.Time
	CallURL  string
	ErrorMsg string
	Version  string
}

// Engine is the shared template loader/renderer. One instance per webui.
type Engine struct {
	cfg          Config
	logger       *slog.Logger
	templateRoot fs.FS
	staticRoot   fs.FS
	mu           sync.RWMutex
	cache        map[string]*template.Template
	funcs        template.FuncMap
}

// NewEngine wires up an Engine against caller-supplied embedded asset trees.
// Both FSes are expected to be rooted at the package's static/ and templates/
// directories respectively (see webui.go for the embed plumbing).
func NewEngine(logger *slog.Logger, cfg Config, staticFS, templatesFS embed.FS) (*Engine, error) {
	if logger == nil {
		return nil, errors.New("logger required")
	}
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("static sub: %w", err)
	}
	tmplSub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		return nil, fmt.Errorf("templates sub: %w", err)
	}
	return &Engine{
		cfg:          cfg,
		logger:       logger.With(slog.String("component", "webui-server")),
		templateRoot: tmplSub,
		staticRoot:   staticSub,
		cache:        make(map[string]*template.Template, 16),
		funcs:        GetTemplateFuncs(),
	}, nil
}

// NewFrontend builds the static-asset handler for an Engine. It is split from
// the engine itself because the handler is mounted at "/" while the engine is
// also used by per-page handlers to render templates.
func NewFrontend(logger *slog.Logger, engine *Engine) *Frontend {
	fsys := http.FS(engine.staticRoot)
	return &Frontend{
		logger:         logger.With(slog.String("component", "webui-static")),
		defaultHandler: http.FileServer(fsys),
		rootFileSys:    fsys,
		NotFoundHandler: func(w http.ResponseWriter, r *http.Request) {
			engine.HandleNotFound(w, r)
		},
	}
}

// ServeHTTP serves an embedded static file, or delegates to NotFoundHandler.
// Mimics http.FileServer's opening dance so directory listings stay disabled.
func (f *Frontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		r.URL.Path = upath
	}
	name := path.Clean(upath)
	file, err := f.rootFileSys.Open(name)
	if err != nil {
		f.handleErr(err, w, r)
		return
	}
	defer file.Close()
	if _, err := file.Stat(); err != nil {
		f.handleErr(err, w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/") {
		// Directories shouldn't be browsable.
		f.NotFoundHandler(w, r)
		return
	}
	f.defaultHandler.ServeHTTP(w, r)
}

// GetTemplate returns a parsed *template.Template for the given file set.
// Results are cached unless Engine.cfg.Debug is true.
func (e *Engine) GetTemplate(files ...string) (*template.Template, error) {
	name := strings.Join(files, "-")
	if !e.cfg.Debug {
		e.mu.RLock()
		if t, ok := e.cache[name]; ok {
			e.mu.RUnlock()
			return t, nil
		}
		e.mu.RUnlock()
	}

	t := template.New(name).Funcs(e.funcs)
	for _, f := range files {
		data, err := fs.ReadFile(e.templateRoot, f)
		if err != nil {
			return nil, fmt.Errorf("read template %q: %w", f, err)
		}
		tplName := path.Base(f)
		var sub *template.Template
		if tplName == t.Name() {
			sub = t
		} else {
			sub = t.New(tplName)
		}
		if _, err := sub.Parse(string(data)); err != nil {
			return nil, fmt.Errorf("parse template %q: %w", f, err)
		}
	}

	if !e.cfg.Debug {
		e.mu.Lock()
		e.cache[name] = t
		e.mu.Unlock()
	}
	return t, nil
}

// InitPageData builds a PageData prefilled with everything the layout needs.
// Page handlers override Data and (optionally) Meta.Title.
func (e *Engine) InitPageData(r *http.Request, active, urlPath, title string) *PageData {
	site := e.cfg.SiteName
	if site == "" {
		site = "disruptoor"
	}
	now := time.Now()
	fullTitle := site + " — " + fmt.Sprint(now.Year())
	if title != "" {
		fullTitle = title + " — " + site + " — " + fmt.Sprint(now.Year())
	}
	host := ""
	if r != nil {
		host = r.Host
	}
	return &PageData{
		Active: active,
		Meta: &Meta{
			Title:       fullTitle,
			Description: "disruptoor: network disruption sidecar control plane",
			Domain:      host,
			Path:        urlPath,
		},
		Data:      &struct{}{},
		Version:   e.cfg.Version,
		BuildTime: e.cfg.BuildTime,
		Year:      now.UTC().Year(),
		Title:     site,
		Lang:      "en-US",
		Debug:     e.cfg.Debug,
	}
}

// Render runs the named layout template against data and writes the result.
// On error it falls back to a 500 page.
func (e *Engine) Render(w http.ResponseWriter, r *http.Request, pageTemplates []string, data *PageData) {
	files := append([]string{}, LayoutTemplateFiles...)
	files = append(files, pageTemplates...)
	t, err := e.GetTemplate(files...)
	if err != nil {
		e.logger.ErrorContext(r.Context(), "load template",
			slog.String("error", err.Error()),
			slog.String("path", r.URL.Path))
		e.handlePageError(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		e.logger.ErrorContext(r.Context(), "execute template",
			slog.String("error", err.Error()),
			slog.String("path", r.URL.Path))
		e.handlePageError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// HandleNotFound renders the 404 template. Exposed so the static-asset handler
// can defer to it for missing files.
func (e *Engine) HandleNotFound(w http.ResponseWriter, r *http.Request) {
	files := append([]string{}, LayoutTemplateFiles...)
	files = append(files, "_layout/404.html")
	t, err := e.GetTemplate(files...)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data := e.InitPageData(r, "", r.URL.Path, "Not Found")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = t.ExecuteTemplate(w, "layout", data)
}

func (e *Engine) handlePageError(w http.ResponseWriter, r *http.Request, pageErr error) {
	files := append([]string{}, LayoutTemplateFiles...)
	files = append(files, "_layout/500.html")
	t, err := e.GetTemplate(files...)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := e.InitPageData(r, "", r.URL.Path, "Internal Error")
	data.Data = &ErrorPageData{
		CallTime: time.Now(),
		CallURL:  r.URL.String(),
		ErrorMsg: pageErr.Error(),
		Version:  e.cfg.Version,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_ = t.ExecuteTemplate(w, "layout", data)
}

func (f *Frontend) handleErr(err error, w http.ResponseWriter, r *http.Request) {
	if errors.Is(err, fs.ErrNotExist) {
		f.NotFoundHandler(w, r)
		return
	}
	if errors.Is(err, fs.ErrPermission) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f.logger.ErrorContext(r.Context(), "static file error",
		slog.String("path", r.URL.Path),
		slog.String("error", err.Error()))
	http.Error(w, "internal error", http.StatusInternalServerError)
}
