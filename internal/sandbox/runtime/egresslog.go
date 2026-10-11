package runtime

import (
	"sort"
	"strings"
	"sync"
	"unicode"
)

// EgressHost is one destination a run's egress gateway decided on, with how
// many times it allowed and refused it.
type EgressHost struct {
	Host    string
	Allowed int
	Denied  int
}

// maxEgressHosts bounds a run's log: the sandbox picks the names.
const maxEgressHosts = 500

// egressLog tallies a run's gateway decisions by host. The first decision
// of each kind for a host is also reported as an event, so the runner's log
// shows every host a run reached or was refused without one line per
// connection.
type egressLog struct {
	mu      sync.Mutex
	hosts   map[string]*EgressHost
	dropped int // hosts past maxEgressHosts, not tallied
	emit    func(kind, host string)
}

func newEgressLog(emit func(kind, host string)) *egressLog {
	return &egressLog{hosts: map[string]*EgressHost{}, emit: emit}
}

func (l *egressLog) record(allowed bool, host string) {
	host = cleanHost(host)
	l.mu.Lock()
	h, ok := l.hosts[host]
	if !ok {
		if len(l.hosts) >= maxEgressHosts {
			l.dropped++
			l.mu.Unlock()
			return
		}
		h = &EgressHost{Host: host}
		l.hosts[host] = h
	}
	var first bool
	if allowed {
		first = h.Allowed == 0
		h.Allowed++
	} else {
		first = h.Denied == 0
		h.Denied++
	}
	l.mu.Unlock()
	if first && l.emit != nil {
		kind := "sandbox.egress_denied"
		if allowed {
			kind = "sandbox.egress_allowed"
		}
		l.emit(kind, host)
	}
}

// summary is the tally, refused hosts first, then by name.
func (l *egressLog) summary() []EgressHost {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]EgressHost, 0, len(l.hosts))
	for _, h := range l.hosts {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Denied > 0) != (out[j].Denied > 0) {
			return out[i].Denied > 0
		}
		return out[i].Host < out[j].Host
	})
	if l.dropped > 0 {
		out = append(out, EgressHost{Host: "(more hosts, not listed)", Denied: l.dropped})
	}
	return out
}

// cleanHost keeps a sandbox-chosen name safe to log: printable, no
// separators a log or table would read, bounded.
func cleanHost(h string) string {
	h = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) || r == '|' || r == '`' || unicode.IsSpace(r) {
			return '?'
		}
		return r
	}, h)
	if len(h) > 255 {
		h = h[:255] + "…"
	}
	if h == "" {
		h = "?"
	}
	return h
}
