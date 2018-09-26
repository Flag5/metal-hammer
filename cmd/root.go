package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"

	log "github.com/inconshreveable/log15"
)

// Run orchestrates the whole register/wipe/format/burn and reboot process
func Run(spec *Specification) error {
	log.Info("discover run")

	err := RegisterDevice(spec)
	if err != nil {
		log.Error("register device", "error", err)
	}

	err = Install("ubuntu")
	if err != nil {
		log.Error("install", "error", err)
	}

	// FIXME must be before Install
	err = waitForInstall()
	if err != nil {
		log.Error("wait for install", "error", err)
	}

	reboot()
	return nil
}

func waitForInstall() error {
	rootHandler := func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "discover\n")
	}

	http.HandleFunc("/", rootHandler)
	log.Info("waiting for a image to burn")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		return fmt.Errorf("http server not stared %v", err)
	}
	return nil
}

func reboot() {
	exec.Command("reboot")
}