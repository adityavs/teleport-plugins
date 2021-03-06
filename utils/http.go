package utils

import (
	"context"
	"crypto/tls"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"
)

type HTTPConfig struct {
	Listen   string `toml:"listen"`
	KeyFile  string `toml:"https-key-file"`
	CertFile string `toml:"https-cert-file"`
	Hostname string `toml:"host"`
	Insecure bool
}

// HTTP is a tiny wrapper around standard net/http.
// It starts either insecure server or secure one with TLS, depending on the settings.
// It also adds a context to its handlers and the server itself has context to.
// So you are guaranteed that server will be closed when the context is cancelled.
type HTTP struct {
	HTTPConfig
	*httprouter.Router
	server http.Server
}

type httpHandlerWrapper struct {
	serve func(http.ResponseWriter, *http.Request)
}

// NewHTTP creates a new HTTP wrapper
func NewHTTP(config HTTPConfig) *HTTP {
	return &HTTP{
		config,
		httprouter.New(),
		http.Server{Addr: config.Listen},
	}
}

func newHttpHandlerWrapper(baseCtx context.Context, handler http.Handler) *httpHandlerWrapper {
	return &httpHandlerWrapper{
		func(rw http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(baseCtx)
			defer cancel()
			go func() {
				select {
				case <-r.Context().Done():
					cancel()
				case <-ctx.Done():
				}
			}()
			handler.ServeHTTP(rw, r.WithContext(ctx))
		},
	}
}

func (h *httpHandlerWrapper) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h.serve(rw, r)
}

// ListenAndServe runs a http(s) server on a provided port.
func (h *HTTP) ListenAndServe(ctx context.Context) error {
	defer log.Info("HTTP server terminated")

	h.server.Handler = newHttpHandlerWrapper(ctx, h.Router)
	go func() {
		<-ctx.Done()
		h.server.Close()
	}()

	var err error
	if h.Insecure {
		log.Infof("Starting insecure HTTP server on %s", h.Listen)
		err = h.server.ListenAndServe()
	} else {
		log.Infof("Starting secure HTTPS server on %s", h.Listen)
		err = h.server.ListenAndServeTLS(h.CertFile, h.KeyFile)
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return trace.Wrap(err)
}

// Shutdown stops the server gracefully.
func (h *HTTP) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

// ShutdownWithTimeout stops the server gracefully.
func (h *HTTP) ShutdownWithTimeout(ctx context.Context, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	return h.Shutdown(ctx)
}

// EnsureCert checks cert and key files consistency. It also generates a self-signed cert if it was not specified.
func (h *HTTP) EnsureCert(defaultPath string) (err error) {
	if h.Insecure {
		return nil
	}
	// If files are specified by user then they should exist and possess right structure
	if h.CertFile != "" {
		_, err = tls.LoadX509KeyPair(h.CertFile, h.KeyFile)
		return err
	}

	log.Warningf("No TLS Keys provided, using self signed certificate.")

	// If files are not specified, try to fall back on self signed certificate.
	h.CertFile = defaultPath + ".crt"
	h.KeyFile = defaultPath + ".key"
	_, err = tls.LoadX509KeyPair(h.CertFile, h.KeyFile)
	if err == nil {
		// self-signed cert was generated previously
		return nil
	}
	if !os.IsNotExist(err) {
		return trace.Wrap(err, "unrecognized error reading certs")
	}

	log.Warningf("Generating self signed key and cert to %v %v.", h.KeyFile, h.CertFile)

	creds, err := utils.GenerateSelfSignedCert([]string{h.Hostname, "localhost"})
	if err != nil {
		return trace.Wrap(err)
	}

	if err := ioutil.WriteFile(h.KeyFile, creds.PrivateKey, 0600); err != nil {
		return trace.Wrap(err, "error writing key PEM")
	}
	if err := ioutil.WriteFile(h.CertFile, creds.Cert, 0600); err != nil {
		return trace.Wrap(err, "error writing cert PEM")
	}
	return nil
}
