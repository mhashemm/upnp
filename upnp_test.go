package upnp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
		`<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType><serviceList>`
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

// net/http cannot serialize a non-HTTP method to "*", so the datagram is hand
// built and its shape is entirely on us.
func TestHTTPRequest(t *testing.T) {
	got := string(httpRequest(http.Request{
		Method: "M-SEARCH",
		Header: http.Header{"ST": []string{"ssdp:all"}, "MX": []string{"5"}},
	}))
	if !strings.HasPrefix(got, "M-SEARCH * HTTP/1.1\r\n") {
		t.Errorf("bad request line: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Errorf("headers must end with a blank line: %q", got)
	}
	for _, want := range []string{"ST: ssdp:all\r\n", "MX: 5\r\n", "USER-AGENT: "} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// Header.Set would canonicalize these to St/Mx; SSDP wants them uppercase.
	if strings.Contains(got, "St:") || strings.Contains(got, "Mx:") {
		t.Errorf("header casing was canonicalized: %q", got)
	}
}

func ssdpOK(location string) []byte {
	return []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nLOCATION: " + location +
		"\r\nST: upnp:rootdevice\r\nUSN: uuid:1::upnp:rootdevice\r\n\r\n")
}

// One device answers ssdp:all once per service, and the M-SEARCH goes out three
// times on top of that, so without dedupe a single router gets its description
// fetched a dozen times.
func TestParseSSDP(t *testing.T) {
	got, err := parseSSDP([][]byte{
		ssdpOK("http://10.0.0.1:80/desc.xml"),
		ssdpOK("http://10.0.0.1:80/desc.xml"),
		ssdpOK("http://10.0.0.2:80/desc.xml"),
		[]byte("HTTP/1.1 404 Not Found\r\nLOCATION: http://10.0.0.3/desc.xml\r\n\r\n"),
		[]byte("garbage-without-a-newline"),
		[]byte("HTTP/1.1\r\n\r\n"),
	})
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
	if _, err := parseSSDP([][]byte{[]byte("HTTP/1.1 500 Err\r\n\r\n")}); err == nil {
		t.Error("want an error when nothing usable was parsed")
	}
}

// A dropped M-SEARCH is indistinguishable from a router without UPnP, so the
// datagram goes out more than once.
func TestUDPRequestRetransmits(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 5s read deadline")
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

	res, err := udpRequest("127.0.0.1", "127.0.0.1", port, []byte("M-SEARCH * HTTP/1.1\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("no replies collected")
	}
	if n := <-seen; n != 3 {
		t.Errorf("router saw %d datagrams, want 3", n)
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
// its friendlyName loses a router that otherwise works fine.
func TestDeviceDescriptionTolerance(t *testing.T) {
	svc := `<serviceList><service><serviceType>urn:x:WANIPConnection:1</serviceType><SCPDURL>/scpd.xml</SCPDURL><controlURL>/ctl</controlURL></service></serviceList>`
	cases := map[string]string{
		"bare ampersand":   `<?xml version="1.0"?><root><device><friendlyName>Smith & Sons</friendlyName>` + svc + `</device></root>`,
		"undefined entity": `<?xml version="1.0"?><root><device><friendlyName>R&nbsp;D</friendlyName>` + svc + `</device></root>`,
		"truncated":        `<?xml version="1.0"?><root><device>` + svc + `</device>`,
		"well formed":      `<?xml version="1.0"?><root><device>` + svc + `</device></root>`,
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

	if _, err := deviceDescription(http.Header{"Location": []string{srv.URL + "/500"}}); err == nil {
		t.Error("want an error on a non-200 description")
	}
	if _, err := deviceDescription(http.Header{"Location": []string{srv.URL + "/junk"}}); err == nil {
		t.Error("want an error when nothing usable parsed")
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
	if _, err := serviceActions(service{Location: srv.URL, SCPDURL: "/unrelated"}); err == nil {
		t.Error("a service with none of our actions must not qualify")
	}
	if _, err := serviceActions(service{Location: srv.URL, SCPDURL: "/404"}); err == nil {
		t.Error("want an error when the scpd is missing")
	}
}

// A service advertising only GetExternalIPAddress must not beat one that also
// does AddPortMapping, wherever it sits in the device tree. Routers with a
// partial action list are still kept as a candidate rather than rejected.
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
			{ServiceType: "partial", SCPDURL: "/partial.xml"},
			{ServiceType: "unrelated", SCPDURL: "/nope.xml"},
		},
		DeviceList: []device{{
			ServiceList: []service{{ServiceType: "full", SCPDURL: "/full.xml"}},
		}},
	}

	got, err := getConnectionService(srv.URL, root)
	if err == nil {
		t.Error("want the 404 on /nope.xml reported as a diagnostic error")
	}
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2 (the partial one is kept)", len(got))
	}
	if got[0].ServiceType != "full" {
		t.Errorf("first = %q, want the nested full service", got[0].ServiceType)
	}
	if len(got[0].Actions) != 3 {
		t.Errorf("actions = %v, want all three recorded", got[0].Actions)
	}
}

// Routers commonly list both WANIPConnection and WANPPPConnection with only one
// of them live; the dead one reports 0.0.0.0.
func TestConnectedServicePrefersLiveWAN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dead" {
			io.WriteString(w, extIPResp("0.0.0.0"))
			return
		}
		io.WriteString(w, extIPResp("10.207.82.233"))
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
	c = &Client{service: service{Location: srv.URL, ControlURL: "/junk200"}}
	if _, err := c.GetExternalIPAddress(); err == nil {
		t.Error("want an error when a 200 body will not decode")
	}
}

// 725 means the router only does permanent mappings, so the lease must drop to
// 0 and the call be retried rather than reported as a failure.
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
	if _, err := c.AddPortMapping(AddPortMappingRequest{
		NewExternalPort: 80, NewInternalPort: 80, NewProtocol: "TCP", NewEnabled: 1, NewLeaseDuration: 3600,
	}); err != nil {
		t.Fatalf("want retry to succeed, got %v", err)
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
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewExternalPort: 80, NewLeaseDuration: 3600}); err == nil {
		t.Fatal("want the fault reported")
	}
	if calls != 2 {
		t.Errorf("made %d calls, want exactly 2", calls)
	}
}

func TestAddPortMappingDefaultsInternalClient(t *testing.T) {
	body := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body <- string(b)
		io.WriteString(w, respHead+`<u:AddPortMappingResponse xmlns:u="urn:x"></u:AddPortMappingResponse>`+respTail)
	}))
	defer srv.Close()

	c := &Client{LocalIP: "10.0.0.5", service: service{Location: srv.URL, ControlURL: "/ctl"}}
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewExternalPort: 80, NewInternalPort: 80}); err != nil {
		t.Fatal(err)
	}
	if got := <-body; !strings.Contains(got, "<NewInternalClient>10.0.0.5</NewInternalClient>") {
		t.Errorf("want LocalIP filled in as the internal client: %s", got)
	}
	if _, err := c.AddPortMapping(AddPortMappingRequest{NewExternalPort: 80, NewInternalClient: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	if got := <-body; !strings.Contains(got, "<NewInternalClient>10.0.0.9</NewInternalClient>") {
		t.Errorf("an explicit internal client must win: %s", got)
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
	if _, err := c.AddPortMapping(AddPortMappingRequest{
		NewExternalPort: 8080, NewInternalPort: 8080, NewProtocol: "TCP", NewEnabled: 1,
	}); err != nil {
		t.Errorf("AddPortMapping: %v", err)
	}
	if _, err := c.DeletePortMapping(DeletePortMappingRequest{
		NewExternalPort: 8080, NewProtocol: "TCP",
	}); err != nil {
		t.Errorf("DeletePortMapping: %v", err)
	}
}
