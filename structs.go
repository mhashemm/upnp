package upnp

import (
	"encoding/xml"
	"fmt"
	"strings"
)

const (
	addPortMapping       = "AddPortMapping"
	deletePortMapping    = "DeletePortMapping"
	getExternalIPAddress = "GetExternalIPAddress"
)

var supported = []string{addPortMapping, deletePortMapping, getExternalIPAddress}

type Client struct {
	LocalIP string
	service service
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
	Actions     []string `xml:"-"`
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
	BaseUrl     string      `xml:"URLBase"`
}

type argument struct {
	XMLName              xml.Name `xml:"argument"`
	Name                 string   `xml:"name"`
	Direction            string   `xml:"direction"`
	RelatedStateVariable string   `xml:"relatedStateVariable"`
}
type action struct {
	XMLName      xml.Name   `xml:"action"`
	Name         string     `xml:"name"`
	ArgumentList []argument `xml:"argumentList>argument"`
}
type description struct {
	XMLName    xml.Name `xml:"scpd"`
	ActionList []action `xml:"actionList>action"`
}

type envelope struct {
	XMLNS         string   `xml:"xmlns:s,attr"`
	XMLName       xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	EncodingStyle string   `xml:"http://schemas.xmlsoap.org/soap/envelope/ encodingStyle,attr"`
	Body          body     `xml:"http://schemas.xmlsoap.org/soap/envelope/ Body"`
}

type body struct {
	Fault *fault `xml:"Fault,omitempty"`

	AddPortMappingResponse       *AddPortMappingResponse       `xml:"AddPortMappingResponse,omitempty"`
	DeletePortMappingResponse    *DeletePortMappingResponse    `xml:"DeletePortMappingResponse,omitempty"`
	GetExternalIPAddressResponse *GetExternalIPAddressResponse `xml:"GetExternalIPAddressResponse,omitempty"`
}

type fault struct {
	Code        int    `xml:"detail>UPnPError>errorCode"`
	Description string `xml:"detail>UPnPError>errorDescription"`
}

const (
	ErrNoSuchEntryInArray     = 714
	ErrConflictInMappingEntry = 718
	ErrOnlyPermanentLease     = 725
)

type Error struct {
	Action      string
	Code        int
	Description string
}

func (e *Error) Error() string {
	return strings.TrimRight(fmt.Sprintf("%s: upnp error %d: %s", e.Action, e.Code, e.Description), ": ")
}

type AddPortMappingRequest struct {
	XMLName                   xml.Name `xml:"u:AddPortMapping"`
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
	XMLName         xml.Name `xml:"u:DeletePortMapping"`
	XMLNS           string   `xml:"xmlns:u,attr"`
	NewRemoteHost   struct{} `xml:"NewRemoteHost"`
	NewExternalPort int      `xml:"NewExternalPort"`
	NewProtocol     string   `xml:"NewProtocol"`
}

type GetExternalIPAddressRequest struct {
	XMLName xml.Name `xml:"u:GetExternalIPAddress"`
	XMLNS   string   `xml:"xmlns:u,attr"`
}

type AddPortMappingResponse struct{}

type DeletePortMappingResponse struct{}

type GetExternalIPAddressResponse struct {
	NewExternalIPAddress string `xml:"NewExternalIPAddress,omitempty"`
}
