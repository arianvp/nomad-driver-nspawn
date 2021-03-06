package nspawn

import (
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	systemdDbus "github.com/coreos/go-systemd/dbus"
	"github.com/coreos/go-systemd/import1"
	"github.com/coreos/go-systemd/machine1"
	systemdUtil "github.com/coreos/go-systemd/util"
	"github.com/godbus/dbus"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/ugorji/go/codec"
)

const (
	machineMonitorIntv = 2 * time.Second
	dbusInterface      = "org.freedesktop.machine1.Manager"
	dbusPath           = "/org/freedesktop/machine1"

	TarImage string = "tar"
	RawImage string = "raw"
)

type MachineProps struct {
	Name               string
	TimestampMonotonic uint64
	Timestamp          uint64
	NetworkInterfaces  []int32
	ID                 []uint8
	Class              string
	Leader             uint32
	RootDirectory      string
	Service            string
	State              string
	Unit               string
}

type MachineAddrs struct {
	IPv4 net.IP
	//TODO: add parsing for IPv6
	// IPv6         net.IP
}

type MachineConfig struct {
	Boot                 bool               `codec:"boot"`
	Ephemeral            bool               `codec:"ephemeral"`
	ProcessTwo           bool               `codec:"process_two"`
	ReadOnly             bool               `codec:"read_only"`
	UserNamespacing      bool               `codec:"user_namespacing"`
	Command              []string           `codec:"command"`
	Console              string             `codec:"console"`
	Image                string             `codec:"image"`
	ImageDownload        *ImageDownloadOpts `codec:"image_download,omitempty"`
	imagePath            string             `codec:"-"`
	Machine              string             `codec:"machine"`
	PivotRoot            string             `codec:"pivot_root"`
	ResolvConf           string             `codec:"resolv_conf"`
	User                 string             `codec:"user"`
	Volatile             string             `codec:"volatile"`
	WorkingDirectory     string             `codec:"working_directory"`
	NetworkNamespacePath string             `codec:"network_namespace_path"`
	Bind                 MapStrStr          `codec:"bind"`
	BindReadOnly         MapStrStr          `codec:"bind_read_only"`
	Environment          MapStrStr          `codec:"environment"`
	Properties           MapStrStr          `codec:"properties"`
}

type ImageType string

type ImageProps struct {
	CreationTimestamp     uint64
	Limit                 uint64
	LimitExclusive        uint64
	ModificationTimestamp uint64
	Name                  string
	Path                  string
	ReadOnly              bool
	Type                  string
	Usage                 uint64
	UsageExclusive        uint64
}

type ImageDownloadOpts struct {
	URL    string `codec:"url"`
	Type   string `codec:"type"`
	Force  bool   `codec:"force"`
	Verify string `codec:"verify"`
}

func (c *MachineConfig) ConfigArray() ([]string, error) {
	if c.Image == "" {
		return nil, fmt.Errorf("no image configured")
	}
	// check if image exists
	imageStat, err := os.Stat(c.imagePath)
	if err != nil {
		return nil, err
	}
	imageType := "-i"
	if imageStat.IsDir() {
		imageType = "-D"
	}
	args := []string{imageType, c.imagePath}

	if c.Boot {
		args = append(args, "--boot")
	}
	if c.Ephemeral {
		args = append(args, "--ephemeral")
	}
	if c.ProcessTwo {
		args = append(args, "--as-pid2")
	}
	if c.ReadOnly {
		args = append(args, "--read-only")
	}
	if c.UserNamespacing {
		args = append(args, "-U")
	}
	if c.Console != "" {
		args = append(args, fmt.Sprintf("--console=%s", c.Console))
	}
	if c.Machine != "" {
		args = append(args, "--machine", c.Machine)
	}
	if c.PivotRoot != "" {
		args = append(args, "--pivot-root", c.PivotRoot)
	}
	if c.ResolvConf != "" {
		args = append(args, "--resolv-conf", c.ResolvConf)
	}
	if c.User != "" {
		args = append(args, "--user", c.User)
	}
	if c.Volatile != "" {
		args = append(args, fmt.Sprintf("--volatile=%s", c.Volatile))
	}
	if c.WorkingDirectory != "" {
		args = append(args, "--chdir", c.WorkingDirectory)
	}
	if c.NetworkNamespacePath != "" {
		args = append(args, "--network-namespace-path", c.NetworkNamespacePath)
	}
	for k, v := range c.Bind {
		args = append(args, "--bind", k+":"+v)
	}
	for k, v := range c.BindReadOnly {
		args = append(args, "--bind-ro", k+":"+v)
	}
	for k, v := range c.Environment {
		args = append(args, "-E", k+"="+v)
	}
	for k, v := range c.Properties {
		args = append(args, "--property="+k+"="+v)
	}
	if len(c.Command) > 0 {
		args = append(args, c.Command...)
	}
	return args, nil
}

func (c *MachineConfig) Validate() error {
	if c.Volatile != "" {
		switch c.Volatile {
		case "yes", "state", "overlay", "no":
		default:
			return fmt.Errorf("invalid parameter for volatile")
		}
	}
	if c.Console != "" {
		switch c.Console {
		case "interactive", "read-only", "passive", "pipe":
		default:
			return fmt.Errorf("invalid parameter for console")
		}
	}
	if c.ResolvConf != "" {
		switch c.ResolvConf {
		case "copy-host", "copy-static", "bind-host",
			"bind-static", "delete", "auto":
		default:
			return fmt.Errorf("invalid parameter for resolv_conf")
		}
	}
	if c.Boot && c.ProcessTwo {
		return fmt.Errorf("boot and process_two may not be combined")
	}
	if c.Volatile != "" && c.UserNamespacing {
		return fmt.Errorf("volatile and user_namespacing may not be combined")
	}
	if c.ReadOnly && c.UserNamespacing {
		return fmt.Errorf("read_only and user_namespacing may not be combined")
	}
	if c.WorkingDirectory != "" && !filepath.IsAbs(c.WorkingDirectory) {
		return fmt.Errorf("working_directory is not an absolute path")
	}
	if c.PivotRoot != "" {
		for _, p := range strings.Split(c.PivotRoot, ":") {
			if !filepath.IsAbs(p) {
				return fmt.Errorf("pivot_root is not an absolute path")
			}
		}
	}
	if c.Image == "/" && !(c.Ephemeral || c.Volatile == "yes" || c.Volatile == "state") {
		return fmt.Errorf("starting a container from the root directory is not supported. Use ephemeral or volatile")
	}

	if c.ImageDownload != nil {
		switch c.ImageDownload.Type {
		case "raw", "tar":
		default:
			return fmt.Errorf("invalid parameter for image_download.type")
		}
		switch c.ImageDownload.Verify {
		case "no", "checksum", "signature":
		default:
			return fmt.Errorf("invalid parameter for image_download.verify")
		}
	}

	return nil
}

func DescribeMachine(name string, timeout time.Duration) (*MachineProps, error) {
	c, e := machine1.New()
	if e != nil {
		return nil, e
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	done := make(chan bool)
	go func() {
		time.Sleep(timeout)
		done <- true
	}()

	var p map[string]interface{}
	for {
		select {
		case <-done:
			ticker.Stop()
			return nil, fmt.Errorf("timed out while getting machine properties: %+v", e)
		case <-ticker.C:
			p, e = c.DescribeMachine(name)
			if e == nil {
				ticker.Stop()
				return &MachineProps{
					Name:               p["Name"].(string),
					TimestampMonotonic: p["TimestampMonotonic"].(uint64),
					Timestamp:          p["Timestamp"].(uint64),
					NetworkInterfaces:  p["NetworkInterfaces"].([]int32),
					ID:                 p["Id"].([]uint8),
					Class:              p["Class"].(string),
					Leader:             p["Leader"].(uint32),
					RootDirectory:      p["RootDirectory"].(string),
					Service:            p["Service"].(string),
					State:              p["State"].(string),
					Unit:               p["Unit"].(string),
				}, nil
			}
		}
	}
}

func isInstalled() error {
	_, err := exec.LookPath("systemd-nspawn")
	if err != nil {
		return err
	}
	_, err = exec.LookPath("machinectl")
	if err != nil {
		return err
	}
	return nil
}

// systemdVersion uses dbus to check which version of systemd is installed.
func systemdVersion() (string, error) {
	// check if systemd is running
	if !systemdUtil.IsRunningSystemd() {
		return "null", fmt.Errorf("systemd is not running")
	}
	bus, err := systemdDbus.NewSystemdConnection()
	if err != nil {
		return "null", err
	}
	defer bus.Close()
	// get the systemd version
	verString, err := bus.GetManagerProperty("Version")
	if err != nil {
		return "null", err
	}
	// lose the surrounding quotes
	verNumString, err := strconv.Unquote(verString)
	if err != nil {
		return "null", err
	}
	// trim possible version suffix like in "242.19-1"
	verNum := strings.Split(verNumString, ".")[0]
	return verNum, nil
}

func setupPrivateSystemBus() (conn *dbus.Conn, err error) {
	conn, err = dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	methods := []dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}
	if err = conn.Auth(methods); err != nil {
		conn.Close()
		conn = nil
		return
	}
	if err = conn.Hello(); err != nil {
		conn.Close()
		conn = nil
	}
	return conn, nil
}

type MapStrInt map[string]int

func (s *MapStrInt) CodecEncodeSelf(enc *codec.Encoder) {
	v := []map[string]int{*s}
	enc.MustEncode(v)
}

func (s *MapStrInt) CodecDecodeSelf(dec *codec.Decoder) {
	ms := []map[string]int{}
	dec.MustDecode(&ms)

	r := map[string]int{}
	for _, m := range ms {
		for k, v := range m {
			r[k] = v
		}
	}
	*s = r
}

type MapStrStr map[string]string

func (s *MapStrStr) CodecEncodeSelf(enc *codec.Encoder) {
	v := []map[string]string{*s}
	enc.MustEncode(v)
}

func (s *MapStrStr) CodecDecodeSelf(dec *codec.Decoder) {
	ms := []map[string]string{}
	dec.MustDecode(&ms)

	r := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			r[k] = v
		}
	}
	*s = r
}

func DescribeImage(name string) (*ImageProps, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}

	img := conn.Object("org.freedesktop.machine1", "/org/freedesktop/machine1")
	var path dbus.ObjectPath

	err = img.Call("org.freedesktop.machine1.Manager.GetImage", 0, name).Store(&path)
	if err != nil {
		return nil, err
	}

	obj := conn.Object("org.freedesktop.machine1", path)
	props := make(map[string]interface{})

	err = obj.Call("org.freedesktop.DBus.Properties.GetAll", 0, "").Store(&props)
	if err != nil {
		return nil, err
	}

	return &ImageProps{
		CreationTimestamp:     props["CreationTimestamp"].(uint64),
		Limit:                 props["Limit"].(uint64),
		LimitExclusive:        props["LimitExclusive"].(uint64),
		ModificationTimestamp: props["ModificationTimestamp"].(uint64),
		Name:                  props["Name"].(string),
		Path:                  props["Path"].(string),
		ReadOnly:              props["ReadOnly"].(bool),
		Type:                  props["Type"].(string),
		Usage:                 props["Usage"].(uint64),
		UsageExclusive:        props["UsageExclusive"].(uint64),
	}, nil
}

func DownloadImage(url, name, verify, imageType string, force bool, logger hclog.Logger) error {
	c, err := import1.New()
	if err != nil {
		return err
	}

	var t *import1.Transfer
	switch imageType {
	case TarImage:
		t, err = c.PullTar(url, name, verify, force)
	case RawImage:
		t, err = c.PullRaw(url, name, verify, force)
	default:
		return fmt.Errorf("unsupported image type")
	}
	if err != nil {
		return err
	}
	logger.Info("downloading image", "image", name)

	done := false
	ticker := time.NewTicker(2 * time.Second)
	for !done {
		select {
		case <-ticker.C:
			tf, _ := c.ListTransfers()
			if len(tf) == 0 {
				done = true
				ticker.Stop()
				continue
			}
			found := false
			for _, v := range tf {
				if v.Id == t.Id {
					found = true
					if !(math.IsNaN(v.Progress) || math.IsInf(v.Progress, 0) || math.Abs(v.Progress) == math.MaxFloat64) {
						logger.Info("downloading image", "image", name, "progress", v.Progress)
					}
				}
			}
			if !found {
				done = true
				ticker.Stop()
			}
		}
	}

	logger.Info("downloaded image", "image", name)
	return nil
}

func (c *MachineConfig) GetImagePath() (string, error) {
	// check if image is absolute or relative path
	imagePath := c.Image
	if !filepath.IsAbs(c.Image) {
		pwd, e := os.Getwd()
		if e != nil {
			return "", e
		}
		imagePath = filepath.Join(pwd, c.Image)
	}
	// check if image exists
	_, err := os.Stat(imagePath)
	if err == nil {
		return imagePath, err
	}
	// check if image is known to machinectl
	p, err := DescribeImage(c.Image)
	if err != nil {
		return "", err
	}
	return p.Path, nil
}
