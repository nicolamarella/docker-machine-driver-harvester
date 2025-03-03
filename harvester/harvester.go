package harvester

import (
	"fmt"
	"net"
	"strings"
	"time"

	goharv "github.com/harvester/go-harvester/pkg/client"
	goharv1 "github.com/harvester/go-harvester/pkg/client/generated/v1"
	goharverrors "github.com/harvester/go-harvester/pkg/errors"
	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/log"
	"github.com/rancher/machine/libmachine/mcnutils"
	"github.com/rancher/machine/libmachine/state"
)

const driverName = "harvester"

// Driver is the driver used when no driver is selected. It is used to
// connect to existing Docker hosts by specifying the URL of the host as
// an option.
type Driver struct {
	*drivers.BaseDriver

	client *goharv.Client

	Host        string
	Port        int
	Username    string
	Password    string
	Namespace   string
	ClusterType string

	ServerVersion string

	CPU        int
	MemorySize string
	DiskSize   string
	DiskBus    string

	ImageName string

	KeyPairName       string
	SSHPrivateKeyPath string
	SSHPublicKey      string
	SSHPassword       string

	AddUserToDockerGroup bool

	NetworkType string

	NetworkName  string
	NetworkModel string
}

func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return driverName
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetURL() (string, error) {
	if err := drivers.MustBeRunning(d); err != nil {
		return "", err
	}

	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, "2376")), nil
}

func (d *Driver) GetIP() (string, error) {
	if err := drivers.MustBeRunning(d); err != nil {
		return "", err
	}

	vmi, err := d.getVMI()
	if err != nil {
		return "", err
	}

	return strings.Split(vmi.Status.Interfaces[0].IP, "/")[0], nil
}

func (d *Driver) GetState() (state.State, error) {
	c, err := d.getClient()
	if err != nil {
		return state.None, err
	}

	_, err = c.VirtualMachines.Get(d.Namespace, d.MachineName)
	if err != nil {
		return state.None, err
	}

	vmi, err := c.VirtualMachineInstances.Get(d.Namespace, d.MachineName)
	if err != nil {
		if goharverrors.IsNotFound(err) {
			return state.Stopped, nil
		}
		return state.None, err
	}
	return getStateFormVMI(vmi), nil
}

func getStateFormVMI(vmi *goharv1.VirtualMachineInstance) state.State {
	switch vmi.Status.Phase {
	case "Pending", "Scheduling", "Scheduled":
		return state.Starting
	case "Running":
		return state.Running
	case "Succeeded":
		return state.Stopping
	case "Failed":
		return state.Error
	default:
		return state.None
	}
}

func (d *Driver) waitRemoved() error {
	removed := func() bool {
		if _, err := d.getVM(); err != nil {
			if goharverrors.IsNotFound(err) {
				return true
			}
		}
		return false
	}
	log.Debugf("Waiting for node removed")
	if err := mcnutils.WaitForSpecific(removed, 120, 5*time.Second); err != nil {
		return fmt.Errorf("Too many retries waiting for machine removed.  Last error: %s", err)
	}
	return nil
}

func (d *Driver) Remove() error {
	c, err := d.getClient()
	if err != nil {
		return err
	}

	vm, err := c.VirtualMachines.Get(d.Namespace, d.MachineName)
	if err != nil {
		if goharverrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	removedDisks := make([]string, 0, len(vm.Spec.Template.Spec.Volumes))
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.DataVolume != nil {
			removedDisks = append(removedDisks, volume.Name)
		}
	}
	log.Debugf("Remove node")
	_, err = c.VirtualMachines.Delete(d.Namespace, d.MachineName, map[string]string{
		"removedDisks":      strings.Join(removedDisks, ","),
		"propagationPolicy": "Foreground",
	})
	if err != nil {
		return err
	}

	return d.waitRemoved()
}

func (d *Driver) Restart() error {
	c, err := d.getClient()
	if err != nil {
		return err
	}
	vmi, err := c.VirtualMachineInstances.Get(d.Namespace, d.MachineName)
	if err != nil {
		return err
	}
	oldUID := string(vmi.UID)

	log.Debugf("Restart node")
	err = c.VirtualMachines.Restart(d.Namespace, d.MachineName)
	if err != nil {
		return err
	}

	return d.waitForRestart(oldUID)
}

func (d *Driver) Start() error {
	c, err := d.getClient()
	if err != nil {
		return err
	}
	log.Debugf("Start node")
	if err = c.VirtualMachines.Start(d.Namespace, d.MachineName); err != nil {
		return err
	}
	return d.waitForReady()
}

func (d *Driver) Stop() error {
	c, err := d.getClient()
	if err != nil {
		return err
	}
	log.Debugf("Stop node")
	if err = c.VirtualMachines.Stop(d.Namespace, d.MachineName); err != nil {
		return err
	}
	return d.waitForState(state.Stopped)
}

func (d *Driver) Kill() error {
	return d.Stop()
}

func (d *Driver) getVMI() (*goharv1.VirtualMachineInstance, error) {
	c, err := d.getClient()
	if err != nil {
		return nil, err
	}
	return c.VirtualMachineInstances.Get(d.Namespace, d.MachineName)
}

func (d *Driver) getVM() (*goharv1.VirtualMachine, error) {
	c, err := d.getClient()
	if err != nil {
		return nil, err
	}
	return c.VirtualMachines.Get(d.Namespace, d.MachineName)
}
