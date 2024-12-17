package main

import (
	"fmt"

	"github.com/mhashemm/upnp"
)

func main() {
	// fmt.Println(upnp.AddPortMapping(upnp.AddPortMappingRequest{
	// 	NewProtocol:               "TCP",
	// 	NewRemoteHost:             struct{}{},
	// 	NewExternalPort:           5000,
	// 	NewInternalPort:           5000,
	// 	NewEnabled:                1,
	// 	NewPortMappingDescription: "testing",
	// 	NewLeaseDuration:          1440,
	// }))
	ip, err := upnp.GetExternalIPAddress()
	fmt.Println(err)

	fmt.Println(ip.NewExternalIPAddress)
}
