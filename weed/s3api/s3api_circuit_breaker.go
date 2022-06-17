package s3api

import (
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/pb/s3_pb"
	"github.com/chrislusf/seaweedfs/weed/s3api/s3_config"
	"github.com/chrislusf/seaweedfs/weed/s3api/s3err"
	"github.com/gorilla/mux"
	"go.uber.org/atomic"
	"net/http"
	"sync"
)

type CircuitBreaker struct {
	sync.Mutex
	Enabled     bool
	counters    map[string]*atomic.Int64
	limitations map[string]int64
}

func NewCircuitBreaker(option *S3ApiServerOption) *CircuitBreaker {
	cb := &CircuitBreaker{
		counters:    make(map[string]*atomic.Int64),
		limitations: make(map[string]int64),
	}

	err := pb.WithFilerClient(false, option.Filer, option.GrpcDialOption, func(client filer_pb.SeaweedFilerClient) error {
		content, err := filer.ReadInsideFiler(client, s3_config.CircuitBreakerConfigDir, s3_config.CircuitBreakerConfigFile)
		if err != nil {
			return fmt.Errorf("read S3 circuit breaker config: %v", err)
		}
		return cb.LoadS3ApiConfigurationFromBytes(content)
	})

	if err != nil {
		glog.Warningf("fail to load config: %v", err)
	}

	return cb
}

func (cb *CircuitBreaker) LoadS3ApiConfigurationFromBytes(content []byte) error {
	cbCfg := &s3_pb.S3CircuitBreakerConfig{}
	if err := filer.ParseS3ConfigurationFromBytes(content, cbCfg); err != nil {
		glog.Warningf("unmarshal error: %v", err)
		return fmt.Errorf("unmarshal error: %v", err)
	}
	if err := cb.loadCircuitBreakerConfig(cbCfg); err != nil {
		return err
	}
	return nil
}

func (cb *CircuitBreaker) loadCircuitBreakerConfig(cfg *s3_pb.S3CircuitBreakerConfig) error {

	//global
	globalEnabled := false
	globalOptions := cfg.Global
	limitations := make(map[string]int64)
	if globalOptions != nil && globalOptions.Enabled && len(globalOptions.Actions) > 0 {
		globalEnabled = globalOptions.Enabled
		for action, limit := range globalOptions.Actions {
			limitations[action] = limit
		}
	}
	cb.Enabled = globalEnabled

	//buckets
	for bucket, cbOptions := range cfg.Buckets {
		if cbOptions.Enabled {
			for action, limit := range cbOptions.Actions {
				limitations[s3_config.Concat(bucket, action)] = limit
			}
		}
	}

	cb.limitations = limitations
	return nil
}

func (cb *CircuitBreaker) Limit(f func(w http.ResponseWriter, r *http.Request), action string) (http.HandlerFunc, Action) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cb.Enabled {
			f(w, r)
			return
		}

		vars := mux.Vars(r)
		bucket := vars["bucket"]

		rollback, errCode := cb.limit(r, bucket, action)
		defer func() {
			for _, rf := range rollback {
				rf()
			}
		}()

		if errCode == s3err.ErrNone {
			f(w, r)
			return
		}
		s3err.WriteErrorResponse(w, r, errCode)
	}, Action(action)
}

func (cb *CircuitBreaker) limit(r *http.Request, bucket string, action string) (rollback []func(), errCode s3err.ErrorCode) {

	//bucket simultaneous request count
	bucketCountRollBack, errCode := cb.loadCounterAndCompare(bucket, action, s3_config.LimitTypeCount, 1, s3err.ErrTooManyRequest)
	if bucketCountRollBack != nil {
		rollback = append(rollback, bucketCountRollBack)
	}
	if errCode != s3err.ErrNone {
		return
	}

	//bucket simultaneous request content bytes
	bucketContentLengthRollBack, errCode := cb.loadCounterAndCompare(bucket, action, s3_config.LimitTypeBytes, r.ContentLength, s3err.ErrRequestBytesExceed)
	if bucketContentLengthRollBack != nil {
		rollback = append(rollback, bucketContentLengthRollBack)
	}
	if errCode != s3err.ErrNone {
		return
	}

	//global simultaneous request count
	globalCountRollBack, errCode := cb.loadCounterAndCompare("", action, s3_config.LimitTypeCount, 1, s3err.ErrTooManyRequest)
	if globalCountRollBack != nil {
		rollback = append(rollback, globalCountRollBack)
	}
	if errCode != s3err.ErrNone {
		return
	}

	//global simultaneous request content bytes
	globalContentLengthRollBack, errCode := cb.loadCounterAndCompare("", action, s3_config.LimitTypeBytes, r.ContentLength, s3err.ErrRequestBytesExceed)
	if globalContentLengthRollBack != nil {
		rollback = append(rollback, globalContentLengthRollBack)
	}
	if errCode != s3err.ErrNone {
		return
	}
	return
}

func (cb *CircuitBreaker) loadCounterAndCompare(bucket, action, limitType string, inc int64, errCode s3err.ErrorCode) (f func(), e s3err.ErrorCode) {
	key := s3_config.Concat(bucket, action, limitType)
	e = s3err.ErrNone
	if max, ok := cb.limitations[key]; ok {
		counter, exists := cb.counters[key]
		if !exists {
			cb.Lock()
			counter, exists = cb.counters[key]
			if !exists {
				counter = atomic.NewInt64(0)
				cb.counters[key] = counter
			}
			cb.Unlock()
		}
		current := counter.Load()
		if current+inc > max {
			e = errCode
			return
		} else {
			counter.Add(inc)
			f = func() {
				counter.Sub(inc)
			}
			current = counter.Load()
			if current > max {
				e = errCode
				return
			}
		}
	}
	return
}
