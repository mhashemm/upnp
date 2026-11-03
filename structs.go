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

type service struct {
	XMLName     xml.Name `xml:"service"`
	ServiceType string   `xml:"serviceType"`
	ControlURL  string   `xml:"controlURL"`
	SCPDURL     string   `xml:"SCPDURL"`
	Location    string   `xml:"-"`
	Actions     []string `xml:"-"`
}
type device struct {
	XMLName     xml.Name  `xml:"device"`
	ServiceList []service `xml:"serviceList>service"`
	DeviceList  []device  `xml:"deviceList>device"`
}

type root struct {
	XMLName xml.Name `xml:"root"`
	Device  device   `xml:"device"`
	BaseUrl string   `xml:"URLBase"`
}

type action struct {
	XMLName xml.Name `xml:"action"`
	Name    string   `xml:"name"`
}
type description struct {
	XMLName    xml.Name `xml:"scpd"`
	ActionList []action `xml:"actionList>action"`
}

type envelope struct {
	XMLName xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	Body    body     `xml:"http://schemas.xmlsoap.org/soap/envelope/ Body"`
}

type body struct {
	Fault *fault `xml:"Fault"`

	AddPortMappingResponse       *AddPortMappingResponse       `xml:"AddPortMappingResponse"`
	DeletePortMappingResponse    *DeletePortMappingResponse    `xml:"DeletePortMappingResponse"`
	GetExternalIPAddressResponse *GetExternalIPAddressResponse `xml:"GetExternalIPAddressResponse"`
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

type AddPortMappingResponse struct {
	LeaseDuration int `xml:"-"`
}

type DeletePortMappingResponse struct{}

type GetExternalIPAddressResponse struct {
	NewExternalIPAddress string `xml:"NewExternalIPAddress"`
}
