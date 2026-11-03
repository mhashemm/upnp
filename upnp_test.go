package upnp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	respHead = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`
	respTail = `</s:Body></s:Envelope>`
)

func extIPResp(ip string) string {
	return respHead + `<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>` +
		ip + `</NewExternalIPAddress></u:GetExternalIPAddressResponse>` + respTail
}

func faultResp(code string) string {
	return respHead + `<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>` +
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>` + code +
		`</errorCode><errorDescription>test</errorDescription></UPnPError></detail></s:Fault>` + respTail
}

func scpd(actions ...string) string {
	s := `<?xml version="1.0"?><scpd><actionList>`
	for _, a := range actions {
		s += `<action><name>` + a + `</name></action>`
	}
	return s + `</actionList></scpd>`
}

func allActions() []string {
	return []string{addPortMapping, deletePortMapping, getExternalIPAddress}
}

// fakeSvc describes one WAN connection service a fake router should publish.
type fakeSvc struct {
	typ     string
	actions []string
	extIP   string
}

// fakeRouter serves a device description at /desc.xml plus an SCPD and a SOAP
// control endpoint per service, which is everything discovery touches after
// SSDP. Services appear in the description in the order given.
func fakeRouter(t *testing.T, svcs ...fakeSvc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	desc := `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><device>` +
		`<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>` +
		`<manufacturer>Acme</manufacturer><modelName>R1</modelName><serviceList>`
	for i, s := range svcs {
		id := fmt.Sprint(i)
		desc += `<service><serviceType>` + s.typ + `</serviceType>` +
			`<SCPDURL>/scpd/` + id + `</SCPDURL><controlURL>/ctl/` + id + `</controlURL></service>`
		mux.HandleFunc("/scpd/"+id, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, scpd(s.actions...))
		})
		mux.HandleFunc("/ctl/"+id, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			switch {
			case strings.Contains(string(body), getExternalIPAddress):
				io.WriteString(w, extIPResp(s.extIP))
			case strings.Contains(string(body), addPortMapping):
				io.WriteString(w, respHead+`<u:AddPortMappingResponse xmlns:u="urn:x"></u:AddPortMappingResponse>`+respTail)
			default:
				io.WriteString(w, respHead+`<u:DeletePortMappingResponse xmlns:u="urn:x"></u:DeletePortMappingResponse>`+respTail)
			}
		})
	}
	desc += `</serviceList></device></root>`
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, desc)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func location(url string) http.Header {
	return http.Header{"Location": []string{url + "/desc.xml"}}
}

// The URL forms below all appear in real device descriptions. Concatenating
// them onto the host root, as the old joinPath did, breaks the last three.
func TestResolveURL(t *testing.T) {
	cases := []struct{ base, ref, want string }{
		{"http://10.0.0.1:5000", "/ctl/IPConn", "http://10.0.0.1:5000/ctl/IPConn"},
		{"http://10.0.0.1:5000/", "/ctl/IPConn", "http://10.0.0.1:5000/ctl/IPConn"},
		{"http://10.0.0.1:5000/rootDesc.xml", "/ctl/IPConn", "http://10.0.0.1:5000/ctl/IPConn"},
		{"http://10.0.0.1:5000/rootDesc.xml", "http://10.0.0.1:5000/ctl/IPConn", "http://10.0.0.1:5000/ctl/IPConn"},
		{"http://10.0.0.1:5000/upnp/desc.xml", "x_scpd.xml", "http://10.0.0.1:5000/upnp/x_scpd.xml"},
		{"http://10.0.0.1:5000/upnp/desc.xml", "./ctl/IPConn", "http://10.0.0.1:5000/upnp/ctl/IPConn"},
	}
	for _, c := range cases {
		got, err := resolveURL(c.base, c.ref)
		if err != nil {
			t.Fatalf("resolveURL(%q, %q): %v", c.base, c.ref, err)
		}
		if got != c.want {
			t.Errorf("resolveURL(%q, %q) = %q, want %q", c.base, c.ref, got, c.want)
		}
	}
	if _, err := resolveURL("http://10.0.0.1", "http://[::1]:namedport"); err == nil {
		t.Error("want an error for an unparseable reference")
	}
}

// net/http cannot serialize a non-HTTP method to "*", so each datagram is a
// literal with a fixed header order; built from a map, the order changed from
// run to run. ssdp:all alone misses gateways: some routers answer it with only
// their first descriptor, typically the WPS WFADevice (tailscale/tailscale
// #3557). miniupnpc never uses it to find a gateway and searches
// InternetGatewayDevice:1 instead; libtorrent searches only upnp:rootdevice.
func TestMSearch(t *testing.T) {
	for _, st := range []string{"ssdp:all", "upnp:rootdevice", "urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
		if !slices.Contains(ssdpTargets, st) {
			t.Errorf("missing search target %q", st)
		}
	}
	for _, st := range ssdpTargets {
		m := string(mSearch(st))
		if !strings.HasPrefix(m, "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n") {
			t.Errorf("request line then HOST must lead: %q", m)
		}
		if !strings.HasSuffix(m, "\r\n\r\n") {
			t.Errorf("headers must end with a blank line: %q", m)
		}
		for _, want := range []string{"\r\nMAN: \"ssdp:discover\"\r\n", "\r\nMX: 5\r\n", "\r\nST: " + st + "\r\n", "\r\nUSER-AGENT: "} {
			if !strings.Contains(m, want) {
				t.Errorf("missing %q in %q", want, m)
			}
		}
	}
}

func ssdpOK(location string) []byte {
	return ssdpST(location, "upnp:rootdevice")
}

func ssdpST(location, st string) []byte {
	return []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nLOCATION: " + location +
		"\r\nSERVER: Linux UPnP/1.1 fake/1.0\r\nST: " + st + "\r\nUSN: uuid:1::" + st + "\r\n\r\n")
}

func datagrams(from string, payloads ...[]byte) []datagram {
	ds := []datagram{}
	for _, p := range payloads {
		ds = append(ds, datagram{from: netip.MustParseAddrPort(from), data: p})
	}
	return ds
}

// One device answers ssdp:all once per service, and the M-SEARCH goes out three
// times on top of that, so without dedupe a single router gets its description
// fetched a dozen times. A reply without LOCATION names nothing to fetch.
func TestParseSSDP(t *testing.T) {
	got, err := parseSSDP(datagrams("10.0.0.1:1900",
		ssdpOK("http://10.0.0.1:80/desc.xml"),
		ssdpOK("http://10.0.0.1:80/desc.xml"),
		ssdpOK("http://10.0.0.2:80/desc.xml"),
		[]byte("HTTP/1.1 404 Not Found\r\nLOCATION: http://10.0.0.3/desc.xml\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nST: upnp:rootdevice\r\n\r\n"),
		[]byte("garbage-without-a-newline"),
		[]byte("HTTP/1.1\r\n\r\n"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d headers, want 2 unique locations", len(got))
	}
	if got[0].Get("LOCATION") != "http://10.0.0.1:80/desc.xml" {
		t.Errorf("first location = %q", got[0].Get("LOCATION"))
	}
	if got[0].Get("USN") == "" {
		t.Error("other ssdp headers must survive parsing")
	}
	_, err = parseSSDP(datagrams("10.0.0.9:1900", []byte("HTTP/1.1 500 Err\r\n\r\n")))
	if err == nil || !strings.Contains(err.Error(), "ssdp reply from 10.0.0.9:1900: 500 Err") {
		t.Errorf("want the sender and status in the error, got %v", err)
	}
}

// ssdp:all makes every UPnP device on the LAN answer, in random order, and each
// one ahead of the router costs a description plus every SCPD, or 10s if it
// stalls. A router's first reply is often the generic upnp:rootdevice, so its
// IGD search target can arrive after its LOCATION was already seen.
func TestParseSSDPGatewayFirst(t *testing.T) {
	got, err := parseSSDP(datagrams("10.0.0.1:1900",
		ssdpOK("http://10.0.0.7/tv.xml"),
		ssdpOK("http://10.0.0.1/igd.xml"),
		ssdpST("http://10.0.0.8/nas.xml", "urn:schemas-upnp-org:device:MediaServer:1"),
		ssdpST("http://10.0.0.1/igd.xml", "urn:schemas-upnp-org:device:InternetGatewayDevice:1"),
	))
	if err != nil {
		t.Fatal(err)
	}
	order := []string{}
	for _, h := range got {
		order = append(order, h.Get("LOCATION"))
	}
	want := []string{"http://10.0.0.1/igd.xml", "http://10.0.0.7/tv.xml", "http://10.0.0.8/nas.xml"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// net/http accepts a status line with no reason phrase; discovery must not be
// stricter than the stdlib.
func TestParseSSDPWithoutReasonPhrase(t *testing.T) {
	got, err := parseSSDP(datagrams("10.0.0.1:1900", []byte("HTTP/1.1 200\r\nLOCATION: http://10.0.0.1/desc.xml\r\n\r\n")))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v err=%v, want the reply accepted", got, err)
	}
}

// A dropped M-SEARCH is indistinguishable from a router without UPnP, so every
// search target goes out more than once.
func TestUDPRequestRetransmits(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 6.5s ssdp deadline")
	}
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port := srv.LocalAddr().(*net.UDPAddr).Port

	seen := make(chan int, 1)
	go func() {
		buf := make([]byte, 2048)
		n := 0
		srv.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, addr, err := srv.ReadFromUDP(buf)
			if err != nil {
				seen <- n
				return
			}
			n++
			srv.WriteToUDP(ssdpOK("http://10.0.0.1/desc.xml"), addr)
		}
	}()

	res, err := udpRequest("127.0.0.1", "127.0.0.1", port, [][]byte{mSearch("ssdp:all"), mSearch("upnp:rootdevice")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("no replies collected")
	}
	if res[0].from.Port() != uint16(port) {
		t.Errorf("reply from %v, want the responder's address so errors and logs can name it", res[0].from)
	}
	if n := <-seen; n != 6 {
		t.Errorf("router saw %d datagrams, want 6: both targets, three times", n)
	}
}

func TestGetLocalIPAddr(t *testing.T) {
	ip := GetLocalIPAddr()
	if ip == "" {
		t.Skip("no outbound route in this environment")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("GetLocalIPAddr() = %q, not an ip", ip)
	}
	if parsed.IsLoopback() || parsed.IsUnspecified() {
		t.Errorf("GetLocalIPAddr() = %q, want the outbound interface address", ip)
	}
}

// Router firmware emits sloppy XML. Rejecting a device over a bare ampersand in
// its friendlyName loses a router that otherwise works fine. encoding/xml also
// refuses any non-UTF-8 declaration outright, and passing the bytes through
// instead fails on the first latin-1 byte as invalid UTF-8.
func TestDeviceDescriptionTolerance(t *testing.T) {
	svc := `<serviceList><service><serviceType>urn:x:WANIPConnection:1</serviceType><SCPDURL>/scpd.xml</SCPDURL><controlURL>/ctl</controlURL></service></serviceList>`
	cases := map[string]string{
		"bare ampersand":   `<?xml version="1.0"?><root><device><friendlyName>Smith & Sons</friendlyName>` + svc + `</device></root>`,
		"undefined entity": `<?xml version="1.0"?><root><device><friendlyName>R&nbsp;D</friendlyName>` + svc + `</device></root>`,
		"truncated":        `<?xml version="1.0"?><root><device>` + svc + `</device>`,
		"well formed":      `<?xml version="1.0"?><root><device>` + svc + `</device></root>`,
		"latin-1":          "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><root><device><friendlyName>Telef\xf3nica</friendlyName>" + svc + "</device></root>",
	}
	for name, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		dd, err := deviceDescription(http.Header{"Location": []string{srv.URL}})
		srv.Close()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(dd.Device.ServiceList) != 1 {
			t.Errorf("%s: got %d services, want 1", name, len(dd.Device.ServiceList))
		}
	}
}

func TestDeviceDescriptionErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/500" {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
			return
		}
		io.WriteString(w, "<html>not a device description at all")
	}))
	defer srv.Close()

	_, err := deviceDescription(http.Header{"Location": []string{srv.URL + "/500"}})
	for _, want := range []string{srv.URL + "/500", "500 Internal Server Error", "boom"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, err := deviceDescription(http.Header{"Location": []string{srv.URL + "/junk"}}); err == nil || !strings.Contains(err.Error(), "not a device description") {
		t.Errorf("want an error saying what the body is not, got %v", err)
	}
}

func TestServiceActions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/full":
			io.WriteString(w, scpd(allActions()...))
		case "/partial":
			io.WriteString(w, scpd("GetGenericPortMappingEntry", getExternalIPAddress))
		case "/unrelated":
			io.WriteString(w, scpd("Browse", "Search"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, err := serviceActions(service{Location: srv.URL, SCPDURL: "/full"})
	if err != nil || len(got) != 3 {
		t.Errorf("full: got %v err=%v, want all three actions", got, err)
	}
	got, err = serviceActions(service{Location: srv.URL, SCPDURL: "/partial"})
	if err != nil || len(got) != 1 || got[0] != getExternalIPAddress {
		t.Errorf("partial: got %v err=%v, want just GetExternalIPAddress", got, err)
	}
	_, err = serviceActions(service{Location: srv.URL, SCPDURL: "/unrelated", ServiceType: "urn:x:ContentDirectory:1"})
	if err == nil || !strings.Contains(err.Error(), "urn:x:ContentDirectory:1 at "+srv.URL+"/unrelated: SCPD lists none of") {
		t.Errorf("a service with none of our actions must not qualify, and must say so: %v", err)
	}
	if _, err := serviceActions(service{Location: srv.URL, SCPDURL: "/404"}); err == nil {
		t.Error("want an error when the scpd is missing")
	}
}

// A service advertising only GetExternalIPAddress must not beat one that also
// does AddPortMapping, wherever it sits in the device tree. Routers with a
// partial action list are still kept as a candidate rather than rejected, and
// so is a WAN connection whose SCPD 404s: miniupnpc never reads the SCPD, so a
// router can ship a broken one and still work everywhere else. It ranks last.
func TestGetConnectionServiceRanksAndRecurses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/partial.xml":
			io.WriteString(w, scpd(getExternalIPAddress))
		case "/full.xml":
			io.WriteString(w, scpd(allActions()...))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root := device{
		ServiceList: []service{
			{ServiceType: "urn:x:WANPPPConnection:1", SCPDURL: "/nope.xml"},
			{ServiceType: "partial", SCPDURL: "/partial.xml"},
			{ServiceType: "unrelated", SCPDURL: "/nope.xml"},
		},
		DeviceList: []device{{
			ServiceList: []service{{ServiceType: "full", SCPDURL: "/full.xml"}},
		}},
	}

	got, err := getConnectionService(srv.URL, root)
	if err == nil {
		t.Error("want the 404s on /nope.xml reported as a diagnostic error")
	}
	if len(got) != 3 {
		t.Fatalf("got %d services, want 3 (partial and WAN kept, unrelated dropped)", len(got))
	}
	if got[0].ServiceType != "full" {
		t.Errorf("first = %q, want the nested full service", got[0].ServiceType)
	}
	if len(got[0].Actions) != 3 {
		t.Errorf("actions = %v, want all three recorded", got[0].Actions)
	}
	if got[2].ServiceType != "urn:x:WANPPPConnection:1" {
		t.Errorf("last = %q, want the WAN service whose SCPD failed", got[2].ServiceType)
	}
}

// Routers commonly list both WANIPConnection and WANPPPConnection with only one
// of them live; the dead one reports 0.0.0.0.
func TestConnectedServicePrefersLiveWAN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dead":
			io.WriteString(w, extIPResp("0.0.0.0"))
		case "/cgn":
			io.WriteString(w, extIPResp("100.70.1.2"))
		case "/public":
			io.WriteString(w, extIPResp(" 88.1.2.3\n"))
		default:
			io.WriteString(w, extIPResp("10.207.82.233"))
		}
	}))
	defer srv.Close()

	dead := service{Location: srv.URL, ControlURL: "/dead", ServiceType: "dead"}
	live := service{Location: srv.URL, ControlURL: "/live", ServiceType: "live"}
	got, ok := connectedService([]service{dead, live})
	if !ok || got.ServiceType != "live" {
		t.Errorf("picked %q ok=%v, want live", got.ServiceType, ok)
	}
	if _, ok := connectedService([]service{dead}); ok {
		t.Error("0.0.0.0 must not count as connected")
	}
	// A private WAN address is what a router behind carrier NAT reports, and it
	// is a working connection.
	if _, ok := connectedService([]service{live}); !ok {
		t.Error("a private external ip is still a connected wan")
	}
	// ISP routers also list management or VoIP connections with private
	// addresses beside the internet one, so a public address wins wherever it
	// sits (tailscale/tailscale#8364), and a CGN 100.64/10 address is private.
	cgn := service{Location: srv.URL, ControlURL: "/cgn", ServiceType: "cgn"}
	public := service{Location: srv.URL, ControlURL: "/public", ServiceType: "public"}
	if got, _ := connectedService([]service{live, cgn, public}); got.ServiceType != "public" {
		t.Errorf("picked %q, want the wan with a public address", got.ServiceType)
	}
}

func TestUpnpServicePicksConnectedService(t *testing.T) {
	srv := fakeRouter(t,
		fakeSvc{typ: "urn:x:WANPPPConnection:1", actions: allActions(), extIP: "0.0.0.0"},
		fakeSvc{typ: "urn:x:WANIPConnection:1", actions: allActions(), extIP: "88.1.2.3"},
	)
	got, err := upnpService([]http.Header{location(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ServiceType != "urn:x:WANIPConnection:1" {
		t.Errorf("picked %q, want the connected service", got.ServiceType)
	}
}

// Every WAN link is down. Discovery still has to hand back a usable service so
// the caller sees the router's own error rather than "no upnp found".
func TestUpnpServiceFallsBackWhenNothingConnected(t *testing.T) {
	srv := fakeRouter(t, fakeSvc{typ: "urn:x:WANIPConnection:1", actions: allActions(), extIP: "0.0.0.0"})
	got, err := upnpService([]http.Header{location(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ServiceType != "urn:x:WANIPConnection:1" {
		t.Errorf("got %q, want the sole candidate as fallback", got.ServiceType)
	}
}

// One unreachable device must not stop discovery from reaching the next.
func TestUpnpServiceSkipsBadDevices(t *testing.T) {
	good := fakeRouter(t, fakeSvc{typ: "urn:x:WANIPConnection:1", actions: allActions(), extIP: "88.1.2.3"})
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer dead.Close()

	got, err := upnpService([]http.Header{location(dead.URL), location(good.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ServiceType != "urn:x:WANIPConnection:1" {
		t.Errorf("got %q, want the reachable router", got.ServiceType)
	}
	if _, err := upnpService([]http.Header{location(dead.URL)}); err == nil {
		t.Error("want an error when no device is usable")
	}
}

// Router UPnP off, and every device that answered has no services at all. Each
// one contributed a nil error, errors.Join returned nil, and New handed back a
// client whose first call failed with `Post "": unsupported protocol scheme`.
func TestUpnpServiceErrorsWithoutServices(t *testing.T) {
	srv := fakeRouter(t)
	s, err := upnpService([]http.Header{location(srv.URL)})
	if err == nil {
		t.Fatalf("got %+v and no error, want an error", s)
	}
	// Named by make, model and type, so the router stands apart from TVs and
	// printers in a list of every device that answered.
	if !strings.Contains(err.Error(), "(Acme R1, urn:schemas-upnp-org:device:InternetGatewayDevice:1): no port mapping service") {
		t.Errorf("error lacks the device identity: %v", err)
	}
}

// Router traffic never leaves the LAN. On http.DefaultTransport it followed
// HTTP_PROXY, which does not exempt private addresses, so on a machine with a
// proxy configured the description fetch went to the proxy instead of the
// router. Keep-alive is off to match miniupnpc's one request per connection;
// otherwise discovery holds an idle connection to every device it probed.
func TestHTTPClientTransport(t *testing.T) {
	tr, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want a dedicated *http.Transport", httpClient.Transport)
	}
	if tr.Proxy != nil {
		t.Error("router traffic must not go through a proxy")
	}
	if !tr.DisableKeepAlives {
		t.Error("keep-alive must be off")
	}
}

// Routers with a strict SOAP parser reject the envelope encoding/xml produces
// from a struct, so the prefixes here are load-bearing.
func TestEnvelope(t *testing.T) {
	s := service{
		ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1",
		ControlURL:  "/ctl/IPConn",
		Location:    "http://10.0.0.1:5000/rootDesc.xml",
	}
	msg := GetExternalIPAddressRequest{XMLNS: s.ServiceType}
	req, err := newEnvelopeReq(getExternalIPAddress, s, &msg)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "http://10.0.0.1:5000/ctl/IPConn" {
		t.Errorf("control url = %s", req.URL)
	}
	// Checked on the wire rather than through the map: the key is deliberately
	// non-canonical, and Header.Get would look up "Soapaction" and miss it.
	wire := &bytes.Buffer{}
	req.Header.Write(wire)
	wantAction := "SOAPAction: \"urn:schemas-upnp-org:service:WANIPConnection:1#GetExternalIPAddress\"\r\n"
	if !strings.Contains(wire.String(), wantAction) {
		t.Errorf("headers = %q, want %q", wire, wantAction)
	}
	// Byte for byte what miniupnpc and libtorrent send, quotes included.
	if wantType := "Content-Type: text/xml; charset=\"utf-8\"\r\n"; !strings.Contains(wire.String(), wantType) {
		t.Errorf("headers = %q, want %q", wire, wantType)
	}
	b, _ := io.ReadAll(req.Body)
	got := string(b)
	for _, want := range []string{
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"`,
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"`,
		`<s:Body>`,
		`<u:GetExternalIPAddress xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">`,
		`</s:Body></s:Envelope>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, got)
		}
	}
	// The action element must not fall back to the Go type name; routers answer
	// 401 Invalid Action when it does.
	if strings.Contains(got, "GetExternalIPAddressRequest") {
		t.Errorf("action named after the go type: %s", got)
	}
}

// A router reports failures as HTTP 500 with the code buried in a SOAP fault.
// Returning that as an opaque body leaves callers unable to branch on it.
func TestFaultIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, faultResp("718"))
	}))
	defer srv.Close()

	c := &Client{service: service{Location: srv.URL, ControlURL: "/ctl"}}
	_, err := c.DeletePortMapping(DeletePortMappingRequest{NewExternalPort: 1, NewProtocol: "TCP"})
	var upnpErr *Error
	if !errors.As(err, &upnpErr) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if upnpErr.Code != ErrConflictInMappingEntry {
		t.Errorf("code = %d, want %d", upnpErr.Code, ErrConflictInMappingEntry)
	}
	if upnpErr.Action != deletePortMapping {
		t.Errorf("action = %q, want %q", upnpErr.Action, deletePortMapping)
	}
}

// A fault is a failure whatever the status line says. Checking for it only on
// non-200 replies reported a refused mapping as created.
func TestFaultInOKResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, faultResp("718"))
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	_, err := c.AddPortMapping(AddPortMappingRequest{NewExternalPort: 80, NewProtocol: "TCP"})
	var upnpErr *Error
	if !errors.As(err, &upnpErr) || upnpErr.Code != ErrConflictInMappingEntry {
		t.Errorf("want *Error %d, got %v", ErrConflictInMappingEntry, err)
	}
}

// Not every 500 carries a fault we can decode, and that must still be an error.
func TestNonFaultFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/html500" {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "<html>Internal Error</html>")
			return
		}
		io.WriteString(w, "not xml at all")
	}))
	defer srv.Close()

	c := &Client{service: service{Location: srv.URL, ControlURL: "/html500"}}
	var upnpErr *Error
	_, err := c.GetExternalIPAddress()
	if err == nil || errors.As(err, &upnpErr) {
		t.Errorf("want a plain error for an undecodable 500, got %v", err)
	}
	for _, want := range []string{srv.URL + "/html500", "500 Internal Server Error", "<html>Internal Error</html>"} {
		if err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	c = &Client{service: service{Location: srv.URL, ControlURL: "/junk200"}}
	if _, err := c.GetExternalIPAddress(); err == nil || !strings.Contains(err.Error(), "undecodable 200 OK response") {
		t.Errorf("want an error when a 200 body will not decode, got %v", err)
	}
}

// An error from a Client call has to stand alone in a bug report: what was
// asked, of which router and service, what its WAN looked like at discovery,
// and what the router answered.
func TestErrorShowsRequestAndGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, faultResp("718"))
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{
		Location: srv.URL, ControlURL: "/ctl", ServiceType: "urn:x:WANIPConnection:1",
		Device: "Acme R1", ExternalIP: "88.1.2.3",
	}}
	_, err := c.AddPortMapping(AddPortMappingRequest{NewInternalPort: 8080, NewProtocol: "tcp"})
	want := "map TCP 8080 -> 10.0.0.5:8080, lease 3600s on Acme R1 at " + srv.URL +
		"/ctl (urn:x:WANIPConnection:1, external ip 88.1.2.3 public): AddPortMapping: upnp error 718: test"
	if err == nil || err.Error() != want {
		t.Errorf("got  %v\nwant %s", err, want)
	}
	_, err = c.DeletePortMapping(DeletePortMappingRequest{NewExternalPort: 8080, NewProtocol: "tcp"})
	if err == nil || !strings.HasPrefix(err.Error(), "unmap TCP 8080 on Acme R1 at "+srv.URL+"/ctl") {
		t.Errorf("got %v", err)
	}
}

// Routers routinely leave errorDescription empty; the spec's name for the code
// takes its place so the error still says what went wrong.
func TestFaultNamesEmptyDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, strings.Replace(faultResp("606"), "<errorDescription>test</errorDescription>", "<errorDescription></errorDescription>", 1))
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	_, err := c.AddPortMapping(AddPortMappingRequest{NewInternalPort: 80})
	var upnpErr *Error
	if !errors.As(err, &upnpErr) || upnpErr.Description != "Action not authorized" {
		t.Errorf("got %v", err)
	}
}

// Errors carry the start of what a router sent, never a whole HTML page; the
// full body is in the debug log.
func TestSnippet(t *testing.T) {
	if got := snippet([]byte("  short \n")); got != "short" {
		t.Errorf("got %q", got)
	}
	got := snippet(bytes.Repeat([]byte("x"), 600))
	if !strings.HasPrefix(got, strings.Repeat("x", 512)+"...") || !strings.HasSuffix(got, "(88 more bytes)") {
		t.Errorf("got %q", got)
	}
}

// SOAP replies come from the same firmware as the description and get the same
// tolerance, latin-1 declaration included.
func TestSOAPResponseLatin1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(extIPResp("88.1.2.3"), `<?xml version="1.0"?>`, `<?xml version="1.0" encoding="ISO-8859-1"?>`, 1))
	}))
	defer srv.Close()

	c := &Client{service: service{Location: srv.URL, ControlURL: "/ctl"}}
	if ip, err := c.GetExternalIPAddress(); err != nil || ip.NewExternalIPAddress != "88.1.2.3" {
		t.Errorf("GetExternalIPAddress = %+v err=%v", ip, err)
	}
}

// 725 means the router only does permanent mappings, so the lease must drop to
// 0 and the call be retried rather than reported as a failure. The caller asked
// for a lease and got a mapping that outlives a crash, so the response says so.
func TestAddPortMappingRetriesPermanentLease(t *testing.T) {
	leases := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lease := "0"
		if !strings.Contains(string(b), "<NewLeaseDuration>0</NewLeaseDuration>") {
			lease = "nonzero"
		}
		leases = append(leases, lease)
		if lease != "0" {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, faultResp("725"))
			return
		}
		io.WriteString(w, respHead+`<u:AddPortMappingResponse xmlns:u="urn:x"></u:AddPortMappingResponse>`+respTail)
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	res, err := c.AddPortMapping(AddPortMappingRequest{
		NewExternalPort: 80, NewInternalPort: 80, NewProtocol: "TCP", NewEnabled: 1, NewLeaseDuration: 3600,
	})
	if err != nil {
		t.Fatalf("want retry to succeed, got %v", err)
	}
	if res.LeaseDuration != 0 {
		t.Errorf("LeaseDuration = %d, want 0 so the caller knows the mapping is permanent", res.LeaseDuration)
	}
	if len(leases) != 2 || leases[0] != "nonzero" || leases[1] != "0" {
		t.Errorf("calls = %v, want [nonzero 0]", leases)
	}
}

// A router that refuses even the retry must surface the fault, not loop.
func TestAddPortMappingGivesUpAfterOneRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, faultResp("725"))
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	_, err := c.AddPortMapping(AddPortMappingRequest{NewExternalPort: 80, NewLeaseDuration: 3600})
	if err == nil {
		t.Fatal("want the fault reported")
	}
	if !strings.Contains(err.Error(), "lease 0s after upnp error 725") {
		t.Errorf("error must say the lease-0 retry happened: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d calls, want exactly 2", calls)
	}
}

// The zero value must not ask for something dangerous or useless. External port
// 0 is a wildcard that older miniupnpd routers honoured by forwarding every
// external port (Tailscale advisory TS-2023-006). Lease 0 is permanent on IGDv1,
// and the readme's wiki recommends 3600. NewEnabled 0 asks for a disabled
// mapping: miniupnpd ignores it, but a stack that honours it answers 200 and
// forwards nothing.
func TestAddPortMappingDefaults(t *testing.T) {
	body := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body <- string(b)
		io.WriteString(w, respHead+`<u:AddPortMappingResponse xmlns:u="urn:x"></u:AddPortMappingResponse>`+respTail)
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewInternalPort: 80}); err != nil {
		t.Fatal(err)
	}
	got := <-body
	for _, want := range []string{
		"<NewExternalPort>80</NewExternalPort>",
		"<NewInternalClient>10.0.0.5</NewInternalClient>",
		"<NewEnabled>1</NewEnabled>",
		"<NewLeaseDuration>3600</NewLeaseDuration>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in %s", want, got)
		}
	}
	if _, err := c.AddPortMapping(AddPortMappingRequest{}); err == nil {
		t.Error("want an error rather than a request for external port 0")
	}
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewExternalPort: 80, NewInternalClient: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	if got := <-body; !strings.Contains(got, "<NewInternalClient>10.0.0.9</NewInternalClient>") {
		t.Errorf("an explicit internal client must win: %s", got)
	}
}

// Some routers reject a lower-case protocol (tailscale/tailscale#7377), and
// miniupnpc and libtorrent both send it upper-case.
func TestProtocolUpperCase(t *testing.T) {
	body := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body <- string(b)
		io.WriteString(w, respHead+respTail)
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewInternalPort: 80, NewProtocol: "tcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeletePortMapping(DeletePortMappingRequest{NewExternalPort: 80, NewProtocol: "udp"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<NewProtocol>TCP</NewProtocol>", "<NewProtocol>UDP</NewProtocol>"} {
		if got := <-body; !strings.Contains(got, want) {
			t.Errorf("want %s in %s", want, got)
		}
	}
}

// The whole point of the typed error is that Code survives to the caller, and
// routers routinely leave errorDescription empty.
func TestErrorString(t *testing.T) {
	got := (&Error{Action: addPortMapping, Code: 718, Description: "ConflictInMappingEntry"}).Error()
	if got != "AddPortMapping: upnp error 718: ConflictInMappingEntry" {
		t.Errorf("got %q", got)
	}
	if got := (&Error{Action: addPortMapping, Code: 501}).Error(); got != "AddPortMapping: upnp error 501" {
		t.Errorf("empty description: got %q", got)
	}
}

// The three public operations against a full fake router, from discovery to
// decoded response.
func TestClientOperations(t *testing.T) {
	srv := fakeRouter(t, fakeSvc{typ: "urn:x:WANIPConnection:1", actions: allActions(), extIP: "88.1.2.3"})
	s, err := upnpService([]http.Header{location(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{LocalIP: "10.0.0.5", service: s}

	ip, err := c.GetExternalIPAddress()
	if err != nil || ip.NewExternalIPAddress != "88.1.2.3" {
		t.Errorf("GetExternalIPAddress = %+v err=%v", ip, err)
	}
	if res, err := c.AddPortMapping(AddPortMappingRequest{
		NewExternalPort: 8080, NewInternalPort: 8080, NewProtocol: "TCP", NewEnabled: 1, NewLeaseDuration: 3600,
	}); err != nil || res.LeaseDuration != 3600 {
		t.Errorf("AddPortMapping = %+v err=%v, want the requested lease reported", res, err)
	}
	if _, err := c.DeletePortMapping(DeletePortMappingRequest{
		NewExternalPort: 8080, NewProtocol: "TCP",
	}); err != nil {
		t.Errorf("DeletePortMapping: %v", err)
	}
}

// Logger is how a failure on someone else's network gets diagnosed: one run has
// to show every reply and its sender, which device each location is, every
// service and WAN probe, the choice, and the SOAP bodies both ways.
func TestLogging(t *testing.T) {
	buf := &bytes.Buffer{}
	Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { Logger = nil })

	srv := fakeRouter(t,
		fakeSvc{typ: "urn:x:WANPPPConnection:1", actions: allActions(), extIP: "0.0.0.0"},
		fakeSvc{typ: "urn:x:WANIPConnection:1", actions: allActions(), extIP: "88.1.2.3"},
	)
	if _, err := parseSSDP(datagrams("10.0.0.1:1900", ssdpOK(srv.URL+"/desc.xml"))); err != nil {
		t.Fatal(err)
	}
	s, err := upnpService([]http.Header{location(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{LocalIP: "10.0.0.5", service: s}
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewInternalPort: 8080, NewProtocol: "TCP"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeletePortMapping(DeletePortMappingRequest{NewExternalPort: 8080, NewProtocol: "TCP"}); err != nil {
		t.Fatal(err)
	}
	log := buf.String()
	for _, want := range []string{
		`msg="ssdp reply" from=10.0.0.1:1900`,
		`server="Linux UPnP/1.1 fake/1.0"`,
		`level=INFO msg="ssdp done" replies=1 devices=1`,
		`msg=device location=` + srv.URL + `/desc.xml`,
		`manufacturer=Acme model=R1`,
		`msg=service base=`,
		`actions="[AddPortMapping DeletePortMapping GetExternalIPAddress]"`,
		`external_ip=0.0.0.0 class=unusable`,
		`external_ip=88.1.2.3 class=public`,
		`level=INFO msg="gateway selected" device="Acme R1"`,
		`msg="soap request" action=AddPortMapping`,
		`<NewExternalPort>8080</NewExternalPort>`,
		`msg="soap response" action=AddPortMapping`,
		`status="200 OK"`,
		`level=INFO msg="port mapping added" external=8080 protocol=TCP internal_client=10.0.0.5 internal_port=8080 lease=3600`,
		`level=INFO msg="port mapping deleted" external=8080 protocol=TCP`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s", want)
		}
	}
}
