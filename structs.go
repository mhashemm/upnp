package upnp

import (
	"encoding/xml"
	"fmt"
	"net/netip"
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
	Device      string   `xml:"-"`
	ExternalIP  string   `xml:"-"`
}
type device struct {
	XMLName      xml.Name  `xml:"device"`
	DeviceType   string    `xml:"deviceType"`
	FriendlyName string    `xml:"friendlyName"`
	Manufacturer string    `xml:"manufacturer"`
	ModelName    string    `xml:"modelName"`
	ModelNumber  string    `xml:"modelNumber"`
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

type datagram struct {
	from netip.AddrPort
	data []byte
}

const (
	ErrNoSuchEntryInArray     = 714
	ErrConflictInMappingEntry = 718
	ErrOnlyPermanentLease     = 725
)

var errorNames = map[int]string{
	401: "Invalid Action",
	402: "Invalid Args",
	501: "Action Failed",
	600: "Argument Value Invalid",
	601: "Argument Value Out of Range",
	602: "Optional Action Not Implemented",
	606: "Action not authorized",
	713: "SpecifiedArrayIndexInvalid",
	714: "NoSuchEntryInArray",
	715: "WildCardNotPermittedInSrcIP",
	716: "WildCardNotPermittedInExtPort",
	718: "ConflictInMappingEntry",
	724: "SamePortValuesRequired",
	725: "OnlyPermanentLeasesSupported",
	726: "RemoteHostOnlySupportsWildcard",
	727: "ExternalPortOnlySupportsWildcard",
	728: "NoPortMapsAvailable",
	729: "ConflictWithOtherMechanisms",
	732: "WildCardNotPermittedInIntPort",
}

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
	NewExternalIPAddress string `xml:"NewExternalIPAddress,omitempty"`
}
