package httpfault

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultWaitTimeout bounds a wait that names none. It is deliberately longer
// than the agent's own HTTP client timeout so that a scenario waiting for an
// injection sees the agent give up rather than the wait expiring first.
const defaultWaitTimeout = 60 * time.Second

// ControlHandler is the proxy's control API. Keep it on a separate listener
// from Handler so that control calls are never themselves subject to rules.
//
//	POST   /control/rules            arm one rule, or an array of rules
//	DELETE /control/rules/{id}       disarm a rule
//	GET    /control/rules/{id}/wait  block until the rule parks a request
//	POST   /control/rules/{id}/release  let the oldest parked request finish
//	GET    /control/events           the ordered event log
//	POST   /control/reset            disarm everything, empty the log
//	GET    /control/healthz          readiness, before the system is started
func (p *Proxy) ControlHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /control/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"upstream": p.Upstream()})
	})

	mux.HandleFunc("POST /control/rules", func(w http.ResponseWriter, r *http.Request) {
		rules, err := decodeRules(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if len(rules) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("no rules in body"))
			return
		}
		for _, rule := range rules {
			if err := p.AddRule(rule); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]int{"armed": len(rules)})
	})

	mux.HandleFunc("DELETE /control/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.RemoveRule(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})

	// Wait answers 408 when the rule parked nothing in time. A scenario must
	// treat that as a failed injection: the request it meant to freeze went
	// through, or never happened.
	mux.HandleFunc("GET /control/rules/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		timeout := defaultWaitTimeout
		if raw := r.URL.Query().Get("timeout"); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("parse timeout %q: %w", raw, err))
				return
			}
			timeout = parsed
		}
		event, ok := p.Wait(r.PathValue("id"), timeout)
		if !ok {
			writeError(w, http.StatusRequestTimeout,
				fmt.Errorf("rule %q parked no request within %s", r.PathValue("id"), timeout))
			return
		}
		writeJSON(w, http.StatusOK, event)
	})

	// Release answers 409 when nothing is parked, so a scenario cannot mistake
	// a no-op for a released request.
	mux.HandleFunc("POST /control/rules/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		how, err := decodeRelease(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !p.Release(r.PathValue("id"), how) {
			writeError(w, http.StatusConflict,
				fmt.Errorf("rule %q has no parked request to release", r.PathValue("id")))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"released": r.PathValue("id")})
	})

	mux.HandleFunc("GET /control/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Events())
	})

	mux.HandleFunc("POST /control/reset", func(w http.ResponseWriter, r *http.Request) {
		p.Reset()
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// decodeRules accepts a single rule or an array of them, so a shell script can
// arm a whole scenario in one call.
func decodeRules(body io.Reader) ([]Rule, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var many []Rule
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one Rule
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("body is neither a rule nor an array of rules: %w", err)
	}
	return []Rule{one}, nil
}

// decodeRelease treats an empty body as "finish the way the rule says", which
// keeps the common case a bare POST.
func decodeRelease(body io.Reader) (Release, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return Release{}, fmt.Errorf("read body: %w", err)
	}
	if len(raw) == 0 {
		return Release{}, nil
	}
	var how Release
	if err := json.Unmarshal(raw, &how); err != nil {
		return Release{}, fmt.Errorf("parse release: %w", err)
	}
	if how.Respond != "" && how.Respond != RespondForward && how.Respond != RespondStatus {
		return Release{}, fmt.Errorf("unknown respond %q", how.Respond)
	}
	return how, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
