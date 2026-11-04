package upnp

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var Logger *slog.Logger

func debug(msg string, args ...any) {
	if Logger != nil {
		Logger.Debug(msg, args...)
	}
}

func info(msg string, args ...any) {
	if Logger != nil {
		Logger.Info(msg, args...)
	}
}

var httpClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req.URL.Hostname() != via[0].URL.Hostname() {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

const (
	ssdpMX           = 5 * time.Second
	ssdpRetries      = 2
	ssdpRetryGap     = 500 * time.Millisecond
	ssdpMargin       = 500 * time.Millisecond
	ssdpWait         = ssdpRetryGap*ssdpRetries + ssdpMX + ssdpMargin
	discoveryTimeout = ssdpWait + 30*time.Second
	maxReplies       = 4096
	maxBody          = 1 << 20
	maxRedirects     = 3
)

var ssdpTargets = []string{
	"ssdp:all",
	"upnp:rootdevice",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

func mSearch(st string) []byte {
	return []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: " + strconv.Itoa(int(ssdpMX/time.Second)) + "\r\n" +
		"ST: " + st + "\r\n" +
		"USER-AGENT: Golang/" + strings.TrimPrefix(runtime.Version(), "go") + " UPnP/1.1 Upnp/1.0\r\n" +
		"\r\n")
}

func decodeXML(b []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = latin1
	return d.Decode(v)
}

func latin1(_ string, r io.Reader) (io.Reader, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	u := make([]rune, len(b))
	for i, c := range b {
		u[i] = rune(c)
	}
	return strings.NewReader(string(u)), nil
}

func snippet(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) <= 512 {
		return string(b)
	}
	return fmt.Sprintf("%s... (%d more bytes)", b[:512], len(b)-512)
}

func readBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, maxBody))
}

func get(c context.Context, url string) ([]byte, *http.Response, error) {
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := readBody(res.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET %s: %s: %s", url, res.Status, snippet(body))
	}
	return body, res, nil
}

func sameHost(base, ref string) bool {
	resolved, err := resolveURL(base, ref)
	if err != nil {
		return false
	}
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	r, err := url.Parse(resolved)
	return err == nil && b.Hostname() == r.Hostname()
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

func udpRequest(c context.Context, localIP, remoteIP string, port int, payloads [][]byte, retryGap, wait time.Duration) ([]datagram, error) {
	c, cancel := context.WithCancel(c)
	defer cancel()
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
	for _, p := range payloads {
		if _, err := socket.WriteToUDP(p, remote); err != nil {
			return nil, err
		}
	}
	go func() {
		for range ssdpRetries {
			select {
			case <-time.After(retryGap):
			case <-c.Done():
				return
			}
			for _, p := range payloads {
				socket.WriteToUDP(p, remote)
			}
		}
	}()
	err = socket.SetReadDeadline(time.Now().Add(wait))
	if err != nil {
		return nil, err
	}
	defer context.AfterFunc(c, func() { socket.SetReadDeadline(time.Now()) })()
	res := []datagram{}
	received := make([]byte, 4096)
	var resErr error
	for len(res) < maxReplies {
		n, from, err := socket.ReadFromUDPAddrPort(received)
		if err != nil {
			resErr = err
			break
		}
		res = append(res, datagram{from: from, data: bytes.Clone(received[:n])})
	}
	if len(res) == 0 {
		if err := c.Err(); err != nil {
			resErr = err
		}
		return nil, fmt.Errorf("%s:%d: %w", remoteIP, port, resErr)
	}
	return res, nil
}

const (
	envelopeOpen  = xml.Header + `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`
	envelopeClose = `</s:Body></s:Envelope>`
)

func newEnvelopeReq(c context.Context, action string, s service, msg any) (*http.Request, error) {
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
	soapAction := fmt.Sprintf("\"%s#%s\"", s.ServiceType, action)
	debug("soap request", "action", action, "url", url, "soapaction", soapAction, "body", buf.String())
	req, err := http.NewRequestWithContext(c, http.MethodPost, url, buf)
	if err != nil {
		return nil, err
	}

	req.Header["SOAPAction"] = []string{soapAction}
	req.Header["Content-Type"] = []string{`text/xml; charset="utf-8"`}
	return req, nil
}

func doRequest(action string, req *http.Request) (envelope, error) {
	res, err := httpClient.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("%s: %w", action, err)
	}
	defer res.Body.Close()
	resBody, err := readBody(res.Body)
	if err != nil {
		return envelope{}, fmt.Errorf("%s: reading the %s response: %w", action, res.Status, err)
	}
	debug("soap response", "action", action, "url", req.URL.String(), "status", res.Status,
		"server", res.Header.Get("Server"), "body", string(resBody))
	envelopeRes := envelope{}
	decodeErr := decodeXML(resBody, &envelopeRes)
	if f := envelopeRes.Body.Fault; f != nil {
		desc := f.Description
		if desc == "" {
			desc = errorNames[f.Code]
		}
		return envelope{}, &Error{Action: action, Code: f.Code, Description: desc}
	}
	if res.StatusCode != http.StatusOK {
		return envelope{}, fmt.Errorf("%s: %s: %s", action, res.Status, snippet(resBody))
	}
	if decodeErr != nil {
		return envelope{}, fmt.Errorf("%s: undecodable %s response: %w: %s", action, res.Status, decodeErr, snippet(resBody))
	}
	return envelopeRes, nil
}
