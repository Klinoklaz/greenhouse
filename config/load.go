package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

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

func Turnstile() *turnstile {
	return &e.Load().Turnstile
}

func LoadSimple(target string) error {
	tmp := defaultE
	t, err := url.Parse(target)
	if err == nil && t.Host != "" && t.Scheme != "" {
		tmp.Rules.Target.Val = t
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
	if !tmp.Rules.SingleTarget() && !Rules().SingleTarget() {
		if len(tmp.Rules.Targets) == 0 {
			return errors.New("no target url provided")
		}
		for _, t := range tmp.Rules.Targets {
			if !t.Valid() {
				return fmt.Errorf(
					"incomplete target url: '%s'", t.Val)
			}
		}
	} else if !tmp.Rules.Target.Valid() {
		return fmt.Errorf(
			"incomplete target url: '%s'", tmp.Rules.Target.Val)
	}

	// handle optional fields, use default or old value if absent
	if c := util.ParseByteSize(tmp.Cache.CapacityStr); c > 0 {
		tmp.Cache.Capacity = c
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
	if !tmp.Turnstile.SessionTTL.Valid() {
		tmp.Turnstile.SessionTTL = Turnstile().SessionTTL
	}

	// check turnstile custom challenge
	if cp := tmp.Turnstile.ChallengePage; cp != "" {
		f, err := os.OpenFile(cp, os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf(
				"unusable custom challenge page: %w", err)
		}
		f.Close()
	}

	// not having timeouts doesn't make sense
	if !tmp.Server.IdleTimeout.Valid() {
		tmp.Server.IdleTimeout = Server().IdleTimeout
	}
	if !tmp.Server.ReadTimeout.Valid() {
		tmp.Server.ReadTimeout = Server().ReadTimeout
	}
	if !tmp.Server.WriteTimeout.Valid() {
		tmp.Server.WriteTimeout = Server().WriteTimeout
	}
	if !tmp.Server.FetchTimeout.Valid() {
		tmp.Server.FetchTimeout = Server().FetchTimeout
	}

	e.Store(&tmp)
	return nil
}
