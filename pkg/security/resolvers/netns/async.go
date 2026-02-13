package netns

import (
	"errors"
	"fmt"
	"github.com/DataDog/datadog-agent/pkg/security/secl/model"
	"github.com/DataDog/datadog-agent/pkg/security/seclog"
	manager "github.com/DataDog/ebpf-manager"
	"github.com/vishvananda/netlink"
	"os"
)

// QueuedNetworkDeviceError is used to indicate that the new network Device was queued until its namespace handle is
// resolved.
type QueuedNetworkDeviceError struct {
	msg string
}

func (err QueuedNetworkDeviceError) Error() string {
	return err.msg
}

type tcClassifierRequestType int

const (
	TcNewDeviceRequestType tcClassifierRequestType = iota
	TcDeviceUpdateRequestType
)

type TcClassifierRequest struct {
	RequestType tcClassifierRequestType
	Device      model.NetDevice
}

func (tcr *Resolver) PushNewTCClassifierRequest(request TcClassifierRequest) {
	select {
	case <-tcr.ctx.Done():
		// the probe is stopping, do not push the new tc classifier request
		return
	case tcr.tcRequests <- request:
		// do nothing
	default:
		seclog.Errorf("failed to slot new tc classifier request: %+v", request)
	}
}

func (tcr *Resolver) startSetupNewTCClassifierLoop() {
	for {
		select {
		case <-tcr.ctx.Done():
			return
		case request, ok := <-tcr.tcRequests:
			if !ok {
				return
			}

			if err := tcr.setupNewTCClassifier(request.Device); err != nil {
				var qnde QueuedNetworkDeviceError
				var linkNotFound netlink.LinkNotFoundError

				if errors.As(err, &qnde) {
					seclog.Debugf("%v", err)
				} else if errors.As(err, &linkNotFound) {
					seclog.Debugf("link not found while setting up new tc classifier: %v", err)
				} else if errors.Is(err, manager.ErrIdentificationPairInUse) {
					if request.RequestType != TcDeviceUpdateRequestType {
						seclog.Errorf("tc classifier already exists: %v", err)
					} else {
						seclog.Debugf("tc classifier already exists: %v", err)
					}
				} else {
					seclog.Errorf("error setting up new tc classifier on %+v: %v", request.Device, err)
				}
			}
		}
	}
}

func (tcr *Resolver) setupNewTCClassifier(device model.NetDevice) error {
	// select netns handle
	var handle *os.File
	var err error
	netns := tcr.ResolveNetworkNamespace(device.NetNS)
	if netns != nil {
		handle, err = netns.GetNamespaceHandleDup()
	}
	defer func() {
		if handle == nil {
			return
		}
		handle.Close()
	}()

	if netns == nil || err != nil || handle == nil {
		// queue network Device so that a TC classifier can be added later
		tcr.QueueNetworkDevice(device)
		return QueuedNetworkDeviceError{msg: fmt.Sprintf("Device %s is queued until %d is resolved", device.Name, device.NetNS)}
	}
	err = tcr.tcResolver.SetupNewTCClassifierWithNetNSHandle(device, handle, tcr.manager)
	if err != nil {
		return err
	}
	if err := handle.Close(); err != nil {
		e := fmt.Errorf("could not close file [%s]: %w", handle.Name(), err)
		handle = nil
		return e
	}
	handle = nil
	return nil
}

func (tcr *Resolver) startTcClassifierLoopGoroutine() {
	// start new tc classifier loop
	tcr.wg.Add(1)
	go func() {
		defer tcr.wg.Done()
		tcr.startSetupNewTCClassifierLoop()
	}()
}
