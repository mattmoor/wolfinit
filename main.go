//go:build !darwin && !windows
// +build !darwin,!windows

// Copyright 2024 Chainguard, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"

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

	// TODO(mattmoor): Set up other important devices.

	// TODO(mattmoor): Set up networking.

	// The command passed to exec.Command[Context] is resolved using this
	// process's PATH, not the PATH passed to the command execution, so set our
	// own PATH here.
	if err := os.Setenv("PATH", defaultPath); err != nil {
		log.Panicf("failed to set PATH: %v", err)
	}

	// TODO(mattmoor): Set up the entrypoint/cmd
	cmd := exec.CommandContext(ctx, "grep", ".", "/proc/cmdline")

	// TODO(mattmoor): Does this even make sense for init?
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	// TODO(mattmoor): Set up the environment.
	cmd.Env = []string{
		fmt.Sprintf("PATH=%s", defaultPath),
	}

	// TODO(mattmoor): Set the user/group to run as.
	// uid, _ := strconv.Atoi("1000") // Replace with the desired user's UID
	// gid, _ := strconv.Atoi("1000") // Replace with the desired group's GID
	// cmd.SysProcAttr = &syscall.SysProcAttr{
	//     Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	// }

	// Run the command, and wait for it to finish.
	if err := cmd.Run(); err != nil {
		log.Panicf("failed to run command: %v", err)
	}
}
