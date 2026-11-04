package upnp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

func New() (*Client, error) {
	return NewContext(context.Background())
}

func NewContext(c context.Context) (*Client, error) {
	c, cancel := context.WithTimeout(c, discoveryTimeout)
	defer cancel()
	localIP, err := localIPAddr()
	if err != nil {
		return nil, fmt.Errorf("no outbound route, cannot determine the local ip: %w", err)
	}
	headers, err := discover(c, localIP)
	if err != nil {
		return nil, err
	}
	service, err := upnpService(c, headers)
	if err != nil {
		return nil, fmt.Errorf("no upnp gateway among the %d devices that answered:\n%w", len(headers), err)
	}
	return &Client{
		LocalIP: localIP,
		service: service,
	}, nil
}

func GetLocalIPAddr() string {
	ip, _ := localIPAddr()
	return ip
}

func localIPAddr() (string, error) {
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   []byte{6, 9, 6, 9},
		Port: 6969,
	})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func upnpService(c context.Context, headers []http.Header) (service, error) {
	errs := []error{}
	all := []service{}
	private := []service{}
	for _, header := range headers {
		if err := c.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		location := header.Get("LOCATION")
		dd, err := deviceDescription(c, header)
		if err != nil {
			debug("device skipped", "location", location, "err", err)
			errs = append(errs, err)
			continue
		}
		base := dd.BaseUrl
		if base == "" || !sameHost(location, base) {
			base = location
		}
		candidates, err := getConnectionService(c, base, dd.Device)
		if len(candidates) == 0 {
			if err == nil {
				err = errors.New("no service offers " + strings.Join(supported, ", "))
			}
			err = fmt.Errorf("%s%s: no port mapping service: %w", location, note(identity(dd), dd.Device.DeviceType), err)
			debug("device skipped", "location", location, "err", err)
			errs = append(errs, err)
			continue
		}
		for i := range candidates {
			candidates[i].Device = identity(dd)
		}
		if s, ok := connectedService(c, candidates); ok {
			if IPClass(s.ExternalIP) == IPPublic {
				selected(s)
				return s, nil
			}
			private = append(private, s)
		}
		all = append(all, candidates...)
	}
	if len(private) > 0 {
		selected(private[0])
		return private[0], nil
	}
	if len(all) > 0 {
		selected(all[0])
		return all[0], nil
	}
	return service{}, errors.Join(errs...)
}

func identity(dd root) string {
	return strings.Join(strings.Fields(dd.Device.Manufacturer+" "+dd.Device.ModelName+" "+dd.Device.ModelNumber), " ")
}

func note(parts ...string) string {
	kept := []string{}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return " (" + strings.Join(kept, ", ") + ")"
}

func (s service) String() string {
	control, err := resolveURL(s.Location, s.ControlURL)
	if err != nil {
		control = s.ControlURL
	}
	wan := ""
	if s.ExternalIP != "" {
		wan = "external ip " + strings.TrimSpace(s.ExternalIP) + " " + IPClass(s.ExternalIP)
	}
	gateway := control + note(s.ServiceType, wan)
	if s.Device != "" {
		gateway = s.Device + " at " + gateway
	}
	return gateway
}

func selected(s service) {
	info("gateway selected", "device", s.Device, "base", s.Location, "service", s.ServiceType, "control", s.ControlURL,
		"external_ip", s.ExternalIP, "class", IPClass(s.ExternalIP))
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

const (
	IPPublic   = "public"
	IPPrivate  = "private"
	IPUnusable = "unusable"
)

func IPClass(s string) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	ip = ip.Unmap()
	switch {
	case err != nil || !ip.IsGlobalUnicast():
		return IPUnusable
	case ip.IsPrivate() || slices.ContainsFunc(nonPublic, func(p netip.Prefix) bool { return p.Contains(ip) }):
		return IPPrivate
	}
	return IPPublic
}

func connectedService(c context.Context, candidates []service) (service, bool) {
	private := []service{}
	for i := range candidates {
		s := &candidates[i]
		res, err := (&Client{service: *s}).externalIP(c)
		s.ExternalIP = res.NewExternalIPAddress
		class := IPClass(s.ExternalIP)
		debug("wan probe", "service", s.ServiceType, "control", s.ControlURL, "external_ip", s.ExternalIP, "class", class, "err", err)
		if class == IPPublic {
			return *s, true
		}
		if class == IPPrivate {
			private = append(private, *s)
		}
	}
	if len(private) == 0 {
		return service{}, false
	}
	return private[0], true
}

func discover(c context.Context, localIP string) ([]http.Header, error) {
	payloads := [][]byte{}
	for _, st := range ssdpTargets {
		payloads = append(payloads, mSearch(st))
	}
	debug("ssdp search", "local", localIP, "targets", ssdpTargets, "wait", ssdpWait)
	devices, err := udpRequest(c, localIP, "239.255.255.250", 1900, payloads, ssdpRetryGap, ssdpWait)
	if err != nil {
		return nil, fmt.Errorf("no ssdp reply within %s to %d searches sent %d times from %s; upnp may be off on the router, or a vpn, virtual interface or firewall is in the way: %w",
			ssdpWait, len(ssdpTargets), ssdpRetries+1, localIP, err)
	}
	return parseSSDP(devices)
}

func parseSSDP(devices []datagram) ([]http.Header, error) {
	headers := []http.Header{}
	errs := []error{}
	seen := map[string]bool{}
	rank := map[string]int{}
	for _, d := range devices {
		res, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(d.data)), nil)
		if err != nil {
			debug("ssdp reply unreadable", "from", d.from, "err", err, "raw", string(d.data))
			errs = append(errs, fmt.Errorf("ssdp reply from %s: %w: %q", d.from, err, snippet(d.data)))
			continue
		}
		location := res.Header.Get("LOCATION")
		debug("ssdp reply", "from", d.from, "status", res.Status, "st", res.Header.Get("ST"), "location", location,
			"server", res.Header.Get("SERVER"), "usn", res.Header.Get("USN"))
		if res.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Errorf("ssdp reply from %s: %s", d.from, res.Status))
			continue
		}
		if location == "" {
			errs = append(errs, fmt.Errorf("ssdp reply from %s has no LOCATION", d.from))
			continue
		}
		if !fromItself(location, d.from.Addr()) {
			errs = append(errs, fmt.Errorf("ssdp reply from %s has a LOCATION elsewhere: %q", d.from, location))
			continue
		}
		if st := res.Header.Get("ST"); strings.Contains(st, "InternetGatewayDevice") || wanConnection(st) {
			rank[location] = -1
		}
		if seen[location] {
			continue
		}
		seen[location] = true
		headers = append(headers, res.Header)
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("%d ssdp replies, none usable:\n%w", len(devices), errors.Join(errs...))
	}
	slices.SortStableFunc(headers, func(a, b http.Header) int {
		return rank[a.Get("LOCATION")] - rank[b.Get("LOCATION")]
	})
	info("ssdp done", "replies", len(devices), "devices", len(headers), "gateways", len(rank))
	return headers, nil
}

func fromItself(location string, sender netip.Addr) bool {
	u, err := url.Parse(location)
	if err != nil {
		return false
	}
	host, err := netip.ParseAddr(u.Hostname())
	return err == nil && host.Unmap() == sender.Unmap()
}

func wanConnection(s string) bool {
	return strings.Contains(s, "WANIPConnection") || strings.Contains(s, "WANPPPConnection")
}

func deviceDescription(c context.Context, header http.Header) (root, error) {
	r := root{}
	location := header.Get("LOCATION")
	body, res, err := get(c, location)
	if err != nil {
		return r, err
	}
	if err := decodeXML(body, &r); err != nil && len(r.Device.ServiceList) == 0 && len(r.Device.DeviceList) == 0 {
		return r, fmt.Errorf("GET %s: not a device description: %w: %s", location, err, snippet(body))
	}
	debug("device", "location", location, "server", res.Header.Get("Server"), "type", r.Device.DeviceType,
		"manufacturer", r.Device.Manufacturer, "model", r.Device.ModelName, "number", r.Device.ModelNumber,
		"name", r.Device.FriendlyName, "urlbase", r.BaseUrl, "body", string(body))
	return r, nil
}

func serviceActions(c context.Context, s service) ([]string, error) {
	url, err := resolveURL(s.Location, s.SCPDURL)
	if err != nil {
		return nil, err
	}
	body, _, err := get(c, url)
	if err != nil {
		return nil, err
	}
	desc := description{}
	if err := decodeXML(body, &desc); err != nil && len(desc.ActionList) == 0 {
		return nil, fmt.Errorf("GET %s: not an SCPD: %w: %s", url, err, snippet(body))
	}

	actions := []string{}
	for _, a := range desc.ActionList {
		if slices.Contains(supported, a.Name) {
			actions = append(actions, a.Name)
		}
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("%s at %s: SCPD lists none of %s", s.ServiceType, url, strings.Join(supported, ", "))
	}
	return actions, nil
}

func getConnectionService(c context.Context, location string, rootDevice device) ([]service, error) {
	services := []service{}
	errs := []error{}
	for _, s := range rootDevice.ServiceList {
		if err := c.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if !sameHost(location, s.ControlURL) || !sameHost(location, s.SCPDURL) {
			errs = append(errs, fmt.Errorf("%s points away from %s: %q, %q", s.ServiceType, location, s.ControlURL, s.SCPDURL))
			continue
		}
		s.Location = location
		actions, err := serviceActions(c, s)
		keep := err == nil || wanConnection(s.ServiceType)
		debug("service", "base", location, "type", s.ServiceType, "control", s.ControlURL, "scpd", s.SCPDURL,
			"actions", actions, "candidate", keep, "err", err)
		if err != nil {
			errs = append(errs, err)
		}
		if !keep {
			continue
		}
		s.Actions = actions
		services = append(services, s)
	}
	for _, d := range rootDevice.DeviceList {
		s, err := getConnectionService(c, location, d)
		services = append(services, s...)
		errs = append(errs, err)
	}
	slices.SortStableFunc(services, func(a, b service) int { return len(b.Actions) - len(a.Actions) })
	return services, errors.Join(errs...)
}

func (c *Client) fail(what string, err error) error {
	return fmt.Errorf("%s on %s: %w", what, c.service, err)
}

func mapping(msg AddPortMappingRequest, retried bool) string {
	what := fmt.Sprintf("map %s %d -> %s:%d, lease %ds", msg.NewProtocol, msg.NewExternalPort, msg.NewInternalClient, msg.NewInternalPort, msg.NewLeaseDuration)
	if retried {
		what += " after upnp error 725"
	}
	return what
}

func (c *Client) AddPortMapping(msg AddPortMappingRequest) (AddPortMappingResponse, error) {
	if msg.NewExternalPort == 0 {
		msg.NewExternalPort = msg.NewInternalPort
	}
	if msg.NewExternalPort == 0 {
		return AddPortMappingResponse{}, errors.New("AddPortMapping: NewInternalPort or NewExternalPort is required")
	}
	if msg.NewInternalClient == "" {
		msg.NewInternalClient = c.LocalIP
	}
	if msg.NewEnabled == 0 {
		msg.NewEnabled = 1
	}
	if msg.NewLeaseDuration == 0 {
		msg.NewLeaseDuration = 3600
	}
	msg.NewProtocol = strings.ToUpper(msg.NewProtocol)
	msg.XMLNS = c.service.ServiceType
	retried := false
	for {
		req, err := newEnvelopeReq(context.Background(), addPortMapping, c.service, &msg)
		if err != nil {
			return AddPortMappingResponse{}, c.fail(mapping(msg, retried), err)
		}
		res, err := doRequest(addPortMapping, req)
		var upnpErr *Error
		if errors.As(err, &upnpErr) && upnpErr.Code == ErrOnlyPermanentLease && msg.NewLeaseDuration != 0 {
			info("router only allows permanent leases, retrying with lease 0", "requested", msg.NewLeaseDuration)
			msg.NewLeaseDuration = 0
			retried = true
			continue
		}
		if err != nil {
			return AddPortMappingResponse{}, c.fail(mapping(msg, retried), err)
		}
		info("port mapping added", "external", msg.NewExternalPort, "protocol", msg.NewProtocol,
			"internal_client", msg.NewInternalClient, "internal_port", msg.NewInternalPort,
			"lease", msg.NewLeaseDuration, "gateway", c.service.String())
		out := AddPortMappingResponse{}
		if res.Body.AddPortMappingResponse != nil {
			out = *res.Body.AddPortMappingResponse
		}
		out.LeaseDuration = msg.NewLeaseDuration
		return out, nil
	}
}

func (c *Client) DeletePortMapping(msg DeletePortMappingRequest) (DeletePortMappingResponse, error) {
	if msg.NewExternalPort == 0 {
		return DeletePortMappingResponse{}, errors.New("DeletePortMapping: NewExternalPort is required")
	}
	msg.NewProtocol = strings.ToUpper(msg.NewProtocol)
	msg.XMLNS = c.service.ServiceType
	what := fmt.Sprintf("unmap %s %d", msg.NewProtocol, msg.NewExternalPort)
	req, err := newEnvelopeReq(context.Background(), deletePortMapping, c.service, &msg)
	if err != nil {
		return DeletePortMappingResponse{}, c.fail(what, err)
	}
	res, err := doRequest(deletePortMapping, req)
	if err != nil {
		return DeletePortMappingResponse{}, c.fail(what, err)
	}
	info("port mapping deleted", "external", msg.NewExternalPort, "protocol", msg.NewProtocol, "gateway", c.service.String())
	if res.Body.DeletePortMappingResponse == nil {
		return DeletePortMappingResponse{}, nil
	}
	return *res.Body.DeletePortMappingResponse, nil
}

func (c *Client) GetExternalIPAddress() (GetExternalIPAddressResponse, error) {
	return c.externalIP(context.Background())
}

func (c *Client) externalIP(ctx context.Context) (GetExternalIPAddressResponse, error) {
	msg := GetExternalIPAddressRequest{
		XMLNS: c.service.ServiceType,
	}
	req, err := newEnvelopeReq(ctx, getExternalIPAddress, c.service, &msg)
	if err != nil {
		return GetExternalIPAddressResponse{}, c.fail("get external ip", err)
	}
	res, err := doRequest(getExternalIPAddress, req)
	if err != nil {
		return GetExternalIPAddressResponse{}, c.fail("get external ip", err)
	}
	if res.Body.GetExternalIPAddressResponse == nil {
		return GetExternalIPAddressResponse{}, nil
	}
	return *res.Body.GetExternalIPAddressResponse, nil
}
