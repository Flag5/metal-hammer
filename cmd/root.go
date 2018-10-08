package cmd

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	log "github.com/inconshreveable/log15"

	"golang.org/x/sys/unix"
)

// Run orchestrates the whole register/wipe/format/burn and reboot process
func Run(spec *Specification) error {
	log.Info("metal-hammer run")
	firmware := bootedWith()
	log.Info("metal-hammer bootet with", "firmware", firmware)

	uuid, err := RegisterDevice(spec)
	if err != nil {
		log.Error("register device", "error", err)
	}

	var url string
	if spec.ImageURL != "" {
		url = spec.ImageURL
	} else {
		url, err = waitForInstall(spec.InstallURL, uuid)
		if err != nil {
			log.Error("wait for install", "error", err)
		}
	}

	err = Install(url)
	if err != nil {
		log.Error("install", "error", err)
	}

	err = reportInstallation()
	if err != nil {
		log.Error("report install", "error", err)
	}

	reboot()
	return nil
}

func bootedWith() string {
	_, err := os.Stat("/sys/firmware/efi")
	if os.IsNotExist(err) {
		return "bios"
	}
	return "efi"
}

func waitForInstall(url, uuid string) (string, error) {
	log.Info("waiting for install, long polling", "uuid", uuid)
	e := fmt.Sprintf("%v/%v", url, uuid)

	var resp *http.Response
	var err error
	for {
		resp, err = http.Get(e)
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Debug("waiting for install failed", "error", err)
		} else {
			break
		}
		log.Debug("Retrying...")
		time.Sleep(2 * time.Second)
	}

	imgURL, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response failed with: %v", err)
	}
	return string(imgURL), nil
}

func reportInstallation() error {
	log.Info("report image installation status back")
	return nil
}

func reboot() {
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		log.Error("unable to reboot", "error", err.Error())
	}
}
