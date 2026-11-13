// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"go.tbhb.dev/agent-capacity/internal/session"
)

const argumentExit = 2

func accountFlags(name string) (*flag.FlagSet, *session.Options) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	options := new(session.Options)
	flags.StringVar(&options.Vendor, "vendor", "", "vendor")
	flags.StringVar(&options.Account, "account", "", "approved account alias")
	flags.StringVar(&options.AliasFile, "alias-file", "", "approved alias file")
	flags.StringVar(&options.Root, "alias-root", "", "parent of alias directories")
	flags.StringVar(&options.Binary, "binary", "", "vendor binary path")
	flags.StringVar(&options.SpoolDir, "spool-dir", "", "snapshot directory")

	return flags, options
}

func loginCommand(args []string, input *os.File, output, diagnostic io.Writer) int {
	flags, options := accountFlags("login")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		fmt.Fprintln(diagnostic, "login refused")

		return argumentExit
	}

	return session.Login(*options, input, output, diagnostic)
}

func sessionCommand(ctx context.Context, args []string, output io.Writer) int {
	if len(args) == 0 {
		return argumentExit
	}

	if args[0] == "render-settings" {
		flags, options := accountFlags("render-settings")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			return argumentExit
		}

		data, err := session.RenderSettings(options.Vendor, options.Account, options.Binary, options.SpoolDir)
		if err != nil {
			return argumentExit
		}

		_, err = output.Write(data)
		if err != nil {
			return 1
		}

		return 0
	}

	if args[0] != "run" {
		return argumentExit
	}

	flags, options := accountFlags("run")
	instance := flags.String("instance", "", "vendor and approved alias")
	flags.StringVar(&options.Prompt, "prompt", "ping", "one word scheduling prompt")

	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return argumentExit
	}

	vendor, account, ok := strings.Cut(*instance, "-")
	if !ok || options.Vendor != "" || options.Account != "" {
		return argumentExit
	}

	options.Vendor, options.Account = vendor, account
	if !options.Valid() {
		return argumentExit
	}

	return (session.Runner{Options: *options, Clock: nil}).Run(ctx)
}
