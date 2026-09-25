package tracepad

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

const secretKey = "tp-sk-never-leaves-the-store"

// seen is one request as it arrived: the raw target and the bearer.
type seen struct{ method, target, authorization string }

// witness is a store that records every request as it arrived and redirects
// `/hop/…` to hopTo; anything else is answered with a prompt-shaped object.
type witness struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []seen
	hopTo string
	code  int // of the redirect; 302 when unset
}

func serveWitness(t *testing.T) *witness {
	t.Helper()
	w := &witness{}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.seen = append(w.seen, seen{r.Method, r.RequestURI, r.Header.Get("Authorization")})
		hopTo, code := w.hopTo, cmp.Or(w.code, http.StatusFound)
		w.mu.Unlock()
		if rest, ok := strings.CutPrefix(r.URL.Path, "/hop"); ok {
			http.Redirect(rw, r, hopTo+rest, code)
			return
		}
		_, _ = rw.Write([]byte(`{"name":"n","version":1,"type":"text","prompt":"hi","labels":[]}`))
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *witness) requests() []seen {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen)
}

func TestTheKeyIsNotInAnyPrintedForm(t *testing.T) {
	c := config{host: "http://h", key: secretKey, environment: "production"}
	o := options{host: "http://h", key: secretKey}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, printed := range []string{fmt.Sprintf(format, c), fmt.Sprintf(format, &c), fmt.Sprintf(format, o), fmt.Sprintf(format, &o)} {
			if strings.Contains(printed, secretKey) || !strings.Contains(printed, "http://h") {
				t.Errorf("%s printed %q", format, printed)
			}
		}
	}
	if c.key != secretKey {
		t.Fatal("the key itself changed")
	}
}

// Both stores are 127.0.0.1 on two ports: one host name, two origins. net/http's
// own rule compares host names and would have carried the key across.
func TestARedirectToAnotherOriginGoesWithoutTheKey(t *testing.T) {
	store, elsewhere := serveWitness(t), serveWitness(t)
	store.hopTo = elsewhere.URL
	c := config{host: store.URL, key: secretKey}

	for range 2 {
		if _, err := request(context.Background(), c, "GET", "/hop/api/v1/prompts/n", nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, r := range store.requests() {
		if r.authorization != "Bearer "+secretKey {
			t.Errorf("the store itself got %q", r.authorization)
		}
	}
	got := elsewhere.requests()
	if len(got) != 2 {
		t.Fatalf("elsewhere saw %v", got)
	}
	for _, r := range got {
		if r.authorization != "" || r.target != "/api/v1/prompts/n" {
			t.Errorf("elsewhere saw %+v", r)
		}
	}
}

func TestTheKeyDoesNotComeBackWhenTheChainDoes(t *testing.T) {
	store, elsewhere := serveWitness(t), serveWitness(t)
	store.hopTo, elsewhere.hopTo = elsewhere.URL, store.URL

	if _, err := request(context.Background(), config{host: store.URL, key: secretKey}, "GET", "/hop/hop/api/v1/prompts/n", nil, nil); err != nil {
		t.Fatal(err)
	}

	want := []seen{{"GET", "/hop/hop/api/v1/prompts/n", "Bearer " + secretKey}, {"GET", "/api/v1/prompts/n", ""}}
	if got := store.requests(); !slices.Equal(got, want) {
		t.Errorf("the store saw %v", got)
	}
	if got := elsewhere.requests(); !slices.Equal(got, []seen{{"GET", "/hop/api/v1/prompts/n", ""}}) {
		t.Errorf("elsewhere saw %v", got)
	}
}

// net/http turns a POST into a GET on a 302 and re-sends it on a 307; a
// write is re-sent on neither.
func TestAWriteIsNotReSentWhereARedirectPoints(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		store, target := serveWitness(t), serveWitness(t)
		store.hopTo, store.code = target.URL, code

		_, err := request(context.Background(), config{host: store.URL, key: secretKey}, "POST", "/hop/api/v1/scores", []any{}, nil)

		if err == nil || !strings.Contains(err.Error(), "a redirect to "+target.URL+"/api/v1/scores is not followed for POST") {
			t.Errorf("%d: %v", code, err)
		}
		if got := target.requests(); len(got) != 0 {
			t.Errorf("%d: the target saw %v", code, got)
		}
	}
}

func TestARedirectWithinTheOriginKeepsTheKey(t *testing.T) {
	store := serveWitness(t)
	store.hopTo = store.URL

	if _, err := request(context.Background(), config{host: store.URL, key: secretKey}, "GET", "/hop/api/v1/prompts/n", nil, nil); err != nil {
		t.Fatal(err)
	}

	got := store.requests()
	if last := got[len(got)-1]; last != (seen{"GET", "/api/v1/prompts/n", "Bearer " + secretKey}) {
		t.Errorf("the hop arrived as %+v", last)
	}
}

func TestANameIsOneSegmentWhateverItHolds(t *testing.T) {
	for name, segment := range map[string]string{
		"x?confirm=x#": "x%3Fconfirm=x%23",
		"a/b":          "a%2Fb",
		"50%":          "50%25",
		"with space":   "with%20space",
		"ünï":          "%C3%BCn%C3%AF",
	} {
		t.Run(name, func(t *testing.T) {
			store := serveWitness(t)
			setup(t, WithHost(store.URL))
			ctx := context.Background()

			_, err1 := NewDataset(name).Delete(ctx, "")
			_, err2 := Prompt(ctx, name)
			err3 := ScoreConfigs(ctx, []ScoreConfig{{Name: name, DataType: "boolean"}})
			_, err4 := Compare(ctx, name, name)
			for _, err := range []error{err1, err2, err3, err4} {
				if err != nil {
					t.Fatal(err)
				}
			}

			want := []seen{
				{"DELETE", "/api/v1/datasets/" + segment, "Bearer " + testKey},
				{"GET", "/api/v1/prompts/" + segment, "Bearer " + testKey},
				{"PUT", "/api/v1/score-configs/" + segment, "Bearer " + testKey},
				{"GET", "/api/v1/runs/" + segment + "/compare/" + segment, "Bearer " + testKey},
			}
			if got := store.requests(); !slices.Equal(got, want) {
				t.Errorf("got  %v\nwant %v", got, want)
			}
		})
	}
}

func TestANameThatIsNoSegmentIsRefusedBeforeTheWire(t *testing.T) {
	store := serveWitness(t)
	setup(t, WithHost(store.URL))
	ctx := context.Background()

	for _, name := range []string{"", ".", ".."} {
		_, err := NewDataset(name).Delete(ctx, name)
		if err == nil || !strings.Contains(err.Error(), "empty or dot segment") {
			t.Errorf("Delete(%q): %v", name, err)
		}
		_, err = Prompt(ctx, name)
		if err == nil || !strings.Contains(err.Error(), "empty or dot segment") {
			t.Errorf("Prompt(%q): %v", name, err)
		}
	}
	if got := store.requests(); len(got) != 0 {
		t.Errorf("the store saw %v", got)
	}
}
