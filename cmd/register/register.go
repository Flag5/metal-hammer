package register

import (
	"fmt"
	"io/ioutil"
	gonet "net"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/metal-stack/metal-hammer/cmd/network"
	"github.com/metal-stack/metal-hammer/cmd/storage"
	"github.com/metal-stack/metal-hammer/metal-core/client/machine"
	"github.com/metal-stack/metal-hammer/metal-core/models"
	"github.com/metal-stack/metal-hammer/pkg/bios"
	"github.com/metal-stack/metal-hammer/pkg/ipmi"

	log "github.com/inconshreveable/log15"
	"github.com/jaypipes/ghw"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

// Register the Machine
type Register struct {
	MachineUUID string
	Client      *machine.Client
	Network     *network.Network
}

// RegisterMachine register a machine at the metal-api via metal-core
func (r *Register) RegisterMachine() (*models.DomainMetalHammerRegisterMachineRequest, string, error) {
	hw, err := r.readHardwareDetails()
	if err != nil {
		return hw, "", errors.Wrap(err, "unable to read all hardware details")
	}
	params := machine.NewRegisterParams()
	params.SetBody(hw)
	params.ID = hw.UUID
	resp, err := r.Client.Register(params)

	if err != nil {
		return hw, "", errors.Wrapf(err, "unable to register machine:%#v", hw)
	}
	if resp == nil {
		return hw, "", errors.Errorf("unable to register machine:%#v response payload is nil", hw)
	}

	log.Info("register machine returned", "response", resp.Payload)
	return hw, *resp.Payload.ID, nil
}

// this mac is used to calculate the IPMI Port offset in the metal-lab environment.
var eth0Mac = ""

func (r *Register) readHardwareDetails() (*models.DomainMetalHammerRegisterMachineRequest, error) {
	err := createSyslog()
	if err != nil {
		return nil, errors.Wrap(err, "unable to write kernel boot message to /var/log/syslog")
	}

	hw := &models.DomainMetalHammerRegisterMachineRequest{}

	memory, err := ghw.Memory()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get system memory")
	}
	hw.Memory = &memory.TotalPhysicalBytes

	// FIXME can be replaced by runtime.NumCPU()
	cpu, err := ghw.CPU()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get system cpu(s)")
	}
	cores := int32(cpu.TotalCores)
	hw.CPUCores = &cores

	nics := []*models.ModelsV1MachineNicExtended{}
	loFound := false
	links, err := netlink.LinkList()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get all links")
	}
	for _, l := range links {
		attrs := l.Attrs()
		name := attrs.Name
		mac := attrs.HardwareAddr.String()
		_, err := gonet.ParseMAC(mac)
		if err != nil {
			log.Debug("skip interface with invalid mac", "interface", name, "mac", mac)
			continue
		}
		// check if after mac validation loopback is still present
		if name == "lo" {
			loFound = true
		}
		if name == "eth0" {
			eth0Mac = mac
		}

		nic := &models.ModelsV1MachineNicExtended{
			Mac:  &mac,
			Name: &name,
		}
		log.Info("register", "nic", name, "mac", mac)
		nics = append(nics, nic)
	}
	// add a lo interface if not present
	// this is required to have this interface present
	// in our DCIM management to add a ip later.
	// FIXME this was required for netbox which is no longer used.
	if !loFound {
		mac := "00:00:00:00:00:00"
		name := "lo"
		lo := &models.ModelsV1MachineNicExtended{
			Mac:  &mac,
			Name: &name,
		}
		nics = append(nics, lo)
	}

	// now attach neighbors, this will wait up to 2*tx-intervall
	// if during this timeout not all required neighbors where found abort and reboot.
	for _, n := range nics {
		neighbors, err := r.Network.Neighbors(*n.Name)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to determine neighbors of interface:%s", *n.Name)
		}
		n.Neighbors = neighbors
	}

	hw.Nics = nics

	blockInfo, err := ghw.Block()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get system block devices")
	}
	disks := []*models.ModelsV1MachineBlockDevice{}
	for _, disk := range blockInfo.Disks {
		if strings.HasPrefix(disk.Name, storage.DiskPrefixToIgnore) {
			continue
		}
		size := int64(disk.SizeBytes)
		blockDevice := &models.ModelsV1MachineBlockDevice{
			Name: &disk.Name,
			Size: &size,
		}
		disks = append(disks, blockDevice)
	}
	hw.Disks = disks
	hw.UUID = r.MachineUUID

	ipmiconfig, err := readIPMIDetails(eth0Mac)
	if err != nil {
		return nil, err
	}
	hw.IPMI = ipmiconfig

	b := bios.Bios()
	hw.Bios = &models.ModelsV1MachineBIOS{
		Version: &b.Version,
		Vendor:  &b.Vendor,
		Date:    &b.Date,
	}

	return hw, nil
}

const defaultIpmiPort = "623"

// defaultIpmiUser the name of the user created by metal in the ipmi config
const defaultIpmiUser = "metal"

// defaultIpmiUserID the id of the user created by metal in the ipmi config
const defaultIpmiUserID = "10"

// IPMI configuration and
func readIPMIDetails(eth0Mac string) (*models.ModelsV1MachineIPMI, error) {
	config := ipmi.LanConfig{}
	var fru models.ModelsV1MachineFru
	i := ipmi.New()
	var err error
	var pw string
	var user string
	var bmcInfo ipmi.BMCInfo
	if i.DevicePresent() {
		log.Info("ipmi details from bmc")
		user = defaultIpmiUser
		// FIXME userid should be verified if available
		pw, err = i.CreateUser(user, defaultIpmiUserID, ipmi.Administrator)
		if err != nil {
			return nil, errors.Wrap(err, "ipmi create user failed")
		}
		config, err = i.GetLanConfig()
		if err != nil {
			return nil, errors.Wrap(err, "unable to read ipmi lan configuration")
		}
		log.Debug("register", "ipmi lanconfig", config)
		config.IP = config.IP + ":" + defaultIpmiPort
		f, err := i.GetFru()
		if err != nil {
			return nil, errors.Wrap(err, "unable to read ipmi fru configuration")
		}
		bmcInfo, err = i.GetBMCInfo()
		if err != nil {
			return nil, errors.Wrap(err, "unable to read ipmi bmc info configuration")
		}
		fru = models.ModelsV1MachineFru{
			ChassisPartNumber:   f.ChassisPartNumber,
			ChassisPartSerial:   f.ChassisPartSerial,
			BoardMfg:            f.BoardMfg,
			BoardMfgSerial:      f.BoardMfgSerial,
			BoardPartNumber:     f.BoardPartNumber,
			ProductManufacturer: f.ProductManufacturer,
			ProductPartNumber:   f.ProductPartNumber,
			ProductSerial:       f.ProductSerial,
		}

	} else {
		log.Info("ipmi details faked")

		if len(eth0Mac) == 0 {
			eth0Mac = "00:00:00:00:00:00"
		}

		macParts := strings.Split(eth0Mac, ":")
		lastOctet := macParts[len(macParts)-1]
		port, err := strconv.ParseUint(lastOctet, 16, 32)
		if err != nil {
			return nil, errors.Wrap(err, "unable to parse last octet of eth0 mac to a integer")
		}

		const baseIPMIPort = 6230
		// Fixed IP of vagrant environment gateway
		config.IP = fmt.Sprintf("192.168.121.1:%d", baseIPMIPort+port)
		config.Mac = "00:00:00:00:00:00"
		pw = "vagrant"
		user = "vagrant"
	}

	intf := "lanplus"
	details := &models.ModelsV1MachineIPMI{
		Address:    &config.IP,
		Mac:        &config.Mac,
		Password:   &pw,
		User:       &user,
		Interface:  &intf,
		Fru:        &fru,
		Bmcversion: &bmcInfo.FirmwareRevision,
	}

	return details, nil
}

// save the content of kernel ringbuffer to /var/log/syslog
// by calling the appropriate syscall.
// Only required if Memory is gathered by ghw.Memory()
// FIXME consider different implementation
func createSyslog() error {
	const SyslogActionReadAll = 3
	level := uintptr(SyslogActionReadAll)

	b := make([]byte, 256*1024)
	amt, _, err := syscall.Syscall(syscall.SYS_SYSLOG, level, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	if err != 0 {
		return err
	}

	return ioutil.WriteFile("/var/log/syslog", b[:amt], 0666)
}
