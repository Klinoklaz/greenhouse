package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/klinoklaz/greenhouse/util"
)

func Cache() *cache {
	return &e.Load().Cache
}

func Server() *server {
	return &e.Load().Server
}

func Log() *log {
	return &e.Load().Log
}

func Rules() *rules {
	return &e.Load().Rules
}

func LoadSimple(target string) error {
	tmp := defaultE
	t, err := url.Parse(target)
	if err == nil && t.Host != "" && t.Scheme != "" {
		tmp.Rules.Target = t
		e.Store(&tmp)
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid target url: %w", err)
	}
	return fmt.Errorf("incomplete target url: %s", target)
}

var savedFilePath string

func LoadFile(file string) error {
	if file == "" {
		file = savedFilePath
	} else {
		savedFilePath = file
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	var tmp everything
	if strings.HasSuffix(file, ".json") {
		err = json.Unmarshal(data, &tmp)
	} else {
		err = yaml.Unmarshal(data, &tmp)
	}
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// can't switch between single- & multi-target on the fly,
	// it will change generated hash key
	if tmp.Rules.TargetStr == "" && !Rules().SingleTarget() {
		if len(tmp.Rules.TargetsStr) == 0 {
			return errors.New("no target url provided")
		}
		tmp.Rules.Targets = make(
			map[string]*url.URL,
			len(tmp.Rules.TargetsStr))
		for k, v := range tmp.Rules.TargetsStr {
			t, err := url.Parse(v)
			if err != nil {
				return fmt.Errorf("invalid target url: %w", err)
			}
			if t.Host == "" || t.Scheme == "" {
				return fmt.Errorf("incomplete target url: %s", v)
			}
			tmp.Rules.Targets[k] = t
		}
	} else {
		t, err := url.Parse(tmp.Rules.TargetStr)
		if err != nil {
			return fmt.Errorf("invalid target url: %w", err)
		}
		if t.Host == "" || t.Scheme == "" {
			return fmt.Errorf(
				"incomplete target url: %s", tmp.Rules.TargetStr)
		}
		tmp.Rules.Target = t
	}
	// modify VaryHeaders slice without changing index
	if old := Rules().VaryHeaders; old != nil {
		current := tmp.Rules.VaryHeaders
		m := make(map[string]bool, len(current))
		for _, v := range current {
			m[v] = true
		}
		for i, v := range old {
			if !m[v] {
				old[i] = ""
			} else {
				delete(m, v)
			}
		}
		for v := range m {
			old = append(old, v)
		}
		tmp.Rules.VaryHeaders = old
	}

	// handle optional fields, use default or old value if absent
	if c := util.ParseByteSize(tmp.Cache.CapacityStr); c > 0 {
		tmp.Cache.Capacity = int64(c)
	} else {
		tmp.Cache.Capacity = Cache().Capacity
	}

	if tmp.Server.ListenAddr == "" {
		tmp.Server.ListenAddr = Server().ListenAddr
	}
	if tmp.Log.Level == "" {
		tmp.Log.Level = Log().Level
	}
	if tmp.Log.Location == "" {
		tmp.Log.Location = Log().Location
	}
	if tmp.Rules.StatusFilter == nil {
		tmp.Rules.StatusFilter = Rules().StatusFilter
	}

	// not having timeouts doesn't make sense
	if i := tmp.Server.IdleTimeoutStr; i > 0 {
		tmp.Server.IdleTimeout = time.Duration(i)
	} else {
		tmp.Server.IdleTimeout = Server().IdleTimeout
	}
	if r := tmp.Server.ReadTimeoutStr; r > 0 {
		tmp.Server.ReadTimeout = time.Duration(r)
	} else {
		tmp.Server.ReadTimeout = Server().ReadTimeout
	}
	if w := tmp.Server.WriteTimeoutStr; w > 0 {
		tmp.Server.WriteTimeout = time.Duration(w)
	} else {
		tmp.Server.WriteTimeout = Server().WriteTimeout
	}
	if f := tmp.Server.FetchTimeoutStr; f > 0 {
		tmp.Server.FetchTimeout = time.Duration(f)
	} else {
		tmp.Server.FetchTimeout = Server().FetchTimeout
	}

	e.Store(&tmp)
	return nil
}
