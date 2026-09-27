package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/logpace"
	"github.com/tracepad/tracepad/internal/store"
)

// Where a request comes from, and how many password checks each source may
// ask for (spec 046).

// from sets the peer a request arrived from, as net/http would.
func from(peer string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = peer }
}

// forwarded adds one X-Forwarded-For line.
func forwarded(value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Add("X-Forwarded-For", value) }
}

// TestClientAddress is the resolver of #1 and #2, case by case: the peer
// unless it is trusted, then the header read from the right through the
// trusted hops, stopping at the first address that is not trusted — or at the
// first entry that is not an address, which leaves the last trusted one.
func TestClientAddress(t *testing.T) {
	proxies := func(list ...string) trustedProxies {
		var out trustedProxies
		for _, entry := range list {
			out = append(out, netip.MustParsePrefix(entry))
		}
		return out
	}
	loopback := newTrustedProxies(nil)
	twoHops := proxies("127.0.0.0/8", "10.0.0.0/8")
	many := make([]string, 40)
	for i := range many {
		many[i] = fmt.Sprintf("10.0.0.%d", i+1)
	}
	cases := []struct {
		name    string
		trusted trustedProxies
		peer    string
		lines   []string
		want    string
	}{
		{"a direct peer", loopback, "203.0.113.7:5000", nil, "203.0.113.7"},
		{"an untrusted peer's header is ignored", loopback, "203.0.113.7:5000", []string{"198.51.100.1"}, "203.0.113.7"},
		{"a trusted proxy with one hop", loopback, "127.0.0.1:5000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"a trusted proxy with no header is the source", loopback, "127.0.0.1:5000", nil, "127.0.0.1"},
		{"a spoofed left entry behind a trusted proxy", loopback, "127.0.0.1:5000", []string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7"},
		{"two trusted hops", twoHops, "127.0.0.1:5000", []string{"203.0.113.7, 10.1.2.3"}, "203.0.113.7"},
		{"two header lines read as one list", twoHops, "127.0.0.1:5000", []string{"6.6.6.6, 203.0.113.7", "10.1.2.3"}, "203.0.113.7"},
		{"every hop trusted: the leftmost", twoHops, "127.0.0.1:5000", []string{"10.9.9.9, 10.1.2.3"}, "10.9.9.9"},
		{"an entry with a port", loopback, "127.0.0.1:5000", []string{"203.0.113.7:51234"}, "203.0.113.7"},
		{"a bracketed IPv6 entry with a port", loopback, "[::1]:5000", []string{"[2001:db8::1]:443"}, "2001:db8::1"},
		{"a bracketed IPv6 entry without one", loopback, "127.0.0.1:5000", []string{"[2001:db8::1]"}, "2001:db8::1"},
		{"a zone is dropped", loopback, "127.0.0.1:5000", []string{"fe80::1%eth0"}, "fe80::1"},
		{"a mapped peer is IPv4", loopback, "[::ffff:203.0.113.7]:5000", nil, "203.0.113.7"},
		{"a mapped entry is IPv4", loopback, "127.0.0.1:5000", []string{"::ffff:203.0.113.7"}, "203.0.113.7"},
		{"unknown stops the walk at the last trusted hop", twoHops, "127.0.0.1:5000", []string{"203.0.113.7, unknown, 10.1.2.3"}, "10.1.2.3"},
		{"garbage stops the walk", loopback, "127.0.0.1:5000", []string{"203.0.113.7, not-an-address"}, "127.0.0.1"},
		{"an empty entry stops the walk", loopback, "127.0.0.1:5000", []string{"203.0.113.7,"}, "127.0.0.1"},
		{"thirty-two entries and no more", twoHops, "127.0.0.1:5000", []string{"203.0.113.7, " + strings.Join(many, ", ")}, "10.0.0.9"},
		{"none trusts loopback's header no more", proxies(), "127.0.0.1:5000", []string{"203.0.113.7"}, "127.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{trusted: c.trusted, proxyLog: &logpace.Keyed{Every: time.Hour, Keys: 8}}
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.peer
			for _, line := range c.lines {
				r.Header.Add("X-Forwarded-For", line)
			}
			if got := s.clientAddress(r); got.String() != c.want {
				t.Errorf("clientAddress = %s, want %s", got, c.want)
			}
		})
	}
}

// TestSourceIsASlash64: an IPv6 host is given a whole /64 and rotates its
// privacy addresses inside it, so the /64 is the source; an IPv4 address is
// its own (#4).
func TestSourceIsASlash64(t *testing.T) {
	a := sourceOf(netip.MustParseAddr("2001:db8:1:2::1"))
	b := sourceOf(netip.MustParseAddr("2001:db8:1:2:aaaa:bbbb:cccc:dddd"))
	c := sourceOf(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b {
		t.Errorf("two addresses of one /64 are two sources: %s and %s", a, b)
	}
	if a == c {
		t.Errorf("two /64s are one source: %s", a)
	}
	if got := sourceText(a); got != "2001:db8:1:2::/64" {
		t.Errorf("sourceText = %q, want the /64", got)
	}
	if got := sourceText(sourceOf(netip.MustParseAddr("203.0.113.7"))); got != "203.0.113.7" {
		t.Errorf("sourceText = %q, want the bare address", got)
	}
	if sourceOf(netip.MustParseAddr("203.0.113.7")) == sourceOf(netip.MustParseAddr("203.0.113.8")) {
		t.Error("two IPv4 addresses are one source")
	}
}

// TestSourceLimiterIsGCRA: twenty at once, then one every three seconds, and a
// refused request does not push the next one further away (#7).
func TestSourceLimiterIsGCRA(t *testing.T) {
	l := newSourceLimiter()
	source := sourceOf(netip.MustParseAddr("203.0.113.7"))
	now := time.Now()
	for i := range sourceBurst {
		if _, ok := l.take(source, now); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	wait, ok := l.take(source, now)
	if ok || wait != sourceEvery {
		t.Fatalf("the 21st at the same instant: admitted %v, wait %s; want refused, %s", ok, wait, sourceEvery)
	}
	// Hammering does not move the wait.
	for range 100 {
		l.take(source, now)
	}
	if wait, ok := l.take(source, now.Add(time.Second)); ok || wait != 2*time.Second {
		t.Errorf("a second later: admitted %v, wait %s; want refused, 2s", ok, wait)
	}
	// One every three seconds, for as long as it asks.
	at := now
	for i := range 50 {
		at = at.Add(sourceEvery)
		if _, ok := l.take(source, at); !ok {
			t.Fatalf("the sustained rate was refused at step %d", i)
		}
		if _, ok := l.take(source, at); ok {
			t.Fatalf("a second request inside one interval was admitted at step %d", i)
		}
	}
	// And a source that stops is whole again after a minute.
	at = at.Add(time.Duration(sourceBurst) * sourceEvery)
	for i := range sourceBurst {
		if _, ok := l.take(source, at); !ok {
			t.Fatalf("after a rest, request %d of a new burst was refused", i+1)
		}
	}
}

// TestSourceEvictionTakesTheLeastDebt: a source whose bucket is full again
// leaves before any other, and past the capacity the one owing least goes —
// so a spray of fresh sources, each owing one check, cannot push out a source
// that owes twenty (#9, spec 028 #31 c's lesson).
func TestSourceEvictionTakesTheLeastDebt(t *testing.T) {
	l := newSourceLimiter()
	l.capacity = 100
	now := time.Now()
	flooder := sourceOf(netip.MustParseAddr("198.51.100.1"))
	for range sourceBurst {
		l.take(flooder, now)
	}
	for i := range 10 * l.capacity {
		l.take(sourceOf(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)})), now)
	}
	if len(l.entries) > l.capacity || len(l.byTAT) != len(l.entries) {
		t.Fatalf("the limiter holds %d entries (%d in the heap), want at most %d",
			len(l.entries), len(l.byTAT), l.capacity)
	}
	if _, ok := l.take(flooder, now); ok {
		t.Fatal("a spray of fresh sources gave a source over its limit its checks back")
	}

	// Sources whose debt is paid leave without a word, before anyone owing.
	later := now.Add(10 * sourceEvery)
	l.take(sourceOf(netip.MustParseAddr("192.0.2.1")), later)
	if len(l.entries) != 2 {
		t.Errorf("after the spray's debts ran out the limiter holds %d, want the flooder and the newcomer", len(l.entries))
	}

	// A million distinct sources, and memory stays at the ceiling.
	l = newSourceLimiter()
	for i := range 1 << 20 {
		l.take(sourceOf(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})), now)
	}
	if len(l.entries) != sourceTracked {
		t.Errorf("the limiter holds %d entries, want its capacity %d", len(l.entries), sourceTracked)
	}
}

// TestAFloodFromOneSourceLeavesTheGateFree is spec 028 #31's flood, replayed:
// from one address, sign-ins with a fresh email each — every one a decoy
// comparison and a limiter record of its own, so no email's limit trips. The
// source's limit holds it to twenty comparisons, and a person signing in from
// anywhere else meets neither the flood's 429 nor the gate's 503.
func TestAFloodFromOneSourceLeavesTheGateFree(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)
	flood := func(i int) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/auth/login",
			mustJSON(t, map[string]any{"email": fmt.Sprintf("flood-%d@example.com", i), "password": "not the password"}),
			anonymous, from("198.51.100.1:4000"), func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, asJSON)
	}

	work := h.server.passwords.Spent()
	var (
		done  sync.WaitGroup
		mu    sync.Mutex
		codes = map[int]int{}
		last  *httptest.ResponseRecorder
	)
	for i := range 200 {
		done.Add(1)
		go func() {
			defer done.Done()
			rec := flood(i)
			mu.Lock()
			codes[rec.Code]++
			if rec.Code == http.StatusTooManyRequests {
				last = rec
			}
			mu.Unlock()
		}()
	}
	done.Wait()
	if spent := h.server.passwords.Spent() - work; spent > sourceBurst+1 {
		t.Errorf("the flood spent %d comparisons, want at most %d", spent, sourceBurst+1)
	}
	if codes[http.StatusTooManyRequests] < 200-sourceBurst-1 {
		t.Fatalf("codes = %v, want all but twenty refused 429", codes)
	}
	if got := last.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("Retry-After = %q, want the seconds until a check is back", got)
	}
	var refusal struct{ Error string }
	if err := json.Unmarshal(last.Body.Bytes(), &refusal); err != nil ||
		refusal.Error != fmt.Sprintf(sourceRefused, mustAtoi(t, last.Header().Get("Retry-After"))) {
		t.Errorf("the refusal says %q, want the network's sentence with Retry-After's seconds", last.Body)
	}

	// Somebody else, meanwhile, signs in.
	rec := h.call(t, "POST", "/api/v1/auth/login",
		mustJSON(t, map[string]any{"email": "owner@example.com", "password": testAccountPassword}),
		anonymous, from("203.0.113.9:4000"), func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, asJSON)
	expectStatus(t, rec, http.StatusOK)
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return n
}

// TestTheSourceIsAskedLast: each check costs more than the one before it, and
// a refusal gives back what the checks before it reserved (#6, #8, #11). A
// sign-in the source refuses leaves the email's five; an email locked out, a
// link that does not exist and an over-long email cost the source nothing;
// and a password change the source refuses leaves the account's count.
func TestTheSourceIsAskedLast(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	h.invited(t, "new@example.com", false)
	origin := func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }
	login := func(peer, email, password string) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/auth/login",
			mustJSON(t, map[string]any{"email": email, "password": password}),
			anonymous, from(peer), origin, asJSON)
	}
	// A limit of one at a time, and nothing refills during the test.
	h.server.sources = newSourceLimiter()
	h.server.sources.burst, h.server.sources.every = 1, time.Hour

	// The source's one check, then its refusal: the email keeps its count.
	expectStatus(t, login("198.51.100.1:1", "owner@example.com", "wrong once"), http.StatusUnauthorized)
	expectError(t, login("198.51.100.1:1", "owner@example.com", "wrong twice"), http.StatusTooManyRequests,
		"from your network")
	for i := range loginFailureLimit - 1 {
		expectStatus(t, login(fmt.Sprintf("203.0.113.%d:1", i+1), "owner@example.com", "wrong"), http.StatusUnauthorized)
	}
	// Four more failures after the one: five in all, so the refused
	// attempt was not counted — and now the email is locked.
	expectError(t, login("203.0.113.100:1", "owner@example.com", "wrong"), http.StatusTooManyRequests, "for this email")

	// The locked email, an over-long one and a bogus link, all from a
	// fresh source, cost it nothing: its one check is still there after.
	fresh := "192.0.2.50:1"
	expectError(t, login(fresh, "owner@example.com", "wrong"), http.StatusTooManyRequests, "for this email")
	expectStatus(t, login(fresh, strings.Repeat("a", 300)+"@example.com", "wrong"), http.StatusUnauthorized)
	expectStatus(t, h.call(t, "POST", "/api/v1/auth/accept-invite",
		mustJSON(t, map[string]any{"token": "no-such-token", "password": testAccountPassword}),
		anonymous, from(fresh), origin, asJSON), http.StatusForbidden)
	expectStatus(t, login(fresh, "nobody@example.com", "wrong"), http.StatusUnauthorized)
	expectError(t, login(fresh, "nobody@example.com", "wrong"), http.StatusTooManyRequests, "from your network")

	// The invitation, setup's sibling: a real link from a spent source is
	// refused before its hash, and works from another.
	accept := func(peer string) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/auth/accept-invite",
			mustJSON(t, map[string]any{"token": "token-for-new@example.com", "password": testAccountPassword}),
			anonymous, from(peer), origin, asJSON)
	}
	expectError(t, accept(fresh), http.StatusTooManyRequests, "from your network")
	expectStatus(t, accept("192.0.2.51:1"), http.StatusOK)

	// A password change the source refuses leaves the account's count.
	change := func(peer, current string) *httptest.ResponseRecorder {
		return h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
			"password": map[string]any{"current": current, "new": "a brand new password"},
		}), asSession(who), from(peer))
	}
	expectStatus(t, change("192.0.2.60:1", "wrong"), http.StatusForbidden)
	expectError(t, change("192.0.2.60:1", "wrong"), http.StatusTooManyRequests, "from your network")
	for i := range loginFailureLimit - 1 {
		expectStatus(t, change(fmt.Sprintf("192.0.2.%d:1", 70+i), "wrong"), http.StatusForbidden)
	}
	expectError(t, change("192.0.2.80:1", "wrong"), http.StatusTooManyRequests, "for this account")
}

// TestBehindAProxy, end to end over a real listener: the server trusts its
// loopback peer by default, so two clients a proxy forwards are two sources,
// and the session list shows each one's own address rather than the proxy's
// (#1, #5).
func TestBehindAProxy(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)
	h.server.sources = newSourceLimiter()
	h.server.sources.burst, h.server.sources.every = 1, time.Hour
	proxy := httptest.NewServer(h.server.Handler())
	defer proxy.Close()

	login := func(client string) *http.Response {
		req, err := http.NewRequest("POST", proxy.URL+"/api/v1/auth/login",
			bytes.NewReader(mustJSON(t, map[string]any{"email": "owner@example.com", "password": testAccountPassword})))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "6.6.6.6, "+client)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	first := login("203.0.113.1")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("the first client's sign-in: %d", first.StatusCode)
	}
	if code := login("203.0.113.1").StatusCode; code != http.StatusTooManyRequests {
		t.Fatalf("the first client's second sign-in: %d, want 429 with a limit of one", code)
	}
	if code := login("203.0.113.2").StatusCode; code != http.StatusOK {
		t.Fatalf("the second client's sign-in: %d, want its own bucket", code)
	}

	var cookie string
	for _, c := range first.Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Value
		}
	}
	rec := h.call(t, "GET", "/api/v1/auth/sessions", nil, asSession(&signedIn{cookie: cookie}))
	expectStatus(t, rec, http.StatusOK)
	var body struct {
		Sessions []struct {
			IP      string `json:"ip"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, one := range body.Sessions {
		if one.Current && one.IP != "203.0.113.1" {
			t.Errorf("the session is listed from %q, want the client the proxy vouched for", one.IP)
		}
	}
}

// recordLogs sends the server's log to a buffer for the rest of the test.
func recordLogs(t *testing.T) func() string {
	t.Helper()
	var (
		mu     sync.Mutex
		logged bytes.Buffer
	)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, out: &logged}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return logged.String()
	}
}

// TestAnUntrustedProxyIsWarnedAbout: a private peer that forwards is a proxy
// nobody named, and every client behind it is one source — said once an hour,
// with the setting to change. A public peer sending the header is a client,
// and a trusted one is configured: neither is logged (#12).
func TestAnUntrustedProxyIsWarnedAbout(t *testing.T) {
	h := newAccountHarness(t)
	logs := recordLogs(t)

	system := func(mutate ...func(*http.Request)) *httptest.ResponseRecorder {
		return h.call(t, "GET", "/api/v1/system", nil, mutate...)
	}
	for range 3 {
		expectStatus(t, system(from("172.17.0.1:5000"), forwarded("203.0.113.7")), http.StatusOK)
	}
	expectStatus(t, system(from("203.0.113.8:5000"), forwarded("198.51.100.1")), http.StatusOK)
	expectStatus(t, system(from("127.0.0.1:5000"), forwarded("198.51.100.2")), http.StatusOK)
	expectStatus(t, system(from("10.0.0.1:5000")), http.StatusOK)

	text := logs()
	if n := strings.Count(text, "a proxy this server does not trust"); n != 1 {
		t.Fatalf("the warning was logged %d times, want once:\n%s", n, text)
	}
	if !strings.Contains(text, "proxy=172.17.0.1") || !strings.Contains(text, "TRACEPAD_TRUSTED_PROXIES") {
		t.Errorf("the warning names neither the proxy nor the setting:\n%s", text)
	}
}

// TestAProxyBehindAProxyIsWarnedAbout: a load balancer in front of a local
// nginx is the peer's peer. The walk steps past the trusted loopback and stops
// at the balancer — an untrusted private address with the clients still to
// its left — so every client is the balancer, and that is said too. A private
// address with nothing to its left is a client on the network, and says
// nothing (#16).
func TestAProxyBehindAProxyIsWarnedAbout(t *testing.T) {
	h := newAccountHarness(t)
	logs := recordLogs(t)
	system := func(mutate ...func(*http.Request)) *httptest.ResponseRecorder {
		return h.call(t, "GET", "/api/v1/system", nil, mutate...)
	}
	expectStatus(t, system(from("127.0.0.1:5000"), forwarded("192.168.1.20")), http.StatusOK)
	if text := logs(); strings.Contains(text, "does not trust") {
		t.Fatalf("a private client behind the trusted proxy was warned about:\n%s", text)
	}
	expectStatus(t, system(from("127.0.0.1:5000"), forwarded("203.0.113.7, 10.0.0.5")), http.StatusOK)
	if text := logs(); !strings.Contains(text, "proxy=10.0.0.5") {
		t.Fatalf("the balancer in front of the trusted proxy was not warned about:\n%s", text)
	}
}

// TestNoneIsNotWarnedAbout: an operator who set TRACEPAD_TRUSTED_PROXIES=none
// chose to trust nobody, and a warning telling them to add a proxy every hour
// is one they could never silence (#16).
func TestNoneIsNotWarnedAbout(t *testing.T) {
	h := newHarness(t, &config.Config{
		Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		TrustedProxies: []netip.Prefix{},
	}, store.WriterOptions{})
	logs := recordLogs(t)
	expectStatus(t, h.call(t, "GET", "/api/v1/system", nil, from("127.0.0.1:5000"), forwarded("203.0.113.7")), http.StatusOK)
	expectStatus(t, h.call(t, "GET", "/api/v1/system", nil, from("172.17.0.1:5000"), forwarded("203.0.113.7")), http.StatusOK)
	if text := logs(); strings.Contains(text, "does not trust") {
		t.Fatalf("under none, a proxy was warned about:\n%s", text)
	}
}

// TestRetryAfterRoundsUp: a client that waits what Retry-After says is not
// turned away again for waiting too little — 2.4 s is "3", not "2" (#16).
func TestRetryAfterRoundsUp(t *testing.T) {
	for wait, want := range map[time.Duration]string{
		2400 * time.Millisecond: "3",
		3 * time.Second:         "3",
		time.Millisecond:        "1",
		0:                       "1",
	} {
		if got := retryAfterSeconds(wait); got != want {
			t.Errorf("retryAfterSeconds(%s) = %q, want %q", wait, got, want)
		}
	}
}

// TestTheClientIsWorkedOutOnce: a public route checks a password and then
// opens a session, and both ask where the request comes from; the second
// question reads the first answer (#16).
func TestTheClientIsWorkedOutOnce(t *testing.T) {
	s := &Server{trusted: newTrustedProxies(nil), proxyLog: &logpace.Keyed{Every: time.Hour, Keys: 8}}
	r := withClientMemo(httptest.NewRequest("POST", "/", nil))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	first := s.clientAddress(r)
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if second := s.clientAddress(r); second != first {
		t.Errorf("the second question worked the address out again: %s, then %s", first, second)
	}
}

// TestSystemReportsTheSource: /api/v1/system says which source the asking
// request counts as — the one way to check a proxy deployment without
// guessing (#13) — and nothing of the limit's (#16).
func TestSystemReportsTheSource(t *testing.T) {
	h := newHarness(t, &config.Config{
		Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.17.0.0/16")},
	}, store.WriterOptions{})
	rec := h.call(t, "GET", "/api/v1/system", nil, from("172.17.0.1:5000"), forwarded("2001:db8:1:2::7"))
	expectStatus(t, rec, http.StatusOK)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["source"] != "2001:db8:1:2::/64" {
		t.Errorf("source = %v, want the forwarded client's /64", body["source"])
	}
	// Only the asker's own: the limit's counts are every tenant's, and the
	// trusted list is the deployment's topology (#16).
	if _, told := body["source_limit"]; told {
		t.Error("the system read reports the limit's deployment-wide counts to a project key")
	}
}
