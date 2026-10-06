//go:build unix

package upgrade

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A release server that takes the connection and never speaks — a TLS
// handshake that does not come — ends the request at its deadline, and the
// refusal says what may hold it and how to go on without it (the live run of
// rc.3, where an application firewall held the connection for minutes).
func TestASilentReleaseServerIsAnAnswerInTime(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, silent, until the listener goes
		}
	}()
	r := &Releases{HTTP: networkClient(time.Second, 200*time.Millisecond, 200*time.Millisecond)}
	start := time.Now()
	_, err = r.read(context.Background(), "https://"+l.Addr().String()+"/download/v0.2.0/checksums.txt")
	if err == nil || !strings.Contains(err.Error(), "did not answer in") || !strings.Contains(err.Error(), "TRACEPAD_DOWNLOAD_URL=file://") {
		t.Errorf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the request took %s", time.Since(start))
	}
}

// A connection that is never made — a firewall holding it until a person
// answers — holds the plan no longer than its look at the releases may take,
// and the plan says why it stopped.
func TestTheReleaseLookupKeepsItsDeadline(t *testing.T) {
	saved := lookupWait
	lookupWait = 300 * time.Millisecond
	t.Cleanup(func() { lookupWait = saved })
	held := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	for _, args := range [][]string{{"--plan", "--to", "0.2.0"}, {"--plan"}} {
		deps := containerDeps(t, newFakeDocker(t))
		deps.Releases = &Releases{Base: githubReleases, API: "https://api.github.com/repos/" + repo, HTTP: held}
		start := time.Now()
		rep, code := runReport(t, deps, args...)
		if code != exitRefused || !strings.Contains(rep.Summary, "did not answer in") || !strings.Contains(rep.Summary, "a firewall") {
			t.Errorf("%q: %d %s", args, code, rep.Summary)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("%q: the plan took %s", args, time.Since(start))
		}
	}
}

// The download client is Go's default transport with the deadlines: HTTP/2
// and the environment's proxy kept (the review of #225).
func TestTheNetworkClientKeepsTheDefaults(t *testing.T) {
	t.Parallel()
	tr := networkClient(time.Second, 2*time.Second, 3*time.Second).Transport.(*http.Transport)
	if !tr.ForceAttemptHTTP2 || tr.Proxy == nil || tr.MaxIdleConns == 0 || tr.TLSHandshakeTimeout != 2*time.Second || tr.ResponseHeaderTimeout != 3*time.Second {
		t.Errorf("%+v", tr)
	}
}
