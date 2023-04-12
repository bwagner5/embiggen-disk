/*
Copyright 2018 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The embiggen-disk command live resizes a filesystem and LVM objects
// and partition tables as needed. It's useful within a VM guest to make
// its filesystem bigger when the hypervisor live resizes the underlying
// block device.
package main

// TODO: test/fix on disks with non-512 byte sectors ( /sys/block/sda/queue/hw_sector_size)

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/samber/lo"
)

var (
	dry     = flag.Bool("dry-run", false, "don't make changes")
	verbose = flag.Bool("verbose", false, "verbose output")
	daemon  = flag.Bool("daemon", false, "daemon mode")
)

func init() {
	flag.Usage = usage
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of embiggen-disk:\n\n")
	fmt.Fprintf(os.Stderr, "# embiggen-disk [flags] <mount-point-to-enlarge>\n\n")
	fmt.Fprintf(os.Stderr, "# embiggen-disk systemd - installs systemd unit file, enables, and starts service in daemon mode \n\n")
	flag.PrintDefaults()
	os.Exit(1)
}

func fatalf(format string, args ...interface{}) {
	log.SetFlags(0)
	log.Fatalf(format, args...)
}

func vlogf(format string, args ...interface{}) {
	if *verbose {
		log.Printf(format, args...)
	}
}

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}
	if runtime.GOOS != "linux" {
		fatalf("embiggen-disk only runs on Linux.")
	}

	switch flag.Arg(0) {
	case "systemd":
		unitFile := []byte(`[Unit]
Description=embiggen-disk

[Service]
ExecStart=/root/go/bin/embiggen-disk -verbose -daemon /

[Install]
WantedBy=multi-user.target`)
		os.WriteFile("/etc/systemd/system/embiggen-disk.service", unitFile, 0644)
		lo.Must0(exec.Command("systemctl", "daemon-reload").Run())
		lo.Must0(exec.Command("systemctl", "enable", "embiggen-disk.service").Run())
		lo.Must0(exec.Command("systemctl", "start", "embiggen-disk.service").Run())
		statusCmd := exec.Command("systemctl", "status", "embiggen-disk.service")
		lo.Must0(statusCmd.Run())
		output, err := statusCmd.CombinedOutput()
		if err != nil {
			log.Printf("unable to systemctl status embiggen-disk.service: %s", err)
		}
		fmt.Println(string(output))
		fmt.Println("Successfully setup embiggen-disk.service")
		os.Exit(0)
	}

	mnt := flag.Arg(0)
	ticker := time.NewTicker(10 * time.Second)
	for range ticker.C {
		e, err := getFileSystemResizer(mnt)
		vlogf("getFileSystemResizer(%q) = %#v, %v", mnt, e, err)
		if err != nil {
			fatalf("error preparing to enlarge %s: %v", mnt, err)
		}
		changes, err := Resize(e)
		if len(changes) > 0 {
			fmt.Printf("Changes made:\n")
			for _, c := range changes {
				fmt.Printf("  * %s\n", c)
			}
			restartKubeletCmd := exec.Command("systemctl", "restart", "kubelet")
			lo.Must0(restartKubeletCmd.Run())
			output, err := restartKubeletCmd.CombinedOutput()
			if err != nil {
				log.Printf("there was a problem gathering combined output from `systemctl restart kubelet`: %s", err.Error())
			} else {
				fmt.Printf("Restarted Kubelet! %s\n", string(output))
			}
		} else if err == nil {
			fmt.Printf("No changes made.\n")
		}
		if err != nil {
			fatalf("error: %v", err)
		}
	}
}

// An Resizer is anything that can enlarge something and describe its state.
// An Resizer can depend on another Resizer to run first.
type Resizer interface {
	String() string                       // "ext4 filesystem at /", "LVM PV foo"
	State() (string, error)               // "534 blocks"
	Resize() error                        // both may be non-zero
	DepResizer() (dep Resizer, err error) // can return (nil, nil) for none
}

// Resize resizes e's dependencies and then resizes e.
func Resize(e Resizer) (changes []string, err error) {
	s0, err := e.State()
	if err != nil {
		return
	}
	dep, err := e.DepResizer()
	if err != nil {
		return
	}
	if dep != nil {
		changes, err = Resize(dep)
		if err != nil {
			return
		}
	}
	err = e.Resize()
	if err != nil {
		return
	}
	s1, err := e.State()
	if err != nil {
		err = fmt.Errorf("error after successful resize of %v: %v", e, err)
		return
	}
	if s0 != s1 {
		changes = append(changes, fmt.Sprintf("%v: before: %v, after: %v", e, s0, s1))
	}
	return
}
