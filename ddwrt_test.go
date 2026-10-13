package oikoddwrt

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/llehouerou/oiko/bridge"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

const (
	alice = "02:00:00:00:00:0A" // as the router prints them
	bob   = "02:00:00:00:00:0B"
	guest = "02:00:00:00:00:0C"
)

// TestDDWRT follows two phones through the Replay, then Bob arriving and
// Alice leaving, a guest's phone associated all along.
func TestDDWRT(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router := &fakeRouter{wireless: wireless(alice, guest)}
		h := start(t, router, `"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:0a"},
			"bob": {"name": "Bob's phone", "mac": "02:00:00:00:00:0b"}`)

		if !h.Online() || !h.Replayed() {
			t.Fatalf("online %v, replayed %v: want both", h.Online(), h.Replayed())
		}
		var devices []string
		for _, d := range h.Devices() {
			for _, f := range d.Functions {
				for _, c := range f.Capabilities {
					devices = append(devices, fmt.Sprintf("%s %s: %s %s %s %s %+v %s", d.NativeAddress, d.Name, f.Key, f.Kind, c.Key, c.Type, c.Access, c.Category))
				}
			}
		}
		if want := []string{
			"alice Alice's phone: network network connected binary {Observable:true Settable:false Queryable:false} primary",
			"bob Bob's phone: network network connected binary {Observable:true Settable:false Queryable:false} primary",
		}; !slices.Equal(devices, want) {
			t.Errorf("devices = %q,\nwant %q", devices, want)
		}
		assertConnected(t, h, map[string]bool{"alice": true, "bob": false})
		for _, key := range []string{"alice", "bob"} {
			if a := h.Availability(key); a != bridge.Online {
				t.Errorf("%s: %s, want online with the Bridge", key, a)
			}
		}
		if err := h.Command("bob", "network", map[string]any{"connected": true}); !errors.Is(err, bridgetest.ErrRefused) {
			t.Errorf("command: %v, want ErrRefused: nothing is settable", err)
		}

		// Bob arrives: the next poll, 30 s on, reports him, and Alice again.
		replayed, _ := h.Value("alice", "network", "connected")
		router.set(wireless(alice, bob, guest))
		time.Sleep(pollEvery)
		synctest.Wait()
		assertConnected(t, h, map[string]bool{"alice": true, "bob": true})
		if v, _ := h.Value("alice", "network", "connected"); !v.At.After(replayed.At) {
			t.Errorf("alice reported at %v, then at %v: want a refresh every poll", replayed.At, v.At)
		}

		// Alice leaves.
		router.set(wireless(bob, guest))
		time.Sleep(pollEvery)
		synctest.Wait()
		assertConnected(t, h, map[string]bool{"alice": false, "bob": true})
	})
}

// TestOffline follows the router not answering, then refusing the password,
// then answering again: the Bridge goes offline and its phones' Availability
// unknown, each failure logged once, and the next good poll brings it back.
func TestOffline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router := &fakeRouter{wireless: wireless(alice)}
		var logs logBuffer
		h := start(t, router, `"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:0a"}`,
			func(b *Bridge) { b.log = slog.New(slog.NewTextHandler(&logs, nil)) })
		assertOnline := func(when string, want bool) {
			t.Helper()
			availability := map[bool]bridge.Availability{true: bridge.Online, false: bridge.Unknown}[want]
			if h.Online() != want || h.Availability("alice") != availability {
				t.Errorf("%s: online %v, alice %s; want %v, %s", when, h.Online(), h.Availability("alice"), want, availability)
			}
		}
		assertOnline("replayed", true)

		// The router hangs: the poll times out, and the next one too.
		router.setHanging(true)
		time.Sleep(2 * pollEvery)
		synctest.Wait()
		assertOnline("hanging", false)

		// Its password was changed: a 401, logged as a credentials error.
		router.setHanging(false)
		router.setPassword("changed")
		time.Sleep(2 * pollEvery)
		synctest.Wait()
		assertOnline("401", false)

		// The router takes the password again; Alice has left meanwhile.
		router.setPassword("secret")
		router.set(wireless())
		time.Sleep(pollEvery)
		synctest.Wait()
		assertOnline("back", true)
		assertConnected(t, h, map[string]bool{"alice": false})

		lines := logs.lines()
		if len(lines) != 3 ||
			!strings.Contains(lines[0], "Status_Wireless.live.asp: no answer within 10s") ||
			!strings.Contains(lines[1], "Status_Wireless.live.asp: the router refuses the credentials: check username and passwordFile") ||
			!strings.Contains(lines[2], "answers again") {
			t.Errorf("logs:\n%s\nwant a timeout, a credentials error, then back, each once", strings.Join(lines, "\n"))
		}
		for _, l := range lines {
			if strings.Contains(l, "secret") || strings.Contains(l, "changed") {
				t.Errorf("log %q holds a password", l)
			}
		}
	})
}

// A phone whose MAC rotated is re-bound by editing its mac: the key, its
// Native Address, stays.
func TestRotatedMAC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router := &fakeRouter{wireless: wireless("02:00:00:00:00:0D")}
		for mac, want := range map[string]bool{"02:00:00:00:00:0a": false, "02:00:00:00:00:0d": true} {
			h := start(t, router, `"alice": {"name": "Alice's phone", "mac": "`+mac+`"}`)
			assertConnected(t, h, map[string]bool{"alice": want})
		}
	})
}

// TestParse reads made-up status pages in the shapes of several DD-WRT
// builds. testdata's is a real router's, every value made up: each client a
// MAC and fourteen values, the clients of both radios, though assoc_count
// counts those of the one the page shows.
func TestParse(t *testing.T) {
	page, err := os.ReadFile("testdata/Status_Wireless.live.asp")
	if err != nil {
		t.Fatal(err)
	}
	b, err := open(env(t, `"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:0a"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := b.(*Bridge).router
	for _, c := range []struct {
		page string
		want []string
	}{
		{string(page), []string{"02:00:00:00:00:0a", "02:00:00:00:00:0c"}},
		{"{wl_ssid::Example}\n{active_wireless::}\n{assoc_count::0}", nil},
		{"<html>A captive portal</html>", nil},                                     // an error: no active_wireless
		{wireless(alice, bob), []string{"02:00:00:00:00:0a", "02:00:00:00:00:0b"}}, // the current source's, twenty values a client
		// An older Broadcom build's, nine values a client.
		{"{active_wireless::'02:00:00:00:00:0B','eth1','2:00:00','54M','54M','-60','-92','32','640'}", []string{"02:00:00:00:00:0b"}},
	} {
		r.client.Transport = network{"router.example": &fakeRouter{wireless: c.page}}
		got, err := r.associated(t.Context())
		if portal := c.page[0] == '<'; portal != (err != nil) || !slices.Equal(got, c.want) {
			t.Errorf("associated = %q, %v; want %q", got, err, c.want)
		}
	}
}

// TestOpen creates the Bridge as Oiko does, from its section.
func TestOpen(t *testing.T) {
	password := passwordFile(t)
	section := func(fields string) string {
		return `{"url": "http://router.example", "username": "admin", "passwordFile": "` + password + `"` + fields + `}`
	}
	phones := `, "phones": {"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:0a"}}`
	for _, c := range []struct {
		name, config, err string // err: in the error; "" for none
	}{
		{"one phone", section(phones), ""},
		{"https", strings.Replace(section(phones), "http:", "https:", 1), ""},
		{"no phones", section(""), "no phones"},
		{"no url", strings.Replace(section(phones), "http://router.example", "", 1), "url"},
		{"no scheme", strings.Replace(section(phones), "http://", "", 1), "router.example"},
		{"credentials in url", strings.Replace(section(phones), "http://", "http://admin:hunter2@", 1), "url holds credentials"},
		{"no username", strings.Replace(section(phones), `"admin"`, `""`, 1), "username is required"},
		{"no password file", strings.Replace(section(phones), password, "/nonexistent", 1), "/nonexistent"},
		{"no name", section(`, "phones": {"alice": {"mac": "02:00:00:00:00:0a"}}`), "alice: name is required"},
		{"bad mac", section(`, "phones": {"alice": {"name": "Alice's phone", "mac": "02:00:00:00:0a"}}`), "alice: mac"},
		{"same mac", section(`, "phones": {"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:0a"},
			"bob": {"name": "Bob's phone", "mac": "02:00:00:00:00:0A"}}`), "alice and bob have the same mac"},
		{"unknown key", section(phones + `, "password": "hunter2"`), "password"},
		{"unknown phone key", section(`, "phones": {"alice": {"name": "Alice's phone", "mac": "02:00:00:00:00:0a", "ip": "192.168.1.10"}}`), "ip"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := open(bridgetest.Env(t, "ddwrt", c.config))
			if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
				t.Errorf("err = %v, want one about %q", err, c.err)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("err = %v: holds the password", err)
			}
		})
	}
}

// TestManifest checks the catalogue's manifest: the types registered, each
// with a description and an example section made of config's keys only (it
// names a password file that isn't there, so New would refuse it).
func TestManifest(t *testing.T) {
	data, err := os.ReadFile("oiko-bridge.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Types map[string]struct {
			Description string
			Config      json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	types := bridge.Types()
	if len(m.Types) != len(types) {
		t.Errorf("manifest types: %d, registered: %v", len(m.Types), types)
	}
	for _, r := range types {
		typ, ok := m.Types[r.Type]
		if !ok {
			t.Errorf("type %s missing from the manifest", r.Type)
			continue
		}
		var c config
		if err := bridgetest.Env(t, r.Type, string(typ.Config)).Decode(&c); err != nil || typ.Description == "" || len(c.Phones) == 0 {
			t.Errorf("type %s: example section: %v, %d phones; description %q", r.Type, err, len(c.Phones), typ.Description)
		}
	}
}

// start runs a Bridge with phones, reaching router, until the test ends, and
// returns its home once the Bridge waits for its next poll. Each of adjust
// changes the Bridge before it runs.
func start(t *testing.T, router http.Handler, phones string, adjust ...func(*Bridge)) *bridgetest.Home {
	b, err := open(env(t, phones))
	if err != nil {
		t.Fatal(err)
	}
	b.(*Bridge).router.client.Transport = network{"router.example": router}
	for _, f := range adjust {
		f(b.(*Bridge))
	}
	h := bridgetest.New(b)
	go b.Run(t.Context(), h.Port())
	synctest.Wait()
	return h
}

// env is a Bridge's on router.example, following phones, a JSON object's
// members.
func env(t *testing.T, phones string) bridge.Env {
	return bridgetest.Env(t, "ddwrt", `{"url": "http://router.example", "username": "admin",
		"passwordFile": "`+passwordFile(t)+`", "phones": {`+phones+`}}`)
}

func passwordFile(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertConnected(t *testing.T, h *bridgetest.Home, want map[string]bool) {
	t.Helper()
	for key, w := range want {
		if v, ok := h.Value(key, "network", "connected"); !ok || v.Data != w {
			t.Errorf("%s connected = %v (known %v), want %v", key, v.Data, ok, w)
		}
	}
}

// wireless is Status_Wireless.live.asp with macs associated, each with the
// twenty values DD-WRT's current source follows it with.
func wireless(macs ...string) string {
	var clients []string
	for _, mac := range macs {
		clients = append(clients, "'"+mac+"','','wlan0','1:02:03','866.7M','780.0M','VHT80SGI','-48','-95','47','1000','-50','-49','0','0','0','0','0','0','','wlan0'")
	}
	return fmt.Sprintf("{wl_mac::02:00:00:00:00:F0}\n{wl_ssid::Example}\n{active_wireless::%s}\n{active_wds::}\n{assoc_count::%d}\n",
		strings.Join(clients, ","), len(macs))
}

// fakeRouter is a DD-WRT router's web interface, which asks for admin and
// its password, secret unless set.
type fakeRouter struct {
	mu       sync.Mutex
	wireless string // Status_Wireless.live.asp
	password string
	hanging  bool // answering nothing
}

func (f *fakeRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.hanging {
		f.mu.Unlock()
		<-r.Context().Done() // until the client times out
		return
	}
	defer f.mu.Unlock()
	if user, password, _ := r.BasicAuth(); user != "admin" || password != cmp.Or(f.password, "secret") {
		w.Header().Set("WWW-Authenticate", `Basic realm="DD-WRT"`)
		http.Error(w, "Authorization required", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/Status_Wireless.live.asp":
		io.WriteString(w, f.wireless)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRouter) set(wireless string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wireless = wireless
}

func (f *fakeRouter) setPassword(password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.password = password
}

func (f *fakeRouter) setHanging(hanging bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hanging = hanging
}

// logBuffer is a log the Bridge writes while the test reads it.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Split(strings.TrimSpace(l.buf.String()), "\n")
}

// network is an in-memory LAN, an http.RoundTripper: each host's handler
// answers its requests, with no socket, so synctest.Wait sees the Bridge idle
// and the poll's ticker and the client's timeout fast-forward. Any other host
// is unreachable.
type network map[string]http.Handler

func (n network) RoundTrip(r *http.Request) (*http.Response, error) {
	h, ok := n[r.URL.Host]
	if !ok {
		return nil, errors.New("connection refused")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if err := r.Context().Err(); err != nil { // the client gave up: a timeout
		return nil, err
	}
	return w.Result(), nil
}
