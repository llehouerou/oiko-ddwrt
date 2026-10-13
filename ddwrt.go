// Package oikoddwrt is the ddwrt type of Bridge of Oiko: it tells which of
// the phones its configuration lists are associated with a DD-WRT access
// point, for Oiko's Presence sources (Oiko's ADR 0050). Each phone is a
// Device with a network Function, whose connected Capability is true while
// the phone is associated.
//
// An Oiko built with this module imports it for its side effect, which
// registers the type:
//
//	import _ "github.com/llehouerou/oiko-ddwrt"
package oikoddwrt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/llehouerou/oiko/bridge"
)

func init() {
	bridge.Register(bridge.Module{Type: "ddwrt", New: open,
		Commands: map[string]func(bridge.Env, []string) error{"clients": clients}})
}

// config is the type's section of "bridges": one access point, and the
// phones to follow on it. The password is read from the file passwordFile
// names, so that it stays out of the configuration.
type config struct {
	URL          string           `json:"url"`
	Username     string           `json:"username"`
	PasswordFile string           `json:"passwordFile"`
	Phones       map[string]phone `json:"phones"` // by key, the Device's Native Address
}

type phone struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

// pollEvery is fixed: departures are smoothed by the Presence source's
// departure delay, not by the poll.
const pollEvery = 30 * time.Second

// open checks the configuration: an error stops Oiko from starting, so it
// names the key or the phone at fault.
func open(env bridge.Env) (bridge.Bridge, error) {
	r, c, err := newRouter(env)
	if err != nil {
		return nil, err
	}
	if len(c.Phones) == 0 {
		return nil, errors.New("ddwrt: no phones")
	}
	phones := map[string]phone{}
	for _, key := range slices.Sorted(maps.Keys(c.Phones)) {
		p := c.Phones[key]
		mac, ok := parseMAC(p.MAC)
		switch {
		case key == "":
			return nil, errors.New("ddwrt: a phone has an empty key")
		case p.Name == "":
			return nil, fmt.Errorf("ddwrt: phone %s: name is required", key)
		case !ok:
			return nil, fmt.Errorf("ddwrt: phone %s: mac %q is no MAC address, such as 02:00:00:00:00:01", key, p.MAC)
		}
		for other, q := range phones {
			if q.MAC == mac {
				return nil, fmt.Errorf("ddwrt: phones %s and %s have the same mac", other, key)
			}
		}
		phones[key] = phone{p.Name, mac}
	}
	return &Bridge{router: r, phones: phones, log: env.Log}, nil
}

// Bridge is one DD-WRT access point, polled every pollEvery for the phones
// associated with it.
type Bridge struct {
	router *router
	phones map[string]phone // by key, their MACs in parseMAC's form
	log    *slog.Logger
	failed string // the last poll's error, "" if it succeeded; only Run's goroutine touches it
}

// Run lists the phones, then polls the router until ctx ends.
func (b *Bridge) Run(ctx context.Context, port bridge.Port) {
	port.SyncDevices(b.devices()) // the configuration's: known before the router answers
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		b.poll(ctx, port)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// poll reads which clients are associated and reports every phone. The first
// poll the router answers is the Replay. While it does not, the Bridge is
// offline, so the phones' Availability is unknown and their Presence sources
// are ignored rather than read as away.
func (b *Bridge) poll(ctx context.Context, port bridge.Port) {
	associated, err := b.router.associated(ctx)
	switch {
	case ctx.Err() != nil: // Oiko is stopping: the router is no less reachable
	case err != nil:
		if err.Error() != b.failed { // every 30 s: logged when it starts or changes
			b.log.Warn("the router does not answer", "err", err)
			b.failed = err.Error()
		}
		port.SetOnline(false)
	default:
		if b.failed != "" {
			b.log.Info("the router answers again")
			b.failed = ""
		}
		port.SetOnline(true)
		for key, p := range b.phones {
			port.SetAvailability(key, bridge.Online)
			port.Report(key, []bridge.Reading{{Function: "network", Capability: "connected",
				Data: slices.Contains(associated, p.MAC)}}, time.Now()) // the page gives no time of its own
		}
		port.Replayed() // later calls change nothing
	}
}

// Send refuses: connected is only observed, so Oiko lets no Command through.
func (b *Bridge) Send(ctx context.Context, address, function string, values map[string]any, _ time.Duration) error {
	return errors.New("ddwrt: read-only")
}

// devices are the configured phones, by key.
func (b *Bridge) devices() []bridge.Device {
	var ds []bridge.Device
	for _, key := range slices.Sorted(maps.Keys(b.phones)) {
		ds = append(ds, bridge.Device{NativeAddress: key, Name: b.phones[key].Name,
			Functions: []bridge.Function{{Key: "network", Kind: "network", Capabilities: []bridge.Capability{{
				Key: "connected", Label: "Connected", Type: bridge.Binary,
				Access: bridge.Access{Observable: true}, Category: bridge.Primary,
			}}}}})
	}
	return ds
}

// clients is `oiko <bridge> clients`: it prints the clients associated with
// the access point now, for the Admin to find a phone's mac. It only reads the
// router, so it is harmless while Oiko serves, and needs no phones yet.
func clients(env bridge.Env, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: oiko %s clients", env.Name)
	}
	r, _, err := newRouter(env)
	if err != nil {
		return err
	}
	return r.printClients(context.Background(), os.Stdout)
}

// printClients writes each client associated, its MAC and the host name of
// its DHCP lease: none when the access point is no DHCP server, or the
// client keeps a static address.
func (r *router) printClients(ctx context.Context, w io.Writer) error {
	associated, err := r.associated(ctx)
	if err != nil {
		return fmt.Errorf("ddwrt: %w", err)
	}
	lan, err := r.status(ctx, "Status_Lan.live.asp", "dhcp_leases")
	if err != nil {
		return fmt.Errorf("ddwrt: %w", err)
	}
	// Each lease is 'host name','IP','MAC',… with more values on newer
	// builds: a host name is two values before its MAC.
	hosts := map[string]string{}
	leases := values(lan["dhcp_leases"])
	for i := 2; i < len(leases); i++ {
		if mac, ok := parseMAC(leases[i]); ok {
			hosts[mac] = leases[i-2]
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MAC\tHOST NAME")
	for _, mac := range associated {
		fmt.Fprintf(tw, "%s\t%s\n", mac, hosts[mac])
	}
	return tw.Flush()
}

// router reads a DD-WRT router's status pages, as its web interface does.
type router struct {
	url                *url.URL
	username, password string
	client             *http.Client
}

// newRouter reads the router's part of the configuration, shared by the
// Bridge and its Commands, and the password.
func newRouter(env bridge.Env) (*router, config, error) {
	var c config
	if err := env.Decode(&c); err != nil { // refuses unknown keys: Oiko won't start
		return nil, c, fmt.Errorf("ddwrt: %w", err)
	}
	u, err := url.Parse(c.URL)
	switch {
	case err == nil && u.User != nil: // never print it: it holds a password
		return nil, c, errors.New("ddwrt: url holds credentials: give them as username and passwordFile")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, c, fmt.Errorf("ddwrt: url %q is no http:// or https:// URL, such as http://192.168.1.1", c.URL)
	case c.Username == "":
		return nil, c, errors.New("ddwrt: username is required")
	}
	password, err := os.ReadFile(c.PasswordFile)
	if err != nil {
		return nil, c, fmt.Errorf("ddwrt: passwordFile: %w", err)
	}
	return &router{url: u, username: c.Username, password: strings.TrimRight(string(password), "\r\n"),
		client: &http.Client{}}, c, nil
}

// associated lists the MACs of the clients associated with the access point
// now: the page's active_wireless, the associations of each of its radios.
func (r *router) associated(ctx context.Context) ([]string, error) {
	status, err := r.status(ctx, "Status_Wireless.live.asp", "active_wireless")
	if err != nil {
		return nil, err
	}
	var macs []string
	for _, v := range values(status["active_wireless"]) {
		if mac, ok := parseMAC(v); ok {
			macs = append(macs, mac)
		}
	}
	return macs, nil
}

// errCredentials is a 401: the router refuses username and the password.
var errCredentials = errors.New("the router refuses the credentials: check username and passwordFile")

// timeout bounds reading a status page, well under a poll.
const timeout = 10 * time.Second

// status reads one of the router's status pages into its fields; one that
// lacks field is no DD-WRT status page.
func (r *router) status(ctx context.Context, page, field string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout) // not the client's: its error's text varies
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url.JoinPath(page).String(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(r.username, r.password)
	resp, err := r.client.Do(req)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s: no answer within %v", page, timeout)
	}
	if err != nil {
		return nil, err // a network error; the URL holds no credentials
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%s: %w", page, errCredentials)
	default:
		return nil, fmt.Errorf("%s: %s", page, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // a status page is a few kB
	if err != nil {
		return nil, fmt.Errorf("%s: %w", page, err)
	}
	fs := fields(string(body))
	if _, ok := fs[field]; !ok {
		return nil, fmt.Errorf("%s: no %s: is it a DD-WRT router?", page, field)
	}
	return fs, nil
}

var statusField = regexp.MustCompile(`\{(\w+)::([^}]*)\}`)

// fields parses a status page, lines of {name::value}.
func fields(page string) map[string]string {
	fs := map[string]string{}
	for _, m := range statusField.FindAllStringSubmatch(page, -1) {
		fs[m[1]] = m[2]
	}
	return fs
}

// values splits a field holding a list, such as 'a','b','c', into its
// values. The router's own pages read their lists this way.
func values(list string) []string {
	return strings.Split(strings.Trim(strings.TrimSpace(list), "'"), "','")
}

// parseMAC reads a 48-bit MAC address in the form the configuration and the
// output use, lower case: the router prints them in upper case.
func parseMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return hw.String(), true
}
