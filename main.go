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
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/moby/sys/mount"
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

	// mount -t proc proc -o nodev,nosuid,hidepid=2 /proc
	if err := mount.Mount("proc", "/proc", "proc", "nodev,nosuid,hidepid=2"); err != nil {
		log.Fatalf("failed to mount: %v", err)
	}
	// Once `/proc` is mounted, we can set up the shutdown handler, which writes
	// to `/proc/sysrq-trigger` to power off the system.
	defer shutdown()

	b, err := os.ReadFile("/etc/apko.json")
	if err != nil {
		log.Panicf("failed to read /etc/apko.json: %v", err)
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

	// TODO(mattmoor): Set up networking.

	// The command passed to exec.Command[Context] is resolved using this
	// process's PATH, not the PATH passed to the command execution, so set our
	// own PATH here.
	if err := os.Setenv("PATH", ic.Environment["PATH"]); err != nil {
		log.Panicf("failed to set PATH: %v", err)
	}

	// Set up the entrypoint/cmd
	cmd := exec.CommandContext(ctx, ic.Entrypoint.Command, strings.Split(ic.Cmd, " ")...)

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

	// Set the user to run as.
	uid, err := strconv.Atoi(ic.Accounts.RunAs)
	if err != nil {
		log.Panicf("failed to convert run-as user: %v", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
		},
	}

	// Run the command, and wait for it to finish.
	if err := cmd.Run(); err != nil {
		log.Panicf("failed to run command: %v", err)
	}
}

type ImageEntrypoint struct {
	// Required: The command of the entrypoint
	Command string `json:"command,omitempty"`
}

type ImageAccounts struct {
	// Required: The user to run the container as. This can be a username or UID.
	RunAs string `json:"run-as,omitempty" yaml:"run-as"`
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
