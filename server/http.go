package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zeebo/xxh3"
	"go.uber.org/zap"

	"github.com/klinoklaz/greenhouse/cache"
	"github.com/klinoklaz/greenhouse/config"
	"github.com/klinoklaz/greenhouse/logger"
	"github.com/klinoklaz/greenhouse/turnstile"
	"github.com/klinoklaz/greenhouse/util"
)

func ListenHTTP(exit <-chan util.Void) {
	srv := &http.Server{
		Addr:         config.Server().ListenAddr,
		Handler:      http.HandlerFunc(handleHTTP),
		IdleTimeout:  config.Server().IdleTimeout.Val,
		ReadTimeout:  config.Server().ReadTimeout.Val,
		WriteTimeout: config.Server().WriteTimeout.Val,
	}

	logger.Get().Sugar().
		Infof("starting http server at %s", srv.Addr)

	go func() {
		<-exit
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logger.Get().Fatal("can not start http server", zap.Error(err))
	}
}

var invalidHost = errors.New("invalid request host")

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	cantCacheMethod := r.Method != "GET" &&
		r.Method != "HEAD" &&
		r.Method != "OPTION"
	var has3rdPartyCookie, cantCacheAuth bool
	if config.Rules().AllowAuth {
		// filter out internal cookies
		for _, c := range r.Cookies() {
			if c.Name != turnstile.SessionName &&
				c.Name != turnstile.TokenName {
				has3rdPartyCookie = true
				break
			}
		}
		cantCacheAuth = has3rdPartyCookie ||
			r.Header.Get("Authorization") != "" ||
			r.URL.User != nil
	}

	t := config.Rules().GetTarget(r.Host)
	var err error
	if t == nil {
		w.WriteHeader(http.StatusBadRequest)
		err = invalidHost
	} else if cantCacheMethod || cantCacheAuth {
		err = handleDirectHTTP(w, r)
	} else {
		err = handleCachedHTTP(w, r)
	}

	if err != nil &&
		!errors.Is(err, syscall.EPIPE) &&
		!errors.Is(err, syscall.ECONNRESET) {
		logger.Get().Error("http request",
			zap.Error(err),
			zap.String("host", r.Host),
			zap.String("uri", r.RequestURI),
			zap.String("client", util.GetRealIP(r)))
	}
}

// directHTTP bypasses the caching layer
var directHTTP = httputil.ReverseProxy{
	Director: func(r *http.Request) {
		target := config.Rules().GetTarget(r.Host)
		r.URL.Host = target.Host
		r.URL.Scheme = target.Scheme
	},
}

// cachedHTTP forwards cache-miss requests to upstream
// and stores the responses into cache pool
var cachedHTTP = httputil.ReverseProxy{
	Director: func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		r.URL.User = nil
		// avoid caching empty body
		if r.Method == "HEAD" {
			r.Method = "GET"
		}
		target := config.Rules().GetTarget(r.Host)
		r.URL.Fragment = ""
		r.URL.Host = target.Host
		r.URL.Scheme = target.Scheme
	},
	ModifyResponse: func(res *http.Response) error {
		res.Header.Del("Set-Cookie")
		res.Header.Del("Expires")
		return nil
	},
}

func handleDirectHTTP(w http.ResponseWriter, r *http.Request) error {
	abort, err := turnstile.ChallengeHTTP(w, r)
	if abort {
		return err
	}
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("direct proxy panicked: %v", e)
		}
	}()
	directHTTP.ServeHTTP(w, r)
	return err
}

func handleCachedHTTP(w http.ResponseWriter, r *http.Request) error {
	key := keygenHTTP(r)
	entry, state := cache.Get(key)
	var err error

	if state != cache.Ok {
		// time.Now() adds noticeable CPU cost on some platform
		ctx, cancel := context.WithDeadlineCause(
			r.Context(),
			time.Now().Add(config.Server().FetchTimeout.Val),
			errors.New("timeout while fetching upstream"))
		defer cancel()
		r = r.WithContext(ctx)
	}

	switch state {
	case cache.Wait:
		if err = entry.Wait(r.Context()); err != nil {
			return fmt.Errorf("incomplete cache entry: %w", err)
		}
	case cache.Miss:
		logger.Get().Debug("http request cache miss",
			zap.String("host", r.Host),
			zap.String("uri", r.RequestURI),
			zap.Any("header", r.Header),
			zap.String("client", util.GetRealIP(r)))

		abort, err := turnstile.ChallengeHTTP(w, r)
		if abort {
			cache.Commit(entry, key, false)
			return err
		}
		defer func() {
			if e := recover(); e != nil {
				cache.Commit(entry, key, false)
				err = fmt.Errorf("cached proxy panicked: %v", e)
			}
		}()
		cw := &cacheWriterHTTP{w: w, e: entry}
		cachedHTTP.ServeHTTP(cw, r)

		cache.Commit(entry, key, !cw.filtered)
		err = cw.err
		if err != nil {
			err = fmt.Errorf("serving fresh response: %w", err)
		}
		return err
	default:
	}

	// serve from cache
	for h := range entry.Header {
		w.Header().Set(h, entry.Header.Get(h))
	}
	w.WriteHeader(entry.StatusCode)
	_, err = w.Write(entry.Body)
	if err != nil {
		err = fmt.Errorf("serving cached response: %w", err)
	}
	return err
}

// keygenHTTP generates cache key from HTTP request info
func keygenHTTP(r *http.Request) cache.Hash {
	h := xxh3.New128()
	if config.Rules().CheckMobile &&
		strings.Contains(r.Header.Get("User-Agent"), "Mobi") {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	// append length to each string to ensure uniqueness
	order := binary.NativeEndian
	if !config.Rules().SingleTarget() {
		binary.Write(h, order, uint16(len(r.Host)))
		h.WriteString(r.Host)
	}
	binary.Write(h, order, uint16(len(r.RequestURI)))
	h.WriteString(r.RequestURI)
	for _, name := range config.Rules().VaryHeaders {
		value := r.Header.Get(name)
		if value == "" {
			continue
		}
		binary.Write(h, order, uint16(len(name)+len(value)+1))
		h.WriteString(name)
		h.WriteString(":")
		h.WriteString(value)
	}
	return h.Sum128()
}

// cacheWriterHTTP decorates http.ResponseWriter
// for copying upstream response into cache
type cacheWriterHTTP struct {
	w        http.ResponseWriter
	e        *cache.Entry
	err      error
	filtered bool // if status code is not in whitelist
}

// (implements http.ResponseWriter)
func (cw *cacheWriterHTTP) Header() http.Header {
	return cw.w.Header()
}

// (implements http.ResponseWriter)
func (cw *cacheWriterHTTP) Write(data []byte) (int, error) {
	cw.e.Body = slices.Concat(cw.e.Body, data)
	// suppress the orginal error to ensure response body
	// can be fully copied into cache during multiple Write() calls
	if cw.err == nil {
		_, cw.err = cw.w.Write(data)
	}
	return len(data), nil
}

// (implements http.ResponseWriter)
func (d *cacheWriterHTTP) WriteHeader(statusCode int) {
	d.w.WriteHeader(statusCode)
	if len(config.Rules().StatusFilter) > 0 &&
		!slices.Contains(config.Rules().StatusFilter, statusCode) {
		d.filtered = true
	}
	d.e.StatusCode = statusCode
	d.e.Header = d.w.Header().Clone()
}
