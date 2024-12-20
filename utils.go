package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

func joinPath(parts ...string) string {
	for i := range parts {
		parts[i] = strings.Trim(parts[i], "/")
	}
	return strings.Join(parts, "/")
}

func httpRequest(r http.Request) []byte {
	r.Header["USER-AGENT"] = []string{"Golang/1.23.3 UPnP/1.1 Upnp/1.0"}
	b := bytes.NewBuffer(nil)
	b.WriteString(r.Method)
	b.WriteString(" * HTTP/1.1\r\n")
	for k, v := range r.Header {
		b.WriteString(fmt.Sprintf("%s: %s\r\n", k, strings.Join(v, "; ")))
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

func udpRequest(addr string, port int, payload []byte) ([][]byte, error) {
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
	res := [][]byte{}
	var resErr error
	for {
		received := make([]byte, 4096)
		err := socket.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err != nil {
			resErr = err
			break
		}
		n, err := socket.Read(received)
		if err != nil {
			resErr = err
			break
		}
		res = append(res, received[:n])
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("%s:%d no content, with error: %s", addr, port, resErr)
	}
	return res, nil
}

func newEnvelopeReq(action string, s service, b body) (*http.Request, error) {
	url := joinPath(s.Location, s.ControlURL)
	e := envelope{
		EncodingStyle: "http://schemas.xmlsoap.org/soap/encoding/",
		XMLNS:         "http://schemas.xmlsoap.org/soap/envelope/",
		Body:          b,
	}
	body, err := xml.Marshal(e)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(xml.Header)+len(body)))
	buf.WriteString(xml.Header)
	buf.Write(body)
	req, err := http.NewRequest(http.MethodPost, url, buf)

	if err != nil {
		return nil, err
	}

	req.Header["SOAPAction"] = []string{fmt.Sprintf("\"%s#%s\"", s.ServiceType, action)}
	req.Header["Content-Type"] = []string{"text/xml; charset=utf-8"}
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
