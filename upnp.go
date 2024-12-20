package upnp

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"
)

func New() (*Client, error) {
	localIP := GetLocalIPAddr()
	headers, err := discover(localIP)
	if err != nil {
		return nil, err
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
	conn, _ := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   []byte{6, 9, 6, 9},
		Port: 6969,
	})
	conn.SetDeadline(time.Unix(0, 0))
	defer conn.Close()
	return strings.Split(conn.LocalAddr().String(), ":")[0]
}

func upnpService(headers []http.Header) (service, error) {
	errs := []error{}
	for _, header := range headers {
		dd, err := deviceDescription(header)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if dd.BaseUrl == "" {
			locationN := strings.SplitN(header.Get("LOCATION"), "/", 4)
			if len(locationN) < 3 {
				errs = append(errs, fmt.Errorf("invalid location: %s\n", header.Get("LOCATION")))
				continue
			}
			dd.BaseUrl = strings.Join(locationN[:3], "/")
		}
		s, err := getConnectionService(dd.BaseUrl, dd.Device)
		if err == nil {
			return s, nil
		}
		errs = append(errs, err)
	}
	return service{}, errors.Join(errs...)
}

func discover(localIP string) ([]http.Header, error) {
	header := http.Header{}
	header["HOST"] = []string{"239.255.255.250:1900"}
	header["ST"] = []string{"ssdp:all"}
	header["MAN"] = []string{"\"ssdp:discover\""}
	header["MX"] = []string{"5"}
	req := http.Request{
		Method: "M-SEARCH",
		Header: header,
	}
	headers := []http.Header{}
	devices, err := udpRequest(localIP, "239.255.255.250", 1900, httpRequest(req))
	if err != nil {
		return nil, err
	}
	errs := []error{}
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
		headers = append(headers, header)
	}
	if len(headers) == 0 {
		return nil, errors.Join(errs...)
	}
	return headers, nil
}

func deviceDescription(header http.Header) (root, error) {
	r := root{}
	res, err := http.Get(header.Get("location"))
	if err != nil {
		return r, err
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return r, err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return r, fmt.Errorf("%s: %s", res.Status, body)
	}
	xml.Unmarshal(body, &r)
	return r, nil
}

func isValidService(s service) error {
	url := joinPath(s.Location, s.SCPDURL)
	res, err := http.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s responed with status=%d body=[%s]", url, res.StatusCode, body)
	}
	desc := description{}
	err = xml.Unmarshal(body, &desc)
	if err != nil {
		return err
	}

	for _, a := range desc.ActionList {
		if slices.Contains([]string{addPortMapping, deletePortMapping, getExternalIPAddress}, a.Name) {
			return nil
		}
	}

	return fmt.Errorf("%s service is not valid", url)
}

func getConnectionService(location string, rootDevice device) (service, error) {
	errs := []error{}
	for _, s := range rootDevice.ServiceList {
		s.Location = location
		err := isValidService(s)
		if err == nil {
			return s, nil
		}
		errs = append(errs, err)
	}
	for _, d := range rootDevice.DeviceList {
		s, err := getConnectionService(location, d)
		if err == nil {
			return s, nil
		}
		errs = append(errs, err)
	}
	return service{}, errors.Join(errs...)
}

func (c *Client) AddPortMapping(msg AddPortMappingRequest) (AddPortMappingResponse, error) {
	if msg.NewInternalClient == "" {
		msg.NewInternalClient = c.LocalIP
	}
	msg.XMLNS = c.service.ServiceType
	req, err := newEnvelopeReq(addPortMapping, c.service, body{AddPortMappingRequest: &msg})
	if err != nil {
		return AddPortMappingResponse{}, err
	}
	res, err := doRequest(req)
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
	req, err := newEnvelopeReq(deletePortMapping, c.service, body{DeletePortMappingRequest: &msg})
	if err != nil {
		return DeletePortMappingResponse{}, err
	}
	res, err := doRequest(req)
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
	req, err := newEnvelopeReq(getExternalIPAddress, c.service, body{GetExternalIPAddressRequest: &msg})
	if err != nil {
		return GetExternalIPAddressResponse{}, err
	}
	res, err := doRequest(req)
	if err != nil {
		return GetExternalIPAddressResponse{}, err
	}
	if res.Body.GetExternalIPAddressResponse == nil {
		return GetExternalIPAddressResponse{}, nil
	}
	return *res.Body.GetExternalIPAddressResponse, nil
}
