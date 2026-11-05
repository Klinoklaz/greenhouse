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
	"github.com/klinoklaz/greenhouse/util"
)

func ListenHTTP(exit <-chan util.Void) {
	srv := &http.Server{
		Addr:         config.Server().ListenAddr,
		Handler:      http.HandlerFunc(handleHTTP),
		IdleTimeout:  config.Server().IdleTimeout,
		ReadTimeout:  config.Server().ReadTimeout,
		WriteTimeout: config.Server().WriteTimeout,
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

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	cantCacheMethod := r.Method != "GET" &&
		r.Method != "HEAD" &&
		r.Method != "OPTION"
	cantCacheAuth := config.Rules().AllowAuth &&
		(r.Header.Get("Authorization") != "" ||
			r.Header.Get("Cookie") != "" ||
			r.URL.User != nil)

	var err error
	if cantCacheMethod || cantCacheAuth {
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
			zap.String("client ip", util.GetRealIP(r)))
	}
}

// directHTTP bypasses the caching layer
var directHTTP = httputil.ReverseProxy{
	Director: func(r *http.Request) {
		target := config.Rules().GetTarget(r.URL.Host)
		r.URL.Host = target.Host
		r.URL.Scheme = target.Scheme
	},
}

var cachedHTTP = httputil.ReverseProxy{
	Director: func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		r.URL.User = nil
		// avoid caching empty body
		if r.Method == "HEAD" {
			r.Method = "GET"
		}
		target := config.Rules().GetTarget(r.URL.Host)
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
	var err error
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
			time.Now().Add(config.Server().FetchTimeout),
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
		cw := &cacheWriterHTTP{w: w, e: entry}
		cachedHTTP.ServeHTTP(cw, r)

		if cw.filtered {
			cache.Del(key)
		}
		entry.AfterInit(!cw.filtered)
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
	if config.Rules().CheckMobileUA &&
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
	for i, name := range config.Rules().VaryHeaders {
		value := r.Header.Get(name)
		if value == "" {
			continue
		}
		// NOTE: using index instead of header name makes
		// config.Rules().VaryHeaders unchangeable after process init
		binary.Write(h, order, uint8(i))
		binary.Write(h, order, uint16(len(value)))
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
