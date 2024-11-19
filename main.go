//go:build !darwin && !windows
// +build !darwin,!windows

// Copyright 2024 Chainguard, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/shlex"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/moby/sys/mount"
	"github.com/u-root/u-root/pkg/dhclient"
	"github.com/vishvananda/netlink"
)

// This is to mimic the following "trap"
// echo s > /proc/sysrq-trigger && echo o > /proc/sysrq-trigger && sleep infinity
func shutdown() {
	// Write 's' to /proc/sysrq-trigger
	if err := os.WriteFile("/proc/sysrq-trigger", []byte("s\n"), 0644); err != nil {
		log.Fatalf("failed to sync %v", err)
	}

	// Write 'o' to /proc/sysrq-trigger
	if err := os.WriteFile("/proc/sysrq-trigger", []byte("o\n"), 0644); err != nil {
		log.Fatalf("failed to poweroff %v", err)
	}

	// Block forever
	select {}
}

const defaultPath = "/sbin:/usr/sbin:/bin:/usr/bin:/usr/local/sbin:/usr/local/bin"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// start watching and reaping zombie processes
	go reapZombieProcesses()

	// mount -t proc proc -o nodev,nosuid,hidepid=2 /proc
	if err := mount.Mount("proc", "/proc", "proc", "nodev,nosuid,hidepid=2"); err != nil {
		log.Printf("failed to mount: %v", err)
	}
	// Once `/proc` is mounted, we can set up the shutdown handler, which writes
	// to `/proc/sysrq-trigger` to power off the system.
	defer shutdown()

	// mount -t devtmpfs -o nosuid,noexec devtmpfs /dev
	if err := mount.Mount("devtmpfs", "/dev", "devtmpfs", "nosuid,noexec"); err != nil {
		log.Printf("failed to mount: %v", err)
	}
	// mount -t sysfs -o nodev,nosuid,noexec sys /sys
	if err := os.Mkdir("/sys", 0555); err != nil {
		log.Printf("failed to create /sys: %v", err)
	} else if err := mount.Mount("sys", "/sys", "sysfs", "nodev,nosuid,noexec"); err != nil {
		log.Printf("failed to mount: %v", err)
	}
	// mount -t cgroup -o all cgroup /sys/fs/cgroup
	if err := mount.Mount("cgroup", "/sys/fs/cgroup", "cgroup", "all"); err != nil {
		log.Printf("failed to mount: %v", err)
	}
	// mount -t tmpfs -o nodev,nosuid,noexec tmpfs /tmp
	if err := mount.Mount("tmpfs", "/tmp", "tmpfs", "nodev,nosuid,noexec"); err != nil {
		log.Printf("failed to mount: %v", err)
	}

	b, err := os.ReadFile("/etc/apko.json")
	if err != nil {
		log.Printf("failed to read /etc/apko.json: %v", err)
	}
	var ic ImageConfiguration
	if err := json.Unmarshal(b, &ic); err != nil {
		log.Panicf("failed to unmarshal /etc/apko.json: %v", err)
	}

	// Ensure path is set in the environment.
	if ic.Environment == nil {
		ic.Environment = make(map[string]string, 1)
	}
	if _, ok := ic.Environment["PATH"]; !ok {
		ic.Environment["PATH"] = defaultPath
	}

	// TODO(mattmoor): Set up other important devices.

	// Set up network interfaces for loopback and veth.
	if lo, err := netlink.LinkByName("lo"); err != nil {
		log.Panicf("failed to get lo: %v", err)
	} else if err := netlink.LinkSetUp(lo); err != nil {
		log.Panicf("failed to set lo up: %v", err)
	}
	// Find the 1st veth interface supporting broadcast and multi-cast
	// that is up.
	ll, err := netlink.LinkList()
	if err != nil {
		log.Panicf("failed to list links: %v", err)
	}
	var eth0 netlink.Link
	for _, link := range ll {
		// This is to mirror this:
		// ip -o link show | grep '<BROADCAST,MULTICAST>'
		attr := link.Attrs()
		if attr.Flags&net.FlagBroadcast != net.FlagBroadcast {
			continue
		} else if attr.Flags&net.FlagMulticast != net.FlagMulticast {
			continue
		}
		eth0 = link
		break
	}
	if eth0 == nil {
		log.Panicf("no suitable interface found to listen on")
	} else if err := netlink.LinkSetUp(eth0); err != nil {
		log.Panicf("failed to set network interface %s up: %v", eth0.Attrs().Name, err)
	}

	// Configure DHCP for eth0
	// Modeled after the u-root configureAll function:
	// https://github.com/u-root/u-root/blob/0c0df672/cmds/core/dhclient/dhclient.go#L67
	{
		c := dhclient.Config{
			Timeout: 10 * time.Second,
			Retries: 3,
			V4ServerAddr: &net.UDPAddr{
				IP:   net.IPv4bcast,
				Port: dhcpv4.ServerPort,
			},
			LogLevel: dhclient.LogInfo, // There is nothing lower than info.
		}
		r := dhclient.SendRequests(ctx, []netlink.Link{eth0},
		true /* ipv4 */, false /* ipv6 */, c, 10*time.Second)
		for result := range r {
			if result.Err != nil {
				log.Printf("Could not configure %s for %s: %v", result.Interface.Attrs().Name, result.Protocol, result.Err)
				continue
			}
			if err := result.Lease.Configure(); err != nil {
				log.Printf("Could not configure %s for %s: %v", result.Interface.Attrs().Name, result.Protocol, err)
				continue
			}
			// log.Printf("Configured %s with %s", result.Interface.Attrs().Name, result.Lease)
		}
		log.Printf("Finished trying to configure all interfaces.")
	}

	// The command passed to exec.Command[Context] is resolved using this
	// process's PATH, not the PATH passed to the command execution, so set our
	// own PATH here.
	if err := os.Setenv("PATH", ic.Environment["PATH"]); err != nil {
		log.Panicf("failed to set PATH: %v", err)
	}

	// Split entrypoint and cmd and build up the args.
	args := []string{}
	if ic.Entrypoint.Command != "" {
		splitep, err := shlex.Split(ic.Entrypoint.Command)
		if err != nil {
			log.Panicf("failed to split entrypoint: %v", err)
		}
		args = append(args, splitep...)
	}
	if ic.Cmd != "" {
		splitcmd, err := shlex.Split(ic.Cmd)
		if err != nil {
			log.Panicf("failed to split command: %v", err)
		}
		args = append(args, splitcmd...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)

	// Set the working directory.
	cmd.Dir = ic.WorkDir

	// TODO(mattmoor): Does this even make sense for init?
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	// Set up the environment.
	cmd.Env = make([]string, 0, len(ic.Environment))
	for k, v := range ic.Environment {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Set the user to run as (default to 0).
	var uid, gid int
	if ic.Accounts.RunAs != "" {
		// Search for a user whose name matches the runAs and if we find one
		// then set uid to that user's UID.
		runAs := ic.Accounts.RunAs
		for _, acct := range ic.Accounts.Users {
			if acct.UserName == runAs || fmt.Sprint(acct.UID) == runAs {
				uid = int(acct.UID)
				gid = int(acct.GID)
				break
			}
		}
		// If we didn't set uid, and the runAs isn't "root", then try to parse
		// the runAs as a UID.
		if uid == 0 && runAs != "root" {
			uid, err = strconv.Atoi(ic.Accounts.RunAs)
			if err != nil {
				log.Panicf("failed to convert run-as user: %v", err)
			}
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}

	// Create a channel of type os.signal to receive the signals
	sigs := make(chan os.Signal, 1)
	defer close(sigs)

	// Use signal.Notify() to trap and relay required signals to our channel
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGABRT, syscall.SIGTERM)
	defer signal.Reset()

	// Start a routine that will receive the signals on the channel and will forward it to child process
	go func() {
		sig := <-sigs
		log.Printf("received  %v signal for PID %v\n", sig, cmd.Process.Pid)
		cmd.Process.Signal(sig)
	}()

	// Run the command, and wait for it to finish.
	err = cmd.Start()
	if err != nil {
		log.Panicf("failed to run command: %v", err)
		os.Exit(1)
	}

	// Blocking code using wait()
	cmd.Wait()
}

func reapZombieProcesses() {
	for {
		var wstatus syscall.WaitStatus

		// get pids to reap
		pid, err := syscall.Wait4(-1, &wstatus, syscall.WNOHANG, nil)
		for syscall.EINTR == err {
			pid, err = syscall.Wait4(pid, &wstatus, syscall.WNOHANG, nil)
		}

		// skip non userland pids
		if pid <= 0 {
			time.Sleep(1 * time.Second)
		} else {
			log.Printf("reaping zombie process %v\n", pid)
			continue
		}
	}
}

type ImageEntrypoint struct {
	// Required: The command of the entrypoint
	Command string `json:"command,omitempty"`
}

type ImageAccounts struct {
	// Required: The user to run the container as. This can be a username or UID.
	RunAs string `json:"run-as,omitempty" yaml:"run-as"`
	// Required: List of users to populate the image with
	Users []User `json:"users,omitempty" yaml:"users"`
}

type User struct {
	// Required: The name of the user
	UserName string `json:"username,omitempty"`
	// Required: The user ID
	UID uint32 `json:"uid,omitempty"`
	// Required: The user's group ID
	GID uint32 `json:"gid,omitempty"`
}

type ImageConfiguration struct {
	// Required: The entrypoint of the container image
	//
	// This typically is the path to the executable to run. Since many of
	// images do not include a shell, this should be the full path
	// to the executable.
	Entrypoint ImageEntrypoint `json:"entrypoint,omitempty" yaml:"entrypoint,omitempty"`

	// Optional: The command of the container image
	//
	// These are the additional arguments to pass to the entrypoint.
	Cmd string `json:"cmd,omitempty" yaml:"cmd,omitempty"`

	// Optional: The working directory of the container
	WorkDir string `json:"work-dir,omitempty" yaml:"work-dir,omitempty"`

	// Optional: Account configuration for the container image
	Accounts ImageAccounts `json:"accounts,omitempty" yaml:"accounts,omitempty"`

	// Optional: Envionment variables to set in the container image
	Environment map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`
}
