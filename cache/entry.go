package cache

import (
	"context"
	"errors"
	"math"
	"math/bits"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"

	"github.com/klinoklaz/greenhouse/util"
)

// counter is the frequency counter
type counter struct {
	flag     atomic.Int32
	lastCalc time.Time
	cnt      atomic.Uint32
	ema      uint32 // exponential moving avg
}

func (e *counter) incr() {
	// ABA prevention:
	//  1. load [0 + 31-bit old version no.]
	//  2. modify to [1 + 31-bit new version no.]
	//  3. business logic
	//  4. revert the prefix bit back to 0
	// if someone else loads the old value simultaneously,
	// the CAS will inevitably fail
	flag := e.flag.Load()
	if time.Since(e.lastCalc) < 1*time.Hour ||
		flag < 0 ||
		!e.flag.CompareAndSwap(flag, (flag+1)|math.MinInt32) {
		e.cnt.Add(1)
	} else {
		e.calcEma(flag == 0)
		e.flag.And(math.MaxInt32)
	}
}

// (not concurrent safe)
func (e *counter) calcEma(init bool) {
	const α = 2. / (24 + 1)
	cnt := e.cnt.Load()
	e.cnt.Add(^(cnt - 1)) // subtract old value
	if init {
		e.ema = cnt
	} else {
		ema := α*float64(cnt) + (1-α)*float64(e.ema)
		e.ema = uint32(math.Ceil(ema))
	}
	e.lastCalc = time.Now()
}

// (not concurrent safe)
func (e *counter) estimate() uint32 {
	elapsed := time.Since(e.lastCalc)
	// avoid manual calculation near an epoch transition
	// where e.cnt and e.ema can be inconsistant
	if elapsed < 10*time.Second {
		return e.ema
	}
	flag := e.flag.Load()
	if elapsed > time.Hour-10*time.Second {
		e.calcEma(flag == 0)
		return e.ema
	}
	const α = 2. / (24 + 1)
	cnt := e.cnt.Load()
	estCnt := float64(cnt) * float64(time.Hour) / float64(elapsed)
	if flag == 0 {
		return uint32(math.Ceil(estCnt))
	}
	estEma := α*estCnt + (1-α)*float64(e.ema)
	return uint32(math.Ceil(estEma))
}

type Hash = xxh3.Uint128

type Entry struct {
	Body       []byte
	Header     http.Header
	StatusCode int

	ready  atomic.Pointer[chan util.Void]
	hit    counter
	digest Hash // Body hash, for deduplication
}

func newEntry() *Entry {
	e := &Entry{}
	ch := make(chan util.Void)
	e.ready.Store(&ch)
	e.hit.lastCalc = time.Now()
	return e
}

// cmp defines ordering of entries in LFU eviction
// (not concurrent safe)
func (e *Entry) cmp(ee *Entry) int {
	est1, est2 := e.hit.estimate(), ee.hit.estimate()
	if est1 != est2 {
		return int(est1 - est2)
	}
	return len(ee.Body) - len(e.Body)
}

func (e *Entry) Wait(ctx context.Context) error {
	ready := e.ready.Load() // get a local copy
	if ready == nil {
		return nil
	}
	select {
	case <-*ready:
		return nil
	case <-ctx.Done():
		return errors.New("request cancelled while waiting for data")
	}
}

func (e *Entry) AfterInit(admitted bool) {
	close(*e.ready.Load())
	// gc the hchan to save a little space
	e.ready.Store(nil)
	if !admitted {
		return
	}
	// if body size is no bigger than pointer size,
	// it's not worth deduplicating by saving a copy of *Entry
	if len(e.Body) > bits.UintSize/8 {
		e.digest = xxh3.Hash128(e.Body)
		deduplicate(e)
	}
}
