package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pod32g/omni-metrics/internal/push"
)

// Pusher ingests a decoded push request (satisfied by *push.Ingester).
type Pusher interface {
	Ingest(req *push.Request, remoteHost string, nowMs int64) (push.Result, error)
}

// PushSourcesProvider supplies push-source health for /api/v1/push/sources.
type PushSourcesProvider interface {
	Sources() []push.Source
}

// PushConfig configures the push handler at the HTTP layer.
type PushConfig struct {
	Enabled      bool
	MaxBodyBytes int64
	AuthToken    string // empty = no auth
}

func (a *API) handlePush(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("push")
	if !a.authorizePush(r) {
		a.self.IncPushRequest(false)
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}
	limit := a.opts.PushConfig.MaxBodyBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	var req push.Request
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		a.self.IncPushRequest(false)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "bad_data", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_data", "invalid JSON: "+err.Error())
		return
	}

	res, err := a.opts.Push.Ingest(&req, remoteHost(r), time.Now().UnixMilli())
	if err != nil {
		a.self.IncPushRequest(false)
		var ie *push.IngestError
		if errors.As(err, &ie) && ie.Kind == push.ErrInternal {
			if errors.Is(err, push.ErrTooManySeries) {
				a.self.IncPushRejected()
			}
			writeError(w, http.StatusServiceUnavailable, "internal", ie.Msg)
			return
		}
		msg := err.Error()
		if ie != nil {
			msg = ie.Msg
		}
		writeError(w, http.StatusBadRequest, "bad_data", msg)
		return
	}
	a.self.IncPushRequest(true)
	a.self.AddPushSamples(res.SamplesAppended)
	writeData(w, map[string]int{"samplesAppended": res.SamplesAppended, "seriesTouched": res.SeriesTouched})
}

func (a *API) handlePushSources(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("push_sources")
	var srcs []push.Source
	if a.opts.PushSources != nil {
		srcs = a.opts.PushSources.Sources()
	}
	writeData(w, srcs)
}

// authorizePush returns true when no token is configured or the request carries
// the matching bearer token (constant-time compared).
func (a *API) authorizePush(r *http.Request) bool {
	want := a.opts.PushConfig.AuthToken
	if want == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
