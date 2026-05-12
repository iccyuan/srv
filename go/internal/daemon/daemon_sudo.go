package daemon

import (
	"encoding/base64"
	"time"
)

// Sudo password cache lives in daemon process memory only. The CLI
// asks for it via the sudo_cache_* protocol ops; storage is keyed by
// profile name so different remotes don't share credentials.
//
// Security considerations (and their counter-arguments):
//   - Memory dump risk: a process inspection would expose the
//     password. Counter: the daemon socket is AF_UNIX 0o600, so only
//     the same user could attach a debugger anyway, and that user
//     could just install a malicious sudo wrapper on the remote.
//   - Plain-text base64 on the wire: the AF_UNIX socket is local-only
//     and 0o600; we trade transport encryption for simplicity.
//   - TTL bound: server clamps to a sane max so a caller can't ask for
//     "forever". Default CLI ttl is 5 min; max here is 60 min.

const sudoCacheMaxTTL = 60 * time.Minute

func (s *daemonState) handleSudoCacheGet(req Request) Response {
	if req.Profile == "" {
		return Response{OK: false, Err: "profile is required"}
	}
	s.sudoMu.Lock()
	entry, ok := s.sudoCache[req.Profile]
	if ok && time.Now().After(entry.expires) {
		// Lazy eviction -- expired entries get reaped on the way out.
		delete(s.sudoCache, req.Profile)
		ok = false
	}
	s.sudoMu.Unlock()
	if !ok {
		return Response{OK: true} // cache miss; empty PasswordB64
	}
	return Response{
		OK:          true,
		PasswordB64: base64.StdEncoding.EncodeToString(entry.password),
	}
}

func (s *daemonState) handleSudoCacheSet(req Request) Response {
	if req.Profile == "" {
		return Response{OK: false, Err: "profile is required"}
	}
	if req.PasswordB64 == "" {
		return Response{OK: false, Err: "password_b64 is required"}
	}
	pw, err := base64.StdEncoding.DecodeString(req.PasswordB64)
	if err != nil {
		return Response{OK: false, Err: "password_b64 decode: " + err.Error()}
	}
	ttl := time.Duration(req.TTLSec) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if ttl > sudoCacheMaxTTL {
		ttl = sudoCacheMaxTTL
	}
	s.sudoMu.Lock()
	s.sudoCache[req.Profile] = sudoCacheEntry{
		password: pw,
		expires:  time.Now().Add(ttl),
	}
	s.sudoMu.Unlock()
	return Response{OK: true}
}

func (s *daemonState) handleSudoCacheClear(req Request) Response {
	if req.Profile == "" {
		return Response{OK: false, Err: "profile is required"}
	}
	s.sudoMu.Lock()
	delete(s.sudoCache, req.Profile)
	s.sudoMu.Unlock()
	return Response{OK: true}
}
