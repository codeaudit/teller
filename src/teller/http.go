package teller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/didip/tollbooth"
	"github.com/sirupsen/logrus"
	"github.com/unrolled/secure"
	"golang.org/x/crypto/acme/autocert"

	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/teller/src/exchange"
	"github.com/skycoin/teller/src/scanner"
	"github.com/skycoin/teller/src/util/httputil"
	"github.com/skycoin/teller/src/util/logger"
)

const (
	shutdownTimeout = time.Second * 5

	// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
	// The timeout configuration is necessary for public servers, or else
	// connections will be used up
	serverReadTimeout  = time.Second * 10
	serverWriteTimeout = time.Second * 60
	serverIdleTimeout  = time.Second * 120

	// Directory where cached SSL certs from Let's Encrypt are stored
	tlsAutoCertCache = "cert-cache"
)

// Throttle is used for ratelimiting requests to the http server
type Throttle struct {
	Max      int64
	Duration time.Duration
}

// HTTPConfig configures the HTTP service
type HTTPConfig struct {
	HTTPAddr    string
	HTTPSAddr   string
	StaticDir   string
	StartAt     time.Time
	AutoTLSHost string
	TLSCert     string
	TLSKey      string
	Throttle    Throttle
}

// Validate checks the HTTP config
func (c HTTPConfig) Validate() error {
	if c.HTTPAddr == "" && c.HTTPSAddr == "" {
		return errors.New("at least one of -http-service-addr, -https-service-addr must be set")
	}

	if c.HTTPSAddr != "" && c.AutoTLSHost == "" && (c.TLSCert == "" || c.TLSKey == "") {
		return errors.New("when using -tls, either -auto-tls-host or both -tls-cert and -tls-key must be set")
	}

	if (c.TLSCert == "" && c.TLSKey != "") || (c.TLSCert != "" && c.TLSKey == "") {
		return errors.New("-tls-cert and -tls-key must be set or unset together")
	}

	if c.AutoTLSHost != "" && (c.TLSKey != "" || c.TLSCert != "") {
		return errors.New("either use -auto-tls-host or both -tls-key and -tls-cert")
	}

	if c.HTTPSAddr == "" && (c.AutoTLSHost != "" || c.TLSKey != "" || c.TLSCert != "") {
		return errors.New("-auto-tls-host or -tls-key or -tls-cert is set but -tls is not enabled")
	}

	return nil
}

type httpServer struct {
	Config HTTPConfig

	log           logrus.FieldLogger
	service       *service
	httpListener  *http.Server
	httpsListener *http.Server
	quit          chan struct{}
}

func newHTTPServer(log logrus.FieldLogger, cfg HTTPConfig, service *service) *httpServer {
	return &httpServer{
		Config: cfg,
		log: log.WithFields(logrus.Fields{
			"prefix": "teller.http",
			"config": cfg,
		}),
		service: service,
	}
}

func (hs *httpServer) Run() error {
	log := hs.log

	log.Info("HTTP service start")
	defer log.Info("HTTP service closed")

	hs.quit = make(chan struct{})

	var mux http.Handler = hs.setupMux()

	allowedHosts := []string{} // empty array means all hosts allowed
	sslHost := ""
	if hs.Config.AutoTLSHost == "" {
		// Note: if AutoTLSHost is not set, but HTTPSAddr is set, then
		// http will redirect to the HTTPSAddr listening IP, which would be
		// either 127.0.0.1 or 0.0.0.0
		// When running behind a DNS name, make sure to set AutoTLSHost
		sslHost = hs.Config.HTTPSAddr
	} else {
		sslHost = hs.Config.AutoTLSHost
		// When using -auto-tls-host,
		// which implies automatic Let's Encrypt SSL cert generation in production,
		// restrict allowed hosts to that host.
		allowedHosts = []string{hs.Config.AutoTLSHost}
	}

	if len(allowedHosts) == 0 {
		log = log.WithField("allowedHosts", "all")
	} else {
		log = log.WithField("allowedHosts", allowedHosts)
	}

	log = log.WithField("sslHost", sslHost)

	log.Info("Configured")

	secureMiddleware := configureSecureMiddleware(sslHost, allowedHosts)
	mux = secureMiddleware.Handler(mux)

	if hs.Config.HTTPAddr != "" {
		hs.httpListener = setupHTTPListener(hs.Config.HTTPAddr, mux)
	}

	handleListenErr := func(f func() error) error {
		if err := f(); err != nil {
			select {
			case <-hs.quit:
				return nil
			default:
				log.WithError(err).Error("ListenAndServe or ListenAndServeTLS error")
				return fmt.Errorf("http serve failed: %v", err)
			}
		}
		return nil
	}

	if hs.Config.HTTPSAddr != "" {
		log.Info("Using TLS")

		hs.httpsListener = setupHTTPListener(hs.Config.HTTPSAddr, mux)

		tlsCert := hs.Config.TLSCert
		tlsKey := hs.Config.TLSKey

		if hs.Config.AutoTLSHost != "" {
			log.Info("Using Let's Encrypt autocert")
			// https://godoc.org/golang.org/x/crypto/acme/autocert
			// https://stackoverflow.com/a/40494806
			certManager := autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(hs.Config.AutoTLSHost),
				Cache:      autocert.DirCache(tlsAutoCertCache),
			}

			hs.httpsListener.TLSConfig = &tls.Config{
				GetCertificate: certManager.GetCertificate,
			}

			// These will be autogenerated by the autocert middleware
			tlsCert = ""
			tlsKey = ""
		}

		errC := make(chan error)

		if hs.Config.HTTPAddr == "" {
			return handleListenErr(func() error {
				return hs.httpsListener.ListenAndServeTLS(tlsCert, tlsKey)
			})
		}
		return handleListenErr(func() error {
			var wg sync.WaitGroup
			wg.Add(2)

			go func() {
				defer wg.Done()
				if err := hs.httpsListener.ListenAndServeTLS(tlsCert, tlsKey); err != nil {
					log.WithError(err).Error("ListenAndServeTLS error")
					errC <- err
				}
			}()

			go func() {
				defer wg.Done()
				if err := hs.httpListener.ListenAndServe(); err != nil {
					log.WithError(err).Println("ListenAndServe error")
					errC <- err
				}
			}()

			done := make(chan struct{})

			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case err := <-errC:
				return err
			case <-hs.quit:
				return nil
			case <-done:
				return nil
			}
		})
	}

	return handleListenErr(func() error {
		return hs.httpListener.ListenAndServe()
	})

}

func configureSecureMiddleware(sslHost string, allowedHosts []string) *secure.Secure {
	sslRedirect := true
	if sslHost == "" {
		sslRedirect = false
	}

	return secure.New(secure.Options{
		AllowedHosts: allowedHosts,
		SSLRedirect:  sslRedirect,
		SSLHost:      sslHost,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/CSP
		// FIXME: Web frontend code has inline styles, CSP doesn't work yet
		// ContentSecurityPolicy: "default-src 'self'",

		// Set HSTS to one year, for this domain only, do not add to chrome preload list
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Strict-Transport-Security
		STSSeconds:           31536000, // 1 year
		STSIncludeSubdomains: false,
		STSPreload:           false,

		// Deny use in iframes
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options
		FrameDeny: true,

		// Disable MIME sniffing in browsers
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Content-Type-Options
		ContentTypeNosniff: true,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-XSS-Protection
		BrowserXssFilter: true,

		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Referrer-Policy
		// "same-origin" is invalid in chrome
		ReferrerPolicy: "no-referrer",
	})
}

func setupHTTPListener(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
	}
}

func (hs *httpServer) setupMux() *http.ServeMux {
	mux := http.NewServeMux()

	handleAPI := func(path string, f http.HandlerFunc) {
		rateLimited := rateLimiter(hs.Config.Throttle, httputil.LogHandler(hs.log, f))
		mux.Handle(path, gziphandler.GzipHandler(rateLimited))
	}

	// API Methods
	handleAPI("/api/bind", httputil.LogHandler(hs.log, BindHandler(hs)))
	handleAPI("/api/status", httputil.LogHandler(hs.log, StatusHandler(hs)))

	// Static files
	mux.Handle("/", gziphandler.GzipHandler(http.FileServer(http.Dir(hs.Config.StaticDir))))

	return mux
}

func rateLimiter(thr Throttle, hd http.HandlerFunc) http.Handler {
	return tollbooth.LimitFuncHandler(tollbooth.NewLimiter(thr.Max, thr.Duration), hd)
}

func (hs *httpServer) Shutdown() {
	if hs.quit != nil {
		close(hs.quit)
	}

	shutdown := func(proto string, ln *http.Server) {
		if ln == nil {
			return
		}
		log := hs.log.WithFields(logrus.Fields{
			"proto":   proto,
			"timeout": shutdownTimeout,
		})

		log.Info("Shutting down server")

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := ln.Shutdown(ctx); err != nil {
			log.WithError(err).Error("HTTP server shutdown error")
		}
	}

	shutdown("HTTP", hs.httpListener)
	shutdown("HTTPS", hs.httpsListener)

	hs.quit = nil
}

// BindResponse http response for /api/bind
type BindResponse struct {
	DepositAddress string `json:"deposit_address,omitempty"`
	CoinType       string `json:"coin_type,omitempty"`
}

type bindRequest struct {
	SkyAddr  string `json:"skyaddr"`
	CoinType string `json:"coin_type"`
}

// BindHandler binds skycoin address with a bitcoin address
// Method: POST
// Accept: application/json
// URI: /api/bind
// Args:
//    {"skyaddr": "...", "coin_type": "BTC"}
func BindHandler(hs *httpServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logger.FromContext(ctx)

		w.Header().Set("Accept", "application/json")

		if !validMethod(ctx, w, r, []string{http.MethodPost}) {
			return
		}

		if r.Header.Get("Content-Type") != "application/json" {
			errorResponse(ctx, w, http.StatusUnsupportedMediaType, errors.New("Invalid content type"))
			return
		}

		bindReq := &bindRequest{}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&bindReq); err != nil {
			err = fmt.Errorf("Invalid json request body: %v", err)
			errorResponse(ctx, w, http.StatusBadRequest, err)
			return
		}
		defer r.Body.Close()

		log = log.WithField("bindReq", bindReq)
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		if bindReq.SkyAddr == "" {
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Missing skyaddr"))
			return
		}

		switch bindReq.CoinType {
		case scanner.CoinTypeBTC:
		case "":
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Missing coin_type"))
		default:
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Invalid coin_type"))
		}

		log.Info()

		if !verifySkycoinAddress(ctx, w, bindReq.SkyAddr) {
			return
		}

		if !readyToStart(ctx, w, hs.Config.StartAt) {
			return
		}

		log.Info("Calling service.BindAddress")

		btcAddr, err := hs.service.BindAddress(bindReq.SkyAddr)
		if err != nil {
			// TODO -- these could be internal server error, gateway error
			log.WithError(err).Error("service.BindAddress failed")
			httputil.ErrResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		log = log.WithField("btcAddr", btcAddr)
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		log.Info("Bound sky and btc addresses")

		if err := httputil.JSONResponse(w, BindResponse{
			DepositAddress: btcAddr,
			CoinType:       scanner.CoinTypeBTC,
		}); err != nil {
			log.WithError(err).Error()
		}
	}
}

// StatusResponse http response for /api/status
type StatusResponse struct {
	Statuses []exchange.DepositStatus `json:"statuses,omitempty"`
}

// StatusHandler returns the deposit status of specific skycoin address
// Method: GET
// URI: /api/status
// Args:
//     skyaddr
func StatusHandler(hs *httpServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := logger.FromContext(ctx)

		if !validMethod(ctx, w, r, []string{http.MethodGet}) {
			return
		}

		skyAddr := r.URL.Query().Get("skyaddr")
		if skyAddr == "" {
			errorResponse(ctx, w, http.StatusBadRequest, errors.New("Missing skyaddr"))
			return
		}

		log = log.WithField("skyAddr", skyAddr)
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		log.Info()

		if !verifySkycoinAddress(ctx, w, skyAddr) {
			return
		}

		if !readyToStart(ctx, w, hs.Config.StartAt) {
			return
		}

		log.Info("Sending StatusRequest to teller")

		depositStatuses, err := hs.service.GetDepositStatuses(skyAddr)
		if err != nil {
			// TODO -- these could be internal server error, gateway error
			log.WithError(err).Error("service.GetDepositStatuses failed")
			httputil.ErrResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		log = log.WithFields(logrus.Fields{
			"depositStatuses":    depositStatuses,
			"depositStatusesLen": len(depositStatuses),
		})
		ctx = logger.WithContext(ctx, log)
		r = r.WithContext(ctx)

		log.Info("Got depositStatuses")

		if err := httputil.JSONResponse(w, StatusResponse{
			Statuses: depositStatuses,
		}); err != nil {
			log.WithError(err).Error()
		}
	}
}

func readyToStart(ctx context.Context, w http.ResponseWriter, startAt time.Time) bool {
	if time.Now().UTC().After(startAt.UTC()) {
		return true
	}

	msg := fmt.Sprintf("Event starts at %v", startAt)
	httputil.ErrResponse(w, http.StatusForbidden, msg)

	log := logger.FromContext(ctx)
	log.WithField("status", http.StatusForbidden).Info(msg)

	return false
}

func validMethod(ctx context.Context, w http.ResponseWriter, r *http.Request, allowed []string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}

	w.Header().Set("Allow", strings.Join(allowed, ", "))

	status := http.StatusMethodNotAllowed
	errorResponse(ctx, w, status, errors.New("Invalid request method"))

	return false
}

func verifySkycoinAddress(ctx context.Context, w http.ResponseWriter, skyAddr string) bool {
	log := logger.FromContext(ctx)

	if _, err := cipher.DecodeBase58Address(skyAddr); err != nil {
		msg := fmt.Sprintf("Invalid skycoin address: %v", err)
		httputil.ErrResponse(w, http.StatusBadRequest, msg)
		log.WithFields(logrus.Fields{
			"status":  http.StatusBadRequest,
			"skyAddr": skyAddr,
		}).WithError(err).Info("Invalid skycoin address")
		return false
	}

	return true
}

func handleServiceResponseError(ctx context.Context, w http.ResponseWriter, err error) {
	if err != nil {
		errorResponse(ctx, w, http.StatusInternalServerError, err)
	}
}

func errorResponse(ctx context.Context, w http.ResponseWriter, code int, err error) {
	log := logger.FromContext(ctx)
	log.WithFields(logrus.Fields{
		"status":    code,
		"statusMsg": http.StatusText(code),
	}).WithError(err).Info()

	httputil.ErrResponse(w, code)
}