package upnp

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

func httpRequest(r http.Request) []byte {
	b := bytes.NewBuffer(nil)
	b.WriteString(r.Method)
	b.WriteString(" * HTTP/1.1\r\n")
	for k, v := range r.Header {
		b.WriteString(fmt.Sprintf("%s: %s\r\n", k, strings.Join(v, "; ")))
	}
	b.WriteString("\r\n")
	return b.Bytes()
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
func udpRequest(addr string, port int, payload []byte) ([]byte, error) {
	socket, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	defer socket.Close()
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	remote := &net.UDPAddr{
		IP:   ip.AsSlice(),
		Port: port,
	}
	_, err = socket.WriteToUDP(payload, remote)
	if err != nil {
		return nil, err
	}
	received := make([]byte, 4096)
	n, err := socket.Read(received)
	if err != nil {
		return nil, err
	}
	return received[:n], nil
}

func upnpService() (service, error) {
	header, err := discover()
	if err != nil {
		return service{}, err
	}
	dd, err := deviceDescription(header)
	if err != nil {
		return service{}, err
	}
	s, err := serviceDescription(dd)
	if err != nil {
		return service{}, err
	}
	locationN := strings.SplitN(header.Get("LOCATION"), "/", 4)
	if len(locationN) < 3 {
		return s, fmt.Errorf("invalid location: %s", header.Get("LOCATION"))
	}
	s.Location = strings.Join(locationN[:3], "/")
	return s, nil
}

func discover() (http.Header, error) {
	header := http.Header{}
	header["HOST"] = []string{"239.255.255.250:1900"}
	header["ST"] = []string{"ssdp:all"}
	header["MAN"] = []string{"\"ssdp:discover\""}
	header["MX"] = []string{"2"}
	req := http.Request{
		Method: "M-SEARCH",
		Header: header,
	}
	res, err := udpRequest("239.255.255.250", 1900, httpRequest(req))
	if err != nil {
		return nil, err
	}
	httpRes, headerRes, found := bytes.Cut(res, []byte{'\n'})
	if !found {
		return nil, fmt.Errorf("invalid response: %s", res)
	}
	s := bytes.Split(httpRes, []byte{' '})
	if len(s) < 3 {
		return nil, fmt.Errorf("invalid response: %s", headerRes)
	}
	statusCode := string(s[1])
	if statusCode != strconv.Itoa(http.StatusOK) {
		return nil, fmt.Errorf("not ok: %s", httpRes)
	}
	_header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(headerRes))).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	header = http.Header(_header)
	return header, nil
}

type specVersion struct {
	XMLName xml.Name `xml:"specVersion"`
	Major   int      `xml:"major"`
	Minor   int      `xml:"minor"`
}
type service struct {
	XMLName     xml.Name `xml:"service"`
	ServiceType string   `xml:"serviceType"`
	ServiceId   string   `xml:"serviceId"`
	ControlURL  string   `xml:"controlURL"`
	EventSubURL string   `xml:"eventSubURL"`
	SCPDURL     string   `xml:"SCPDURL"`
	Location    string   `xml:"-"`
}
type device struct {
	XMLName      xml.Name  `xml:"device"`
	DeviceType   string    `xml:"deviceType"`
	SerialNumber string    `xml:"serialNumber"`
	UDN          string    `xml:"UDN"`
	ServiceList  []service `xml:"serviceList>service"`
	DeviceList   []device  `xml:"deviceList>device"`
}

type root struct {
	XMLName     xml.Name    `xml:"root"`
	XMLNS       string      `xml:"xmlns,attr"`
	SpecVersion specVersion `xml:"specVersion"`
	Device      device      `xml:"device"`
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
func getConnectionDevice(rootDevice device) (device, bool) {
	if len(rootDevice.DeviceList) == 0 {
		return rootDevice, false
	}
	for _, d := range rootDevice.DeviceList {
		if strings.Contains(d.DeviceType, "ConnectionDevice") {
			return d, true
		}
		d, found := getConnectionDevice(d)
		if found {
			return d, true
		}
	}
	return device{}, false
}
func serviceDescription(r root) (service, error) {
	d, found := getConnectionDevice(r.Device)
	if !found {
		return service{}, fmt.Errorf("no connection device found: %v", r)
	}
	if len(d.ServiceList) == 0 {
		return service{}, fmt.Errorf("no services found: %v", d)
	}
	return d.ServiceList[0], nil
}

type envelope struct {
	XMLNS         string   `xml:"xmlns:s,attr"`
	XMLName       xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	EncodingStyle string   `xml:"http://schemas.xmlsoap.org/soap/envelope/ encodingStyle,attr"`
	Body          body     `xml:"http://schemas.xmlsoap.org/soap/envelope/ Body"`
}
type body struct {
	AddPortMappingRequest       *AddPortMappingRequest       `xml:"u:AddPortMapping,omitempty"`
	DeletePortMappingRequest    *DeletePortMappingRequest    `xml:"u:DeletePortMapping,omitempty"`
	GetExternalIPAddressRequest *GetExternalIPAddressRequest `xml:"u:GetExternalIPAddress,omitempty"`

	AddPortMappingResponse       *AddPortMappingResponse       `xml:"AddPortMappingResponse,omitempty"`
	DeletePortMappingResponse    *DeletePortMappingResponse    `xml:"DeletePortMappingResponse,omitempty"`
	GetExternalIPAddressResponse *GetExternalIPAddressResponse `xml:"GetExternalIPAddressResponse,omitempty"`
}

type AddPortMappingRequest struct {
	XMLNS                     string   `xml:"xmlns:u,attr"`
	NewRemoteHost             struct{} `xml:"NewRemoteHost"`
	NewExternalPort           int      `xml:"NewExternalPort"`
	NewProtocol               string   `xml:"NewProtocol"`
	NewInternalPort           int      `xml:"NewInternalPort"`
	NewInternalClient         string   `xml:"NewInternalClient"`
	NewEnabled                int      `xml:"NewEnabled"`
	NewPortMappingDescription string   `xml:"NewPortMappingDescription"`
	NewLeaseDuration          int      `xml:"NewLeaseDuration"`
}

type DeletePortMappingRequest struct {
	XMLNS           string   `xml:"xmlns:u,attr"`
	NewRemoteHost   struct{} `xml:"NewRemoteHost"`
	NewExternalPort int      `xml:"NewExternalPort"`
	NewProtocol     string   `xml:"NewProtocol"`
}

type GetExternalIPAddressRequest struct {
	XMLNS string `xml:"xmlns:u,attr"`
}

func newEnvelopeReq(action string, s service, b body) (*http.Request, error) {
	e := envelope{
		EncodingStyle: "http://schemas.xmlsoap.org/soap/encoding/",
		XMLNS:         "http://schemas.xmlsoap.org/soap/envelope/",
		Body:          b,
	}
	body, err := xml.Marshal(e)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(nil)
	buf.Grow(len(xml.Header) + len(body))
	buf.Write([]byte(xml.Header))
	buf.Write(body)

	req, err := http.NewRequest(http.MethodPost, s.Location+s.ControlURL, buf)

	if err != nil {
		return nil, err
	}

	req.Header["SOAPAction"] = []string{fmt.Sprintf("\"%s#%s\"", s.ServiceType, action)}
	req.Header["Content-Type"] = []string{"text/xml"}
	req.Header["Connection"] = []string{"Close"}
	req.Header["Cache-Control"] = []string{"no-cache"}
	req.Header["Pragma"] = []string{"no-cache"}
	return req, nil
}

func doRequest(req *http.Request) (envelope, error) {
	client := http.DefaultClient
	res, err := client.Do(req)
	if err != nil {
		return envelope{}, err
	}
	defer res.Body.Close()
	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return envelope{}, err
	}
	if res.StatusCode != http.StatusOK {
		return envelope{}, fmt.Errorf("not ok: %s: %s", res.Status, resBody)
	}
	envelopeRes := envelope{}
	err = xml.Unmarshal(resBody, &envelopeRes)
	if err != nil {
		return envelope{}, err
	}
	return envelopeRes, err
}

type AddPortMappingResponse struct{}

func AddPortMapping(msg AddPortMappingRequest) (AddPortMappingResponse, error) {
	if msg.NewInternalClient == "" {
		msg.NewInternalClient = GetLocalIPAddr()
	}
	s, err := upnpService()
	if err != nil {
		return AddPortMappingResponse{}, err
	}
	msg.XMLNS = s.ServiceType
	req, err := newEnvelopeReq("AddPortMapping", s, body{AddPortMappingRequest: &msg})
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

type DeletePortMappingResponse struct{}

func DeletePortMapping(msg DeletePortMappingRequest) (DeletePortMappingResponse, error) {
	s, err := upnpService()
	if err != nil {
		return DeletePortMappingResponse{}, err
	}
	msg.XMLNS = s.ServiceType
	req, err := newEnvelopeReq("DeletePortMapping", s, body{DeletePortMappingRequest: &msg})
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

type GetExternalIPAddressResponse struct {
	NewExternalIPAddress string `xml:"NewExternalIPAddress,omitempty"`
}

func GetExternalIPAddress() (GetExternalIPAddressResponse, error) {
	s, err := upnpService()
	if err != nil {
		return GetExternalIPAddressResponse{}, err
	}
	msg := GetExternalIPAddressRequest{
		XMLNS: s.ServiceType,
	}
	req, err := newEnvelopeReq("GetExternalIPAddress", s, body{GetExternalIPAddressRequest: &msg})
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
