package main

import (
	"errors"
	"fmt"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

// Must match the enums in ../gluon-sim/gluon_sim.c.
const (
	gluonFamilyName = "gluon_sim"

	gluonCmdGetDeviceID = 1

	gluonAttrDeviceID = 1
)

// fetchDeviceID asks the gluon_sim kernel module for this VM's device ID over
// generic netlink. This is the whole point of the exercise: the same transport
// the real Atom uses to reach kwark.
func fetchDeviceID() (uint64, error) {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return 0, fmt.Errorf("dial generic netlink: %w", err)
	}
	defer c.Close()

	// Resolving by name is how userspace discovers a dynamically assigned
	// family ID — the same handshake `genl ctrl list` performs.
	family, err := c.GetFamily(gluonFamilyName)
	if err != nil {
		// The kernel answers ENOENT when no module has registered the family,
		// which is by far the most common cause — call it out plainly.
		if errors.Is(err, errENOENT) {
			return 0, fmt.Errorf("kernel module %q not loaded (try: insmod /opt/ne/gluon_sim.ko)", gluonFamilyName)
		}
		return 0, fmt.Errorf("resolve family %q: %w (is gluon_sim.ko loaded?)", gluonFamilyName, err)
	}

	msgs, err := c.Execute(
		genetlink.Message{
			Header: genetlink.Header{
				Command: gluonCmdGetDeviceID,
				Version: family.Version,
			},
		},
		family.ID,
		netlink.Request,
	)
	if err != nil {
		return 0, fmt.Errorf("GET_DEVICE_ID: %w", err)
	}
	if len(msgs) == 0 {
		return 0, errors.New("GET_DEVICE_ID: empty reply from kernel")
	}

	ad, err := netlink.NewAttributeDecoder(msgs[0].Data)
	if err != nil {
		return 0, fmt.Errorf("decode reply: %w", err)
	}
	var id uint64
	var found bool
	for ad.Next() {
		if ad.Type() == gluonAttrDeviceID {
			id = ad.Uint64()
			found = true
		}
	}
	if err := ad.Err(); err != nil {
		return 0, fmt.Errorf("decode reply: %w", err)
	}
	if !found {
		return 0, errors.New("kernel reply carried no device ID attribute")
	}
	return id, nil
}
