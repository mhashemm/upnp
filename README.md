# upnp

A zero-dependency Go library that opens and closes router port mappings over UPnP IGD.

```go
c, err := upnp.NewContext(ctx) // finds the gateway; takes about 6.5s
if err != nil {
	return err
}
_, err = c.AddPortMapping(upnp.AddPortMappingRequest{
	NewProtocol:               "TCP",
	NewInternalPort:           8080,
	NewExternalPort:           8080,
	NewPortMappingDescription: "my app",
	NewLeaseDuration:          3600, // renew it yourself if you need it longer
})
if err != nil {
	return err
}
defer c.DeletePortMapping(upnp.DeletePortMappingRequest{NewExternalPort: 8080, NewProtocol: "TCP"})

ip, err := c.GetExternalIPAddress()
if err != nil {
	return err
}
if upnp.IPClass(ip.NewExternalIPAddress) != upnp.IPPublic {
	// Behind carrier NAT the mapping succeeds and this address is still not reachable
	// from the internet.
}
```

Set `upnp.Logger` to an `*slog.Logger` before calling `NewContext` to see what discovery
found: Info has one line per outcome, Debug the whole trace.

**Anything on the LAN can answer SSDP.** The library bounds what a device can make it do,
but what a device says comes through as is: a log value or error can hold any text, and
a gateway can lie about its external address. Clean that text before you print it as
lines something else reads.

## Further reading

- [Port mapping protocols overview](https://github.com/Self-Hosting-Group/wiki/wiki/Port-Mapping-Protocols-Overview)
- [RFC 6887](https://datatracker.ietf.org/doc/html/rfc6887) (PCP)
