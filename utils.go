package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

const (
	ssdpMX       = 5 * time.Second
	ssdpRetries  = 2
	ssdpRetryGap = 500 * time.Millisecond
	ssdpMargin   = 500 * time.Millisecond
)

func decodeXML(b []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.AutoClose = xml.HTMLAutoClose
	return d.Decode(v)
}

func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid base url %q: %w", base, err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", ref, err)
	}
	return b.ResolveReference(r).String(), nil
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

func udpRequest(localIP, remoteIP string, port int, payload []byte) ([][]byte, error) {
	local, err := netip.ParseAddr(localIP)
	if err != nil {
		return nil, err
	}
	socket, err := net.ListenUDP("udp", &net.UDPAddr{
		IP: local.AsSlice(),
	})
	if err != nil {
		return nil, err
	}
	defer socket.Close()

	ip, err := netip.ParseAddr(remoteIP)
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
	go func() {
		for range ssdpRetries {
			time.Sleep(ssdpRetryGap)
			socket.WriteToUDP(payload, remote)
		}
	}()
	deadline := time.Now().Add(ssdpRetryGap*ssdpRetries + ssdpMX + ssdpMargin)
	res := [][]byte{}
	var resErr error
	for {
		received := make([]byte, 4096)
		err := socket.SetReadDeadline(deadline)
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
		return nil, fmt.Errorf("%s:%d no content, with error: %s", remoteIP, port, resErr)
	}
	return res, nil
}

const (
	envelopeOpen  = xml.Header + `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`
	envelopeClose = `</s:Body></s:Envelope>`
)

func newEnvelopeReq(action string, s service, msg any) (*http.Request, error) {
	url, err := resolveURL(s.Location, s.ControlURL)
	if err != nil {
		return nil, err
	}
	body, err := xml.Marshal(msg)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(envelopeOpen)+len(body)+len(envelopeClose)))
	buf.WriteString(envelopeOpen)
	buf.Write(body)
	buf.WriteString(envelopeClose)
	req, err := http.NewRequest(http.MethodPost, url, buf)
	if err != nil {
		return nil, err
	}

	req.Header["SOAPAction"] = []string{fmt.Sprintf("\"%s#%s\"", s.ServiceType, action)}
	req.Header["Content-Type"] = []string{"text/xml; charset=utf-8"}
	return req, nil
}

func doRequest(action string, req *http.Request) (envelope, error) {
	res, err := httpClient.Do(req)
	if err != nil {
		return envelope{}, err
	}
	defer res.Body.Close()
	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return envelope{}, err
	}
	envelopeRes := envelope{}
	unmarshalErr := xml.Unmarshal(resBody, &envelopeRes)
	if res.StatusCode != http.StatusOK {
		if unmarshalErr == nil && envelopeRes.Body.Fault != nil {
			f := envelopeRes.Body.Fault
			return envelope{}, &Error{Action: action, Code: f.Code, Description: f.Description}
		}
		return envelope{}, fmt.Errorf("%s: not ok: %s: %s", action, res.Status, resBody)
	}
	if unmarshalErr != nil {
		return envelope{}, unmarshalErr
	}
	return envelopeRes, nil
}
