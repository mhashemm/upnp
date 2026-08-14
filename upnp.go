package upnp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"time"
)

func New() (*Client, error) {
	localIP := GetLocalIPAddr()
	if localIP == "" {
		return nil, errors.New("no outbound route: cannot determine local ip")
	}
	headers, err := discover(localIP)
	if err != nil {
		return nil, fmt.Errorf("no ssdp reply on %s (a vpn or virtual interface can route the multicast away from your lan): %w", localIP, err)
	}
	service, err := upnpService(headers)
	if err != nil {
		return nil, err
	}
	return &Client{
		LocalIP: localIP,
		service: service,
	}, nil
}

func GetLocalIPAddr() string {
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   []byte{6, 9, 6, 9},
		Port: 6969,
	})
	if err != nil {
		return ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Unix(0, 0))
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func upnpService(headers []http.Header) (service, error) {
	errs := []error{}
	all := []service{}
	for _, header := range headers {
		dd, err := deviceDescription(header)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		base := dd.BaseUrl
		if base == "" {
			base = header.Get("LOCATION")
		}
		candidates, err := getConnectionService(base, dd.Device)
		if len(candidates) == 0 {
			errs = append(errs, err)
			continue
		}
		if s, ok := connectedService(candidates); ok {
			return s, nil
		}
		all = append(all, candidates...)
	}
	if len(all) > 0 {
		return all[0], nil
	}
	return service{}, errors.Join(errs...)
}

func connectedService(candidates []service) (service, bool) {
	for _, s := range candidates {
		res, err := (&Client{service: s}).GetExternalIPAddress()
		if err != nil {
			continue
		}
		if ip := res.NewExternalIPAddress; ip != "" && ip != "0.0.0.0" {
			return s, true
		}
	}
	return service{}, false
}

func discover(localIP string) ([]http.Header, error) {
	header := http.Header{}
	header["HOST"] = []string{"239.255.255.250:1900"}
	header["ST"] = []string{"ssdp:all"}
	header["MAN"] = []string{"\"ssdp:discover\""}
	header["MX"] = []string{strconv.Itoa(int(ssdpMX / time.Second))}
	req := http.Request{
		Method: "M-SEARCH",
		Header: header,
	}
	devices, err := udpRequest(localIP, "239.255.255.250", 1900, httpRequest(req))
	if err != nil {
		return nil, err
	}
	return parseSSDP(devices)
}

func parseSSDP(devices [][]byte) ([]http.Header, error) {
	headers := []http.Header{}
	errs := []error{}
	seen := map[string]bool{}
	for _, res := range devices {
		httpRes, headerRes, found := bytes.Cut(res, []byte{'\n'})
		if !found {
			errs = append(errs, fmt.Errorf("invalid response: %s\n", res))
			continue
		}
		s := bytes.Split(httpRes, []byte{' '})
		if len(s) < 3 {
			errs = append(errs, fmt.Errorf("invalid response: %s\n", headerRes))
			continue
		}
		statusCode := string(s[1])
		if statusCode != strconv.Itoa(http.StatusOK) {
			errs = append(errs, fmt.Errorf("not ok: %s\n", headerRes))
			continue
		}
		_header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(headerRes))).ReadMIMEHeader()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		header := http.Header(_header)
		location := header.Get("LOCATION")
		if seen[location] {
			continue
		}
		seen[location] = true
		headers = append(headers, header)
	}
	if len(headers) == 0 {
		return nil, errors.Join(errs...)
	}
	return headers, nil
}

func deviceDescription(header http.Header) (root, error) {
	r := root{}
	location := header.Get("LOCATION")
	res, err := httpClient.Get(location)
	if err != nil {
		return r, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return r, err
	}
	if res.StatusCode != http.StatusOK {
		return r, fmt.Errorf("%s: %s", res.Status, body)
	}
	if err := decodeXML(body, &r); err != nil && len(r.Device.ServiceList) == 0 && len(r.Device.DeviceList) == 0 {
		return r, fmt.Errorf("%s: %w", location, err)
	}
	return r, nil
}

func serviceActions(s service) ([]string, error) {
	url, err := resolveURL(s.Location, s.SCPDURL)
	if err != nil {
		return nil, err
	}
	res, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s responed with status=%d body=[%s]", url, res.StatusCode, body)
	}
	desc := description{}
	if err := decodeXML(body, &desc); err != nil && len(desc.ActionList) == 0 {
		return nil, err
	}

	actions := []string{}
	for _, a := range desc.ActionList {
		if slices.Contains(supported, a.Name) {
			actions = append(actions, a.Name)
		}
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("%s service is not valid", url)
	}
	return actions, nil
}

func getConnectionService(location string, rootDevice device) ([]service, error) {
	services := []service{}
	errs := []error{}
	for _, s := range rootDevice.ServiceList {
		s.Location = location
		actions, err := serviceActions(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.Actions = actions
		services = append(services, s)
	}
	for _, d := range rootDevice.DeviceList {
		s, err := getConnectionService(location, d)
		services = append(services, s...)
		errs = append(errs, err)
	}
	slices.SortStableFunc(services, func(a, b service) int { return len(b.Actions) - len(a.Actions) })
	return services, errors.Join(errs...)
}

func (c *Client) AddPortMapping(msg AddPortMappingRequest) (AddPortMappingResponse, error) {
	if msg.NewInternalClient == "" {
		msg.NewInternalClient = c.LocalIP
	}
	msg.XMLNS = c.service.ServiceType
	req, err := newEnvelopeReq(addPortMapping, c.service, &msg)
	if err != nil {
		return AddPortMappingResponse{}, err
	}
	res, err := doRequest(addPortMapping, req)
	var upnpErr *Error
	if errors.As(err, &upnpErr) && upnpErr.Code == ErrOnlyPermanentLease && msg.NewLeaseDuration != 0 {
		msg.NewLeaseDuration = 0
		return c.AddPortMapping(msg)
	}
	if err != nil {
		return AddPortMappingResponse{}, err
	}
	if res.Body.AddPortMappingResponse == nil {
		return AddPortMappingResponse{}, nil
	}
	return *res.Body.AddPortMappingResponse, nil
}

func (c *Client) DeletePortMapping(msg DeletePortMappingRequest) (DeletePortMappingResponse, error) {
	msg.XMLNS = c.service.ServiceType
	req, err := newEnvelopeReq(deletePortMapping, c.service, &msg)
	if err != nil {
		return DeletePortMappingResponse{}, err
	}
	res, err := doRequest(deletePortMapping, req)
	if err != nil {
		return DeletePortMappingResponse{}, err
	}
	if res.Body.DeletePortMappingResponse == nil {
		return DeletePortMappingResponse{}, nil
	}
	return *res.Body.DeletePortMappingResponse, nil
}

func (c *Client) GetExternalIPAddress() (GetExternalIPAddressResponse, error) {
	msg := GetExternalIPAddressRequest{
		XMLNS: c.service.ServiceType,
	}
	req, err := newEnvelopeReq(getExternalIPAddress, c.service, &msg)
	if err != nil {
		return GetExternalIPAddressResponse{}, err
	}
	res, err := doRequest(getExternalIPAddress, req)
	if err != nil {
		return GetExternalIPAddressResponse{}, err
	}
	if res.Body.GetExternalIPAddressResponse == nil {
		return GetExternalIPAddressResponse{}, nil
	}
	return *res.Body.GetExternalIPAddressResponse, nil
}
