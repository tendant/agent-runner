package runtime

import (
	"sort"
	"strings"
	"sync"
	"unicode"
)

// EgressHost is one destination a run's egress gateway decided on, with how
// many times it allowed and refused it. Outside counts the allowed
// connections its allowlist does not grant, let through because the
// allowlist was only reported on (AGENT_SANDBOX_EGRESS_ENFORCE=report).
type EgressHost struct {
	Host    string
	Allowed int
	Outside int
	Denied  int
}

// Gateway decision kinds (agentbox egress events).
const (
	egressAllowed = "allowed"
	egressDenied  = "denied"
	egressOutside = "outside" // allowed, not granted: report only
)

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
	// each, if set, receives every decision, tallied or not (the audit record).
	each func(kind, host string)
}

func newEgressLog(emit func(kind, host string)) *egressLog {
	return &egressLog{hosts: map[string]*EgressHost{}, emit: emit}
}

// record tallies one decision: egressAllowed, egressOutside or egressDenied.
func (l *egressLog) record(kind, host string) {
	host = cleanHost(host)
	if l.each != nil {
		l.each(kind, host)
	}
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
	switch kind {
	case egressAllowed:
		first = h.Allowed == 0
		h.Allowed++
	case egressOutside:
		first = h.Outside == 0
		h.Allowed++
		h.Outside++
	default:
		kind = egressDenied
		first = h.Denied == 0
		h.Denied++
	}
	l.mu.Unlock()
	if first && l.emit != nil {
		l.emit("sandbox.egress_"+kind, host)
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
