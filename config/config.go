package config

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v3"
)

type duration time.Duration

// (implements json.Unmarshaler)
func (d *duration) UnmarshalJSON(data []byte) error {
	if len(data) < 3 {
		return nil
	}
	dd, err := time.ParseDuration(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*d = duration(dd)
	return nil
}

// (implements yaml.Unmarshaler)
func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	dd, err := time.ParseDuration(value.Value)
	if err != nil {
		return err
	} else {
		*d = duration(dd)
		return nil
	}
}

type cache struct {
	Capacity    int64  `json:"-" yaml:"-"` // Max cache size, default 1 GB
	CapacityStr string `json:"capacity" yaml:"capacity"`
}

type server struct {
	ListenAddr string `json:"listen_addr" yaml:"listen_addr"`

	// Corresponds to http.Server.IdleTimeout
	IdleTimeout    time.Duration `json:"-" yaml:"-"`
	IdleTimeoutStr duration      `json:"idle_timeout" yaml:"idle_timeout"`

	// Corresponds to http.Server.ReadTimeout
	ReadTimeout    time.Duration `json:"-" yaml:"-"`
	ReadTimeoutStr duration      `json:"read_timeout" yaml:"read_timeout"`

	// Corresponds to http.Server.WriteTimeout
	WriteTimeout    time.Duration `json:"-" yaml:"-"`
	WriteTimeoutStr duration      `json:"write_timeout" yaml:"write_timeout"`

	// Cancel request if upstream doesn't respond within this amount of time
	FetchTimeout    time.Duration `json:"-" yaml:"-"`
	FetchTimeoutStr duration      `json:"fetch_timeout" yaml:"fetch_timeout"`
}

type log struct {
	// Log level, e.g. debug, info, warn, error, panic, fatal
	Level string `json:"level" yaml:"level"`

	UseJSON  bool   `json:"use_json" yaml:"use_json"` // Format logs as JSON
	Location string `json:"location" yaml:"location"` // Log file location
}

type rules struct {
	// Map of [request host => upstream target]
	Targets    map[string]*url.URL `json:"-" yaml:"-"`
	TargetsStr map[string]string   `json:"targets" yaml:"targets"`

	// Set this if you don't need to deal with multiple hosts
	Target    *url.URL `json:"-" yaml:"-"`
	TargetStr string   `json:"target" yaml:"target"`

	// List of HTTP status codes that are cacheable
	StatusFilter []int `json:"status_filter" yaml:"status_filter"`

	// Response will change along with these request headers.
	// Adding `Origin` to prevent CORS error is recommended
	VaryHeaders []string `json:"vary_headers" yaml:"vary_headers"`

	// Dictates whether to bypass caching if the request is auth related,
	// the server detects common auth info such as cookie / auth headers.
	//
	// When set to false, auth headers are stripped from request
	// to prevent user-specific or privileged content being cached.
	// Note that some auth mechanisms like CSRF token validation may still break
	// despite this option being turned on, due to the nature of reverse proxy
	AllowAuth bool `json:"allow_auth" yaml:"allow_auth"`

	// Whether to detect mobile UA and cache the responses separately
	CheckMobile bool `json:"check_mobile" yaml:"check_mobile"`
}

func (r *rules) GetTarget(host string) *url.URL {
	if r.SingleTarget() {
		return r.Target
	}
	return r.Targets[host]
}

func (r *rules) SingleTarget() bool {
	return r.Target != nil
}

type everything struct {
	Cache  cache  `json:"cache" yaml:"cache"`
	Server server `json:"server" yaml:"server"`
	Log    log    `json:"log" yaml:"log"`
	Rules  rules  `json:"rules" yaml:"rules"`
}

var e atomic.Pointer[everything]

var defaultE = everything{
	Cache: cache{
		Capacity: 1 << 30,
	},
	Server: server{
		ListenAddr:   ":3456",
		IdleTimeout:  30 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		FetchTimeout: 30 * time.Second,
	},
	Log: log{
		Level:    "info",
		Location: "stderr",
	},
	Rules: rules{
		StatusFilter: []int{http.StatusOK},
	},
}

func init() {
	e.Store(&defaultE)
}
