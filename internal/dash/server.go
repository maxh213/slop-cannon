package dash

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed static/index.html
var indexHTML string

//go:embed static/style.css
var styleCSS string

//go:embed static/app.js
var appJS string

var dashboardPage = template.Must(template.New("dashboard").Parse(indexHTML))

// NormalizeBasePath returns the canonical route prefix: root, or an absolute
// path with no trailing slash. Dot segments, query strings and fragments are
// rejected rather than silently turning into another route.
func NormalizeBasePath(base string) (string, error) {
	if base == "" {
		base = DefaultBasePath
	}
	if !strings.HasPrefix(base, "/") {
		return "", fmt.Errorf("base path must start with /: %q", base)
	}
	if strings.ContainsAny(base, "?#") {
		return "", fmt.Errorf("base path must not contain a query or fragment: %q", base)
	}
	for _, segment := range strings.Split(base, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("base path must not contain dot segments: %q", base)
		}
	}
	base = path.Clean(base)
	if base == "/" {
		return DefaultBasePath, nil
	}
	return strings.TrimSuffix(base, "/"), nil
}

// NewHandler serves exactly the dashboard page, snapshot and health endpoints
// beneath basePath. Every other path is 404; every non-GET method on a known
// path is 405. No route accepts a run id or a filesystem path.
func NewHandler(observer *Observer, basePath string, allowedHosts ...string) (http.Handler, error) {
	if observer == nil {
		return nil, errors.New("dash: nil observer")
	}
	base, err := NormalizeBasePath(basePath)
	if err != nil {
		return nil, err
	}
	explicitHosts, err := NormalizeAllowedHosts(allowedHosts)
	if err != nil {
		return nil, err
	}
	allowed := allowedHostSet(explicitHosts)
	var page bytes.Buffer
	err = dashboardPage.Execute(&page, struct {
		BasePath string
		CSS      template.CSS
		JS       template.JS
	}{BasePath: base, CSS: template.CSS(styleCSS), JS: template.JS(appJS)})
	if err != nil {
		return nil, fmt.Errorf("dash: render page: %w", err)
	}

	routes := map[string]string{
		joinBase(base, "/api/snapshot"): "snapshot",
		joinBase(base, "/health"):       "health",
	}
	root := base
	if root == "/" {
		routes["/"] = "page"
	} else {
		routes[root] = "page"
		routes[root+"/"] = "page"
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !requestHostAllowed(r.Host, allowed) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		route, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch route {
		case "page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
			_, _ = w.Write(page.Bytes())
		case "snapshot":
			writeJSON(w, observer.Snapshot())
		case "health":
			writeJSON(w, observer.Health())
		}
	}), nil
}

func joinBase(base, route string) string {
	if base == "/" {
		return route
	}
	return base + route
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

// ValidateHostPort checks a dashboard listener without opening it. Port zero
// asks the kernel to choose a free port.
func ValidateHostPort(host string, port int) error {
	if strings.TrimSpace(host) == "" {
		return errors.New("dash: host must not be empty")
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("dash: port %d is outside 0..65535", port)
	}
	return nil
}

// ListenAndServe listens before printing the dashboard URL and serves until
// ctx ends or the HTTP server fails. Port zero is accepted for tests and other
// embedders that want the kernel to choose a free port; the CLI supplies 8790.
func ListenAndServe(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.RunDir) == "" {
		return errors.New("dash: empty run directory")
	}
	if opts.Host == "" {
		opts.Host = DefaultHost
	}
	if err := ValidateHostPort(opts.Host, opts.Port); err != nil {
		return err
	}
	base, err := NormalizeBasePath(opts.BasePath)
	if err != nil {
		return err
	}
	opts.BasePath = base
	listener, err := net.Listen("tcp", net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)))
	if err != nil {
		return fmt.Errorf("dash: listen on %s:%d: %w", opts.Host, opts.Port, err)
	}
	defer func() { _ = listener.Close() }()

	observer := NewObserver(opts)
	handler, err := NewHandler(observer, base, opts.AllowedHosts...)
	if err != nil {
		return err
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if _, err := fmt.Fprintf(opts.Out, "Dashboard for run %s: http://%s%s\n",
		opts.Run.ID, listener.Addr().String(), base); err != nil {
		return fmt.Errorf("dash: print dashboard address: %w", err)
	}

	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		observer.Run(serveCtx)
	}()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	select {
	case err := <-served:
		cancel()
		<-observerDone
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("dash: serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := server.Shutdown(shutdownCtx)
		shutdownCancel()
		cancel()
		<-observerDone
		<-served
		if err != nil {
			return fmt.Errorf("dash: shutdown: %w", err)
		}
		return nil
	}
}
