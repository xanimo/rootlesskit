package slirp4netns

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/rootless-containers/rootlesskit/pkg/common"
	"github.com/rootless-containers/rootlesskit/pkg/network"
	"github.com/rootless-containers/rootlesskit/pkg/network/iputils"
	"github.com/rootless-containers/rootlesskit/pkg/network/parentutils"
)

type Features struct {
	// SupportsCIDR --cidr (v0.3.0)
	SupportsCIDR bool
	// SupportsDisableHostLoopback --disable-host-loopback (v0.3.0)
	SupportsDisableHostLoopback bool
	// SupportsAPISocket --api-socket (v0.3.0)
	SupportsAPISocket bool
	// SupportsCreateSandbox --create-sandbox (v0.4.0)
	SupportsCreateSandbox bool
}

func DetectFeatures(binary string) (*Features, error) {
	if binary == "" {
		return nil, errors.New("got empty slirp4netns binary")
	}
	realBinary, err := exec.LookPath(binary)
	if err != nil {
		return nil, errors.Wrapf(err, "slirp4netns binary %q is not installed", binary)
	}
	cmd := exec.Command(realBinary, "--help")
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	s := string(b)
	if err != nil {
		return nil, errors.Wrapf(err,
			"command \"%s --help\" failed, make sure slirp4netns v0.2.0+ is installed: %q",
			realBinary, s)
	}
	f := Features{
		SupportsCIDR:                strings.Contains(s, "--cidr"),
		SupportsDisableHostLoopback: strings.Contains(s, "--disable-host-loopback"),
		SupportsAPISocket:           strings.Contains(s, "--api-socket"),
		SupportsCreateSandbox:       strings.Contains(s, "--create-sandbox"),
	}
	return &f, nil
}

// NewParentDriver instantiates new parent driver.
// ipnet is supported only for slirp4netns v0.3.0+.
// ipnet MUST be nil for slirp4netns < v0.3.0.
//
// disableHostLoopback is supported only for slirp4netns v0.3.0+
// apiSocketPath is supported only for slirp4netns v0.3.0+
// createSandbox is supported only for slirp4netns v0.4.0+
func NewParentDriver(binary string, mtu int, ipnet *net.IPNet, disableHostLoopback bool, apiSocketPath string, createSandbox bool) network.ParentDriver {
	if binary == "" {
		panic("got empty slirp4netns binary")
	}
	if mtu < 0 {
		panic("got negative mtu")
	}
	if mtu == 0 {
		mtu = 65520
	}
	return &parentDriver{
		binary:              binary,
		mtu:                 mtu,
		ipnet:               ipnet,
		disableHostLoopback: disableHostLoopback,
		apiSocketPath:       apiSocketPath,
		createSandbox:       createSandbox,
	}
}

type parentDriver struct {
	binary              string
	mtu                 int
	ipnet               *net.IPNet
	disableHostLoopback bool
	apiSocketPath       string
	createSandbox       bool
}

func (d *parentDriver) MTU() int {
	return d.mtu
}

func (d *parentDriver) ConfigureNetwork(childPID int, stateDir string) (*common.NetworkMessage, func() error, error) {
	tap := "tap0"
	var cleanups []func() error
	if err := parentutils.PrepareTap(childPID, tap); err != nil {
		return nil, common.Seq(cleanups), errors.Wrapf(err, "setting up tap %s", tap)
	}
	ctx, cancel := context.WithCancel(context.Background())
	opts := []string{"--mtu", strconv.Itoa(d.mtu)}
	if d.disableHostLoopback {
		opts = append(opts, "--disable-host-loopback")
	}
	if d.ipnet != nil {
		opts = append(opts, "--cidr", d.ipnet.String())
	}
	if d.apiSocketPath != "" {
		opts = append(opts, "--api-socket", d.apiSocketPath)
	}
	if d.createSandbox {
		opts = append(opts, "--create-sandbox")
	}
	cmd := exec.CommandContext(ctx, d.binary, append(opts, []string{strconv.Itoa(childPID), tap}...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	cleanups = append(cleanups, func() error {
		logrus.Debugf("killing slirp4netns")
		cancel()
		wErr := cmd.Wait()
		logrus.Debugf("killed slirp4netns: %v", wErr)
		return nil
	})
	if err := cmd.Start(); err != nil {
		return nil, common.Seq(cleanups), errors.Wrapf(err, "executing %v", cmd)
	}
	netmsg := common.NetworkMessage{
		Dev: tap,
		MTU: d.mtu,
	}
	if d.ipnet != nil {
		// TODO: get the actual configuration via slirp4netns API?
		x, err := iputils.AddIPInt(d.ipnet.IP, 100)
		if err != nil {
			return nil, common.Seq(cleanups), err
		}
		netmsg.IP = x.String()
		netmsg.Netmask, _ = d.ipnet.Mask.Size()
		x, err = iputils.AddIPInt(d.ipnet.IP, 2)
		if err != nil {
			return nil, common.Seq(cleanups), err
		}
		netmsg.Gateway = x.String()
		x, err = iputils.AddIPInt(d.ipnet.IP, 3)
		if err != nil {
			return nil, common.Seq(cleanups), err
		}
		netmsg.DNS = x.String()
	} else {
		netmsg.IP = "10.0.2.100"
		netmsg.Netmask = 24
		netmsg.Gateway = "10.0.2.2"
		netmsg.DNS = "10.0.2.3"
	}
	return &netmsg, common.Seq(cleanups), nil
}

func NewChildDriver() network.ChildDriver {
	return &childDriver{}
}

type childDriver struct {
}

func (d *childDriver) ConfigureNetworkChild(netmsg *common.NetworkMessage) (string, error) {
	tap := netmsg.Dev
	if tap == "" {
		return "", errors.New("could not determine the preconfigured tap")
	}
	// tap is created and "up".
	// IP stuff and MTU are not configured by the parent here,
	// and they are up to the child.
	return tap, nil
}
